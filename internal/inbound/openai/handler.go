package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/logger"
)

// Handler handles incoming OpenAI-compatible requests.
type Handler struct {
	gateway *core.Gateway
}

// NewHandler creates a new OpenAI inbound handler.
func NewHandler(gateway *core.Gateway) *Handler {
	return &Handler{
		gateway: gateway,
	}
}

// RegisterRoutes registers the OpenAI inbound routes on the provided ServeMux with the given prefix.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("POST "+prefix+"/v1/chat/completions", h.HandleChatCompletions)
	mux.HandleFunc("GET "+prefix+"/v1/models", h.HandleListModels)
}

// HandleListModels processes GET /v1/models.
func (h *Handler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	configuredModels := h.gateway.ConfiguredModels()
	now := time.Now().Unix()

	models := make([]Model, 0, len(configuredModels))
	for _, m := range configuredModels {
		models = append(models, Model{
			ID:      m,
			Object:  "model",
			Created: now,
			OwnedBy: "system", // Generic owner for all models in llmbrane
		})
	}

	res := ModelList{
		Object: "list",
		Data:   models,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// HandleChatCompletions processes POST /v1/chat/completions.
func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request")
		return
	}

	canonicalReq, err := h.translateRequest(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Failed to translate request: "+err.Error(), "invalid_request")
		return
	}

	if req.Stream {
		stream, err := h.gateway.ProcessChatStream(r.Context(), canonicalReq)
		if err != nil {
			writeGatewayError(w, err)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "Streaming unsupported by client", "internal_error")
			return
		}

		for event := range stream {
			if event.Type == "error" && event.Error != nil {
				logger.Error.Printf("[OpenAI Inbound] Stream error event encountered for model %q: %v", req.Model, event.Error)
				break
			}

			chunk := h.translateStreamEvent(event, req.Model)
			if chunk != nil && len(chunk.Choices) > 0 {
				data, _ := json.Marshal(chunk)
				w.Write([]byte("data: "))
				w.Write(data)
				w.Write([]byte("\n\n"))
				flusher.Flush()
			} else if chunk != nil {
				logger.Debug.Printf("[OpenAI Inbound] Omitted stream chunk with empty choices for model %q (event type: %s)", req.Model, event.Type)
			}
		}

		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
		return
	}

	canonicalRes, err := h.gateway.ProcessChat(r.Context(), canonicalReq)
	if err != nil {
		writeGatewayError(w, err)
		return
	}

	if len(canonicalRes.Choices) == 0 {
		logger.Warn.Printf("[OpenAI Inbound] Provider returned empty choices for model %q", req.Model)
		writeError(w, http.StatusBadGateway, "Provider returned response with no choices", "provider_error")
		return
	}

	res := h.translateResponse(canonicalRes)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (h *Handler) translateRequest(req *ChatCompletionRequest) (*canonical.CanonicalChatRequest, error) {
	var messages []canonical.CanonicalMessage
	for _, m := range req.Messages {
		var contentParts []canonical.CanonicalContentPart

		// If it has content, parse it
		if len(m.Content) > 0 && string(m.Content) != "null" {
			var strContent string
			if err := json.Unmarshal(m.Content, &strContent); err == nil {
				if m.Role == "tool" {
					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type: "tool_result",
						ToolResult: &canonical.CanonicalToolResult{
							ToolCallID: m.ToolCallID,
							Content: []canonical.CanonicalContentPart{
								{Type: "text", Text: strContent},
							},
						},
					})
				} else {
					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type: "text",
						Text: strContent,
					})
				}
			} else {
				var parts []ContentPart
				if err := json.Unmarshal(m.Content, &parts); err == nil {
					var parsedParts []canonical.CanonicalContentPart
					for _, p := range parts {
						switch p.Type {
						case "text":
							parsedParts = append(parsedParts, canonical.CanonicalContentPart{
								Type: "text",
								Text: p.Text,
							})
						case "image_url":
							if p.ImageURL != nil {
								parsedParts = append(parsedParts, canonical.CanonicalContentPart{
									Type: "image",
									Image: &canonical.CanonicalImage{
										URL:    p.ImageURL.URL,
										Detail: p.ImageURL.Detail,
									},
								})
							}
						}
					}
					if m.Role == "tool" {
						contentParts = append(contentParts, canonical.CanonicalContentPart{
							Type: "tool_result",
							ToolResult: &canonical.CanonicalToolResult{
								ToolCallID: m.ToolCallID,
								Content:    parsedParts,
							},
						})
					} else {
						contentParts = append(contentParts, parsedParts...)
					}
				} else {
					writeErrorStr := fmt.Sprintf("Failed to parse message content: %v", err)
					return nil, &canonical.GatewayError{Message: writeErrorStr, ErrorKind: canonical.ErrorKindInvalidRequest}
				}
			}
		}

		// Add ToolCalls if present
		for _, tc := range m.ToolCalls {
			contentParts = append(contentParts, canonical.CanonicalContentPart{
				Type: "tool_call",
				ToolCall: &canonical.CanonicalToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: json.RawMessage(tc.Function.Arguments),
				},
			})
		}

		messages = append(messages, canonical.CanonicalMessage{
			Role:       canonical.CanonicalRole(m.Role),
			Name:       m.Name,
			Content:    contentParts,
			ToolCallID: m.ToolCallID,
		})
	}

	var canonicalTools []canonical.CanonicalTool
	for _, t := range req.Tools {
		if t.Type == "function" {
			var inputSchema map[string]any
			if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
				json.Unmarshal(t.Function.Parameters, &inputSchema)
			}
			canonicalTools = append(canonicalTools, canonical.CanonicalTool{
				Type:        "function",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: inputSchema,
			})
		}
	}

	var canonicalToolChoice *canonical.CanonicalToolChoice
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var tcString string
		if err := json.Unmarshal(req.ToolChoice, &tcString); err == nil {
			if tcString != "none" && tcString != "auto" {
				canonicalToolChoice = &canonical.CanonicalToolChoice{Type: tcString}
			} else {
				canonicalToolChoice = &canonical.CanonicalToolChoice{Type: "auto"} // Or map appropriately
			}
		} else {
			var tcObj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &tcObj); err == nil {
				canonicalToolChoice = &canonical.CanonicalToolChoice{
					Type: tcObj.Type,
					Name: tcObj.Function.Name,
				}
			}
		}
	}

	var stop []string
	if len(req.Stop) > 0 {
		var singleStop string
		if err := json.Unmarshal(req.Stop, &singleStop); err == nil {
			stop = []string{singleStop}
		} else {
			var multiStop []string
			if err := json.Unmarshal(req.Stop, &multiStop); err == nil {
				stop = multiStop
			}
		}
	}

	var maxTokens = req.MaxTokens
	if req.MaxCompletionTokens != nil {
		maxTokens = req.MaxCompletionTokens
	}

	return &canonical.CanonicalChatRequest{
		Model:      req.Model,
		Messages:   messages,
		Tools:      canonicalTools,
		ToolChoice: canonicalToolChoice,
		Stream:     req.Stream,
		Params: canonical.CanonicalParams{
			Temperature:      req.Temperature,
			TopP:             req.TopP,
			MaxOutputTokens:  maxTokens,
			Stop:             stop,
			Seed:             req.Seed,
			PresencePenalty:  req.PresencePenalty,
			FrequencyPenalty: req.FrequencyPenalty,
			Logprobs:         req.Logprobs,
			TopLogprobs:      req.TopLogprobs,
			ReasoningEffort:  req.ReasoningEffort,
		},
	}, nil
}

func (h *Handler) translateResponse(res *canonical.CanonicalChatResponse) *ChatCompletionResponse {
	choices := make([]Choice, 0, len(res.Choices))
	for _, c := range res.Choices {
		var contentStr *string
		var reasoningStr *string
		var toolCalls []ToolCall

		for _, p := range c.Message.Content {
			if p.Type == "text" {
				// We assume only one text part or concatenate them
				if contentStr == nil {
					s := p.Text
					contentStr = &s
				} else {
					*contentStr += p.Text
				}
			} else if p.Type == "reasoning" && p.Reasoning != nil {
				if reasoningStr == nil {
					s := p.Reasoning.Content
					reasoningStr = &s
				} else {
					*reasoningStr += p.Reasoning.Content
				}
			} else if p.Type == "tool_call" && p.ToolCall != nil {
				args := string(p.ToolCall.Arguments)
				if args == "" {
					args = "{}"
				}
				toolCalls = append(toolCalls, ToolCall{
					ID:   p.ToolCall.ID,
					Type: "function",
					Function: FunctionCall{
						Name:      p.ToolCall.Name,
						Arguments: args,
					},
				})
			}
		}

		var contentBytes json.RawMessage
		if contentStr != nil {
			contentBytes, _ = json.Marshal(*contentStr)
		} else if len(toolCalls) == 0 {
			contentBytes = []byte("null")
		}

		choices = append(choices, Choice{
			Index: c.Index,
			Message: &Message{
				Role:             string(c.Message.Role),
				Content:          contentBytes,
				ReasoningContent: reasoningStr,
				ToolCalls:        toolCalls,
			},
			FinishReason: c.FinishReason,
		})
	}

	var usage *Usage
	if res.Usage != nil {
		usage = &Usage{
			PromptTokens:     res.Usage.InputTokens,
			CompletionTokens: res.Usage.OutputTokens,
			TotalTokens:      res.Usage.TotalTokens,
		}
	}

	return &ChatCompletionResponse{
		ID:      res.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   res.Model,
		Choices: choices,
		Usage:   usage,
	}
}

func (h *Handler) translateStreamEvent(event *canonical.CanonicalChatStreamEvent, model string) *ChatCompletionStreamResponse {
	chunk := &ChatCompletionStreamResponse{
		ID:      "chatcmpl-stream", // in a real app, generate a unique ID
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []StreamChoice{},
	}

	var finishReason *string
	if event.Stop != nil {
		fr := event.Stop.Reason
		finishReason = &fr
	}

	choice := StreamChoice{
		Index:        event.MessageIndex,
		FinishReason: finishReason,
	}

	hasDelta := false

	if event.Type == "message_start" {
		choice.Delta.Role = "assistant"
		hasDelta = true
	}

	if event.Delta != nil {
		if event.Delta.Type == "reasoning" && event.Delta.Reasoning != nil && event.Delta.Reasoning.Content != "" {
			reasoning := event.Delta.Reasoning.Content
			choice.Delta.ReasoningContent = &reasoning
			hasDelta = true
		} else if event.Delta.Text != "" {
			text := event.Delta.Text
			choice.Delta.Content = &text
			hasDelta = true
		}
	}

	if event.ToolCall != nil {
		tcDelta := ToolCallDelta{
			Index: event.ToolCall.Index,
		}
		if event.ToolCall.ID != "" {
			tcDelta.ID = event.ToolCall.ID
			tcDelta.Type = "function"
		}
		if event.ToolCall.Name != "" || event.ToolCall.ArgumentsDelta != "" {
			tcDelta.Function = &FunctionCallDelta{
				Name:      event.ToolCall.Name,
				Arguments: event.ToolCall.ArgumentsDelta,
			}
		}
		choice.Delta.ToolCalls = []ToolCallDelta{tcDelta}
		hasDelta = true
	}

	// Only append choice if it has actual delta content or a finish reason
	if hasDelta || finishReason != nil {
		chunk.Choices = append(chunk.Choices, choice)
	}

	if event.Usage != nil {
		chunk.Usage = &Usage{
			PromptTokens:     event.Usage.InputTokens,
			CompletionTokens: event.Usage.OutputTokens,
			TotalTokens:      event.Usage.TotalTokens,
		}
	}

	return chunk
}

func writeError(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(canonical.OpenAIErrorResponse{
		Error: canonical.OpenAIErrorDetail{
			Message: message,
			Type:    errType,
		},
	})
}

func writeGatewayError(w http.ResponseWriter, err error) {
	if gwErr, ok := err.(*canonical.GatewayError); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(gwErr.StatusCode)
		json.NewEncoder(w).Encode(gwErr.ToOpenAIFormat())
		return
	}
	// Fallback for non-GatewayError
	writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
}
