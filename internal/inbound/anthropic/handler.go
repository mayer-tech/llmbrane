package anthropic

import (
	"encoding/json"
	"net/http"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/core"
)

// Handler handles incoming Anthropic-compatible requests.
type Handler struct {
	gateway *core.Gateway
}

// NewHandler creates a new Anthropic inbound handler.
func NewHandler(gateway *core.Gateway) *Handler {
	return &Handler{
		gateway: gateway,
	}
}

// RegisterRoutes registers the Anthropic inbound routes on the provided ServeMux with the given prefix.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("POST "+prefix+"/v1/messages", h.HandleMessages)
	mux.HandleFunc("GET "+prefix+"/v1/models", h.HandleListModels)
}

// HandleListModels processes GET /v1/models.
func (h *Handler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}

	configuredModels := h.gateway.ConfiguredModels()

	models := make([]Model, 0, len(configuredModels))
	for _, m := range configuredModels {
		models = append(models, Model{
			Type:        "model",
			ID:          m,
			DisplayName: m,
			CreatedAt:   "2024-01-01T00:00:00Z", // Mocked, anthropic models API requires created_at string
		})
	}

	firstID := ""
	lastID := ""
	if len(models) > 0 {
		firstID = models[0].ID
		lastID = models[len(models)-1].ID
	}

	res := ModelList{
		Data:    models,
		HasMore: false,
		FirstID: firstID,
		LastID:  lastID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// HandleMessages processes POST /v1/messages.
func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}

	var req MessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error")
		return
	}

	canonicalReq, err := h.translateRequest(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Failed to translate request: "+err.Error(), "invalid_request_error")
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
			writeError(w, http.StatusInternalServerError, "Streaming unsupported by client", "api_error")
			return
		}

		for event := range stream {
			if event.Type == "error" && event.Error != nil {
				errEvent := StreamEvent{
					Type: "error",
					Error: &ErrorDetail{
						Type:    "api_error",
						Message: event.Error.Message,
					},
				}
				data, _ := json.Marshal(errEvent)
				w.Write([]byte("event: error\n"))
				w.Write([]byte("data: "))
				w.Write(data)
				w.Write([]byte("\n\n"))
				flusher.Flush()
				break
			}

			chunks := h.translateStreamEvent(event, req.Model)
			for _, chunk := range chunks {
				data, _ := json.Marshal(chunk)
				w.Write([]byte("event: " + chunk.Type + "\n"))
				w.Write([]byte("data: "))
				w.Write(data)
				w.Write([]byte("\n\n"))
				flusher.Flush()
			}
		}
		return
	}

	canonicalRes, err := h.gateway.ProcessChat(r.Context(), canonicalReq)
	if err != nil {
		writeGatewayError(w, err)
		return
	}

	res := h.translateResponse(canonicalRes)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (h *Handler) translateRequest(req *MessagesRequest) (*canonical.CanonicalChatRequest, error) {
	var messages []canonical.CanonicalMessage
	for _, m := range req.Messages {
		var contentParts []canonical.CanonicalContentPart

		switch c := m.Content.(type) {
		case string:
			contentParts = append(contentParts, canonical.CanonicalContentPart{
				Type: "text",
				Text: c,
			})
		case []any:
			for _, item := range c {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				partType, _ := itemMap["type"].(string)
				if partType == "text" {
					text, _ := itemMap["text"].(string)
					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type: "text",
						Text: text,
					})
				} else if partType == "tool_use" {
					id, _ := itemMap["id"].(string)
					name, _ := itemMap["name"].(string)
					input, _ := itemMap["input"].(map[string]any)
					inputBytes, _ := json.Marshal(input)
					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type: "tool_call",
						ToolCall: &canonical.CanonicalToolCall{
							ID:        id,
							Name:      name,
							Arguments: inputBytes,
						},
					})
				} else if partType == "tool_result" {
					toolUseID, _ := itemMap["tool_use_id"].(string)
					isError, _ := itemMap["is_error"].(bool)

					var innerContent []canonical.CanonicalContentPart
					if contentVal, ok := itemMap["content"]; ok {
						if contentStr, ok := contentVal.(string); ok {
							innerContent = append(innerContent, canonical.CanonicalContentPart{
								Type: "text",
								Text: contentStr,
							})
						} else if contentArr, ok := contentVal.([]any); ok {
							for _, innerItem := range contentArr {
								innerMap, ok := innerItem.(map[string]any)
								if !ok {
									continue
								}
								innerType, _ := innerMap["type"].(string)
								if innerType == "text" {
									text, _ := innerMap["text"].(string)
									innerContent = append(innerContent, canonical.CanonicalContentPart{
										Type: "text",
										Text: text,
									})
								}
							}
						}
					}

					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type: "tool_result",
						ToolResult: &canonical.CanonicalToolResult{
							ToolCallID: toolUseID,
							Content:    innerContent,
							IsError:    isError,
						},
					})
				}
			}
		}

		messages = append(messages, canonical.CanonicalMessage{
			Role:    canonical.CanonicalRole(m.Role),
			Content: contentParts,
		})
	}

	var systemParts []canonical.CanonicalContentPart
	if req.System != nil {
		switch s := req.System.(type) {
		case string:
			systemParts = append(systemParts, canonical.CanonicalContentPart{
				Type: "text",
				Text: s,
			})
		case []any:
			for _, item := range s {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				partType, _ := itemMap["type"].(string)
				if partType == "text" {
					text, _ := itemMap["text"].(string)
					systemParts = append(systemParts, canonical.CanonicalContentPart{
						Type: "text",
						Text: text,
					})
				}
			}
		}
	}

	var tools []canonical.CanonicalTool
	for _, t := range req.Tools {
		var inputSchema map[string]any
		json.Unmarshal(t.InputSchema, &inputSchema)
		tools = append(tools, canonical.CanonicalTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			InputSchema: inputSchema,
		})
	}

	var toolChoice *canonical.CanonicalToolChoice
	if req.ToolChoice != nil {
		toolChoice = &canonical.CanonicalToolChoice{
			Type: req.ToolChoice.Type,
			Name: req.ToolChoice.Name,
		}
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096 // default fallback if needed
	}

	return &canonical.CanonicalChatRequest{
		Model:      req.Model,
		Messages:   messages,
		System:     systemParts,
		Tools:      tools,
		ToolChoice: toolChoice,
		Stream:     req.Stream,
		Params: canonical.CanonicalParams{
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			MaxOutputTokens: &maxTokens,
			Stop:            req.StopSequences,
		},
	}, nil
}

func (h *Handler) translateResponse(res *canonical.CanonicalChatResponse) *MessagesResponse {
	var content []ContentBlock
	if len(res.Choices) > 0 {
		msg := res.Choices[0].Message
		for _, p := range msg.Content {
			if p.Type == "text" {
				content = append(content, ContentBlock{
					Type: "text",
					Text: p.Text,
				})
			} else if p.Type == "tool_call" && p.ToolCall != nil {
				var input map[string]any
				json.Unmarshal(p.ToolCall.Arguments, &input)

				var inputRaw json.RawMessage = p.ToolCall.Arguments
				if len(inputRaw) == 0 {
					inputRaw = []byte(`{}`)
				}

				content = append(content, ContentBlock{
					Type:  "tool_use",
					ID:    p.ToolCall.ID,
					Name:  p.ToolCall.Name,
					Input: inputRaw,
				})
			}
		}
	}

	var stopReason *string
	var stopSequence *string
	if res.Stop != nil {
		reason := res.Stop.Reason
		if reason == "stop" {
			reason = "end_turn"
		} else if reason == "tool_calls" {
			reason = "tool_use"
		} else if reason == "length" {
			reason = "max_tokens"
		}
		stopReason = &reason
	}

	var usage *Usage
	if res.Usage != nil {
		usage = &Usage{
			InputTokens:  res.Usage.InputTokens,
			OutputTokens: res.Usage.OutputTokens,
		}
	}

	return &MessagesResponse{
		ID:           res.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        res.Model,
		Content:      content,
		StopReason:   stopReason,
		StopSequence: stopSequence,
		Usage:        usage,
	}
}

func (h *Handler) translateStreamEvent(event *canonical.CanonicalChatStreamEvent, model string) []*StreamEvent {
	var events []*StreamEvent

	switch event.Type {
	case "message_start":
		events = append(events, &StreamEvent{
			Type: "message_start",
			Message: &MessagesResponse{
				ID:      "msg_stream",
				Type:    "message",
				Role:    "assistant",
				Model:   model,
				Content: []ContentBlock{},
			},
		})
	case "tool_call_start":
		idx := event.ContentIndex
		if event.ToolCall != nil {
			events = append(events, &StreamEvent{
				Type:  "content_block_start",
				Index: &idx,
				ContentBlock: &ContentBlock{
					Type:  "tool_use",
					ID:    event.ToolCall.ID,
					Name:  event.ToolCall.Name,
					Input: json.RawMessage(`{}`),
				},
			})
		}
	case "content_delta":
		idx := event.ContentIndex
		if event.Delta != nil && event.Delta.Text != "" {
			// Anthropic implies a content_block_start before content_delta.
			// The outbound adapter might synthesize it or we just send delta.
			// For robustness, we assume content_block_start for text is synthesized or implicit.
			events = append(events, &StreamEvent{
				Type:  "content_block_delta",
				Index: &idx,
				Delta: &Delta{
					Type: "text_delta",
					Text: event.Delta.Text,
				},
			})
		}
	case "tool_call_delta":
		idx := event.ContentIndex
		if event.ToolCall != nil && event.ToolCall.ArgumentsDelta != "" {
			events = append(events, &StreamEvent{
				Type:  "content_block_delta",
				Index: &idx,
				Delta: &Delta{
					Type:        "input_json_delta",
					PartialJson: event.ToolCall.ArgumentsDelta,
				},
			})
		}
	case "usage":
		if event.Usage != nil {
			events = append(events, &StreamEvent{
				Type:  "message_delta",
				Delta: &Delta{
					// empty delta
				},
				Usage: &Usage{
					OutputTokens: event.Usage.OutputTokens,
				},
			})
		}
	case "done":
		var stopReason *string
		if event.Stop != nil {
			r := event.Stop.Reason
			if r == "stop" {
				r = "end_turn"
			} else if r == "tool_calls" {
				r = "tool_use"
			} else if r == "length" {
				r = "max_tokens"
			}
			stopReason = &r
		} else {
			r := "end_turn"
			stopReason = &r
		}

		events = append(events, &StreamEvent{
			Type: "message_delta",
			Delta: &Delta{
				StopReason: stopReason,
			},
		})

		events = append(events, &StreamEvent{
			Type: "message_stop",
		})
	}

	return events
}

func writeError(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorResponse{
		Type: "error",
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
		},
	})
}

func writeGatewayError(w http.ResponseWriter, err error) {
	if gwErr, ok := err.(*canonical.GatewayError); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(gwErr.StatusCode)

		errType := "api_error"
		if gwErr.StatusCode == http.StatusUnauthorized {
			errType = "authentication_error"
		} else if gwErr.StatusCode == http.StatusBadRequest {
			errType = "invalid_request_error"
		} else if gwErr.StatusCode == http.StatusTooManyRequests {
			errType = "rate_limit_error"
		}

		json.NewEncoder(w).Encode(ErrorResponse{
			Type: "error",
			Error: ErrorDetail{
				Type:    errType,
				Message: gwErr.Message,
			},
		})
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error(), "api_error")
}
