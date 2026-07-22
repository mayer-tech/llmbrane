package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

type Adapter struct {
	client *http.Client
}

func NewAdapter() *Adapter {
	return &Adapter{
		client: &http.Client{},
	}
}

func (a *Adapter) translateCanonicalRequest(req *canonical.CanonicalChatRequest, model string, stream bool, endpoint config.Endpoint) ChatCompletionRequest {
	var reasoningEffort *string
	if req.Params.ReasoningEffort != nil {
		eff := *req.Params.ReasoningEffort
		if mapping, ok := endpoint.Extensions["reasoning_effort_mapping"].(map[string]any); ok {
			if mapped, ok := mapping[eff].(string); ok {
				eff = mapped
			}
		} else if mapping, ok := endpoint.Extensions["reasoning_effort_mapping"].(map[string]string); ok {
			if mapped, ok := mapping[eff]; ok {
				eff = mapped
			}
		}
		reasoningEffort = &eff
	}

	outReq := ChatCompletionRequest{
		Model:            model,
		Temperature:      req.Params.Temperature,
		TopP:             req.Params.TopP,
		MaxTokens:        req.Params.MaxOutputTokens,
		Stop:             req.Params.Stop,
		PresencePenalty:  req.Params.PresencePenalty,
		FrequencyPenalty: req.Params.FrequencyPenalty,
		Logprobs:         req.Params.Logprobs,
		TopLogprobs:      req.Params.TopLogprobs,
		Seed:             req.Params.Seed,
		ReasoningEffort:  reasoningEffort,
		Stream:           stream,
	}

	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			if t.Type == "function" {
				outReq.Tools = append(outReq.Tools, Tool{
					Type: "function",
					Function: Function{
						Name:        t.Name,
						Description: t.Description,
						Parameters:  t.InputSchema,
					},
				})
			}
		}
	}

	if req.ToolChoice != nil {
		if req.ToolChoice.Type == "none" || req.ToolChoice.Type == "auto" {
			outReq.ToolChoice = req.ToolChoice.Type
		} else if req.ToolChoice.Type == "function" && req.ToolChoice.Name != "" {
			outReq.ToolChoice = map[string]any{
				"type": "function",
				"function": map[string]string{
					"name": req.ToolChoice.Name,
				},
			}
		}
	}

	for _, msg := range req.Messages {
		var contentStr string
		var toolCalls []ToolCall
		var toolCallID string

		for _, part := range msg.Content {
			if part.Type == "text" {
				contentStr += part.Text
			} else if part.Type == "tool_call" && part.ToolCall != nil {
				tc := ToolCall{
					ID:   part.ToolCall.ID,
					Type: "function",
					Function: FunctionCall{
						Name:      part.ToolCall.Name,
						Arguments: string(part.ToolCall.Arguments),
					},
				}
				if part.ToolCall.Metadata != nil {
					if extra, ok := part.ToolCall.Metadata["extra_content"]; ok {
						tc.ExtraContent = extra
					}
				}
				// Option 1 & 2 combination: Inject bypass if no extra_content is provided but extension is enabled
				if tc.ExtraContent == nil {
					if injectBypass, ok := endpoint.Extensions["inject_thought_signature_bypass"].(bool); ok && injectBypass {
						tc.ExtraContent = map[string]any{
							"google": map[string]any{
								"thought_signature": "skip_thought_signature_validator",
							},
						}
					}
				}
				toolCalls = append(toolCalls, tc)
			} else if part.Type == "tool_result" && part.ToolResult != nil {
				toolCallID = part.ToolResult.ToolCallID
				for _, resPart := range part.ToolResult.Content {
					if resPart.Type == "text" {
						contentStr += resPart.Text
					}
				}
			}
		}

		outMsg := Message{
			Role: string(msg.Role),
			Name: msg.Name,
		}

		if len(toolCalls) > 0 {
			outMsg.ToolCalls = toolCalls
		}
		if toolCallID != "" {
			outMsg.ToolCallID = toolCallID
		}

		if contentStr != "" {
			outMsg.Content = contentStr
		} else if len(toolCalls) == 0 {
			// If no text and no tool calls, OpenAI still might require content
			// But let's only set it to null or empty string. Since Content is any, we can do:
			// actually for role=assistant with tool_calls, content can be omitted or nil
			if msg.Role == canonical.RoleAssistant {
				// omit content
			} else {
				outMsg.Content = ""
			}
		}

		outReq.Messages = append(outReq.Messages, outMsg)
	}

	if len(req.System) > 0 {
		var sysStr string
		for _, part := range req.System {
			if part.Type == "text" {
				sysStr += part.Text
			}
		}
		sysMsg := Message{
			Role:    "system",
			Content: sysStr,
		}
		outReq.Messages = append([]Message{sysMsg}, outReq.Messages...)
	}

	return outReq
}

func (a *Adapter) ProcessChat(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	// 1. Translate
	outReq := a.translateCanonicalRequest(req, model, false, endpoint)

	// 2. Serialize
	reqBody, err := json.Marshal(outReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal outbound request: %w", err)
	}

	// 3. Construct destination URL
	url := strings.ReplaceAll(endpoint.URL, "{target_model}", model)

	// 4. Create HTTP Request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// 5. Apply configured headers
	for k, v := range endpoint.Headers {
		httpReq.Header.Set(k, v)
	}

	// 6. Execute HTTP request
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, canonical.NewGatewayError("openai", model, err.Error(), http.StatusBadGateway, nil)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// 7. Map HTTP/API errors to GatewayError
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp ErrorResponse
		errMsg := "unknown provider error"
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		} else {
			errMsg = string(respBody)
		}
		return nil, canonical.NewGatewayError("openai", model, errMsg, resp.StatusCode, respBody)
	}

	// 8. Parse provider's response into CanonicalChatResponse
	var outResp ChatCompletionResponse
	if err := json.Unmarshal(respBody, &outResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	canonResp := &canonical.CanonicalChatResponse{
		ID:      outResp.ID,
		Model:   outResp.Model,
		Raw:     respBody,
		Choices: make([]canonical.CanonicalChoice, len(outResp.Choices)),
	}

	for i, choice := range outResp.Choices {
		canonMsg := canonical.CanonicalMessage{
			Role: canonical.CanonicalRole(choice.Message.Role),
		}

		// Handle Text Content
		if choice.Message.Content != nil {
			var text string
			if str, ok := choice.Message.Content.(string); ok && str != "" {
				text = str
			}
			if text != "" {
				canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{
					Type: "text",
					Text: text,
				})
			}
		}

		// Handle ToolCalls
		for _, tc := range choice.Message.ToolCalls {
			toolCall := &canonical.CanonicalToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(tc.Function.Arguments),
			}
			if tc.ExtraContent != nil {
				toolCall.Metadata = map[string]any{
					"extra_content": tc.ExtraContent,
				}
			}
			canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{
				Type:     "tool_call",
				ToolCall: toolCall,
			})
		}

		canonResp.Choices[i] = canonical.CanonicalChoice{
			Index:        choice.Index,
			Message:      canonMsg,
			FinishReason: choice.FinishReason,
		}
	}

	if outResp.Usage != nil {
		canonResp.Usage = &canonical.CanonicalUsage{
			InputTokens:  outResp.Usage.PromptTokens,
			OutputTokens: outResp.Usage.CompletionTokens,
			TotalTokens:  outResp.Usage.TotalTokens,
		}
	}

	return canonResp, nil
}

func (a *Adapter) ProcessChatStream(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	// 1. Translate
	outReq := a.translateCanonicalRequest(req, model, true, endpoint)

	// 2. Serialize
	reqBody, err := json.Marshal(outReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal outbound request: %w", err)
	}

	url := strings.ReplaceAll(endpoint.URL, "{target_model}", model)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range endpoint.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, canonical.NewGatewayError("openai", model, err.Error(), http.StatusBadGateway, nil)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		var errResp ErrorResponse
		errMsg := "unknown provider error"
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		} else {
			errMsg = string(respBody)
		}
		return nil, canonical.NewGatewayError("openai", model, errMsg, resp.StatusCode, respBody)
	}

	ch := make(chan *canonical.CanonicalChatStreamEvent)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return
			}

			var chunk ChatCompletionStreamResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:  "error",
					Error: canonical.NewGatewayError("openai", model, "failed to decode stream chunk: "+err.Error(), http.StatusInternalServerError, nil),
				}
				return
			}

			for _, choice := range chunk.Choices {
				if choice.Delta.Role != "" {
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "message_start",
						MessageIndex: choice.Index,
					}
				}

				if choice.Delta.Content != nil {
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "content_delta",
						MessageIndex: choice.Index,
						Delta: &canonical.CanonicalContentPart{
							Type: "text",
							Text: *choice.Delta.Content,
						},
					}
				}

				if len(choice.Delta.ToolCalls) > 0 {
					for _, tc := range choice.Delta.ToolCalls {
						tcEvent := &canonical.CanonicalChatStreamEvent{
							Type:         "tool_call_delta",
							MessageIndex: choice.Index,
							ToolCall: &canonical.CanonicalToolCallDelta{
								Index: tc.Index,
								ID:    tc.ID,
							},
						}
						if tc.Function != nil {
							tcEvent.ToolCall.Name = tc.Function.Name
							tcEvent.ToolCall.ArgumentsDelta = tc.Function.Arguments
						}
						ch <- tcEvent
					}
				}

				if choice.FinishReason != nil {
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "done",
						MessageIndex: choice.Index,
						Stop:         &canonical.CanonicalStop{Reason: *choice.FinishReason},
					}
				}

				// If it's a usage chunk (sometimes usage comes in an empty choice or at the end)
				if chunk.Usage != nil {
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "usage",
						MessageIndex: choice.Index,
						Usage: &canonical.CanonicalUsage{
							InputTokens:  chunk.Usage.PromptTokens,
							OutputTokens: chunk.Usage.CompletionTokens,
							TotalTokens:  chunk.Usage.TotalTokens,
						},
					}
					// Only send usage once per chunk
					chunk.Usage = nil
				}
			}

			// If usage comes outside choices in some variants
			if len(chunk.Choices) == 0 && chunk.Usage != nil {
				ch <- &canonical.CanonicalChatStreamEvent{
					Type: "done",
					Usage: &canonical.CanonicalUsage{
						InputTokens:  chunk.Usage.PromptTokens,
						OutputTokens: chunk.Usage.CompletionTokens,
						TotalTokens:  chunk.Usage.TotalTokens,
					},
				}
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- &canonical.CanonicalChatStreamEvent{
				Type:  "error",
				Error: canonical.NewGatewayError("openai", model, "stream read error: "+err.Error(), http.StatusInternalServerError, nil),
			}
		}
	}()

	return ch, nil
}
