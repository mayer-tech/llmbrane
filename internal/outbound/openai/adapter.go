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
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/logger"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

type Adapter struct {
	client      *http.Client
	idleTimeout time.Duration
	retryPolicy httpretry.Policy
}

func NewAdapter() *Adapter {
	return NewAdapterWithTimeout(60 * time.Second)
}

func NewAdapterWithTimeout(idleTimeout time.Duration) *Adapter {
	return NewAdapterWithTransport(idleTimeout, nil)
}

// NewAdapterWithTransport creates an adapter with a custom HTTP transport and
// the default retry policy. If transport is nil, http.DefaultTransport is used.
func NewAdapterWithTransport(idleTimeout time.Duration, transport http.RoundTripper) *Adapter {
	return NewAdapterWithPolicy(idleTimeout, transport, httpretry.DefaultPolicy)
}

// NewAdapterWithPolicy creates an adapter with a custom HTTP transport and an
// explicit retry policy for provider-side HTTP 429 responses.
func NewAdapterWithPolicy(idleTimeout time.Duration, transport http.RoundTripper, retryPolicy httpretry.Policy) *Adapter {
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}
	return &Adapter{
		client:      &http.Client{Transport: transport},
		idleTimeout: idleTimeout,
		retryPolicy: retryPolicy,
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
		var outboundParts []any
		var hasImage bool
		var toolCalls []ToolCall
		var toolCallID string

		for _, part := range msg.Content {
			if part.Type == "text" {
				contentStr += part.Text
				if part.Text != "" {
					outboundParts = append(outboundParts, map[string]any{
						"type": "text",
						"text": part.Text,
					})
				}
			} else if part.Type == "image" && part.Image != nil {
				hasImage = true
				imgMap := map[string]any{
					"url": part.Image.URL,
				}
				if part.Image.Detail != "" {
					imgMap["detail"] = part.Image.Detail
				}
				outboundParts = append(outboundParts, map[string]any{
					"type":      "image_url",
					"image_url": imgMap,
				})
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
						if resPart.Text != "" {
							outboundParts = append(outboundParts, map[string]any{
								"type": "text",
								"text": resPart.Text,
							})
						}
					} else if resPart.Type == "image" && resPart.Image != nil {
						hasImage = true
						imgMap := map[string]any{
							"url": resPart.Image.URL,
						}
						if resPart.Image.Detail != "" {
							imgMap["detail"] = resPart.Image.Detail
						}
						outboundParts = append(outboundParts, map[string]any{
							"type":      "image_url",
							"image_url": imgMap,
						})
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

		if hasImage {
			outMsg.Content = outboundParts
		} else if contentStr != "" {
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
	resp, err := a.retryPolicy.Do(ctx, a.client, httpReq)
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
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("openai", model, errMsg, resp.StatusCode, respBody), resp.Header)
	}

	// 8. Parse provider's response into CanonicalChatResponse
	var outResp ChatCompletionResponse
	if err := json.Unmarshal(respBody, &outResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if len(outResp.Choices) == 0 {
		return nil, canonical.NewGatewayError("openai", model, "provider returned response with no choices", http.StatusBadGateway, respBody)
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

		// Handle Reasoning Content
		var reasoningText string
		if choice.Message.ReasoningContent != nil && *choice.Message.ReasoningContent != "" {
			reasoningText = *choice.Message.ReasoningContent
		} else if choice.Message.Reasoning != nil && *choice.Message.Reasoning != "" {
			reasoningText = *choice.Message.Reasoning
		}
		if reasoningText != "" {
			canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{
				Type: "reasoning",
				Reasoning: &canonical.CanonicalReasoning{
					Content: reasoningText,
				},
			})
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

	resp, err := a.retryPolicy.Do(ctx, a.client, httpReq)
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
		logger.Warn.Printf("[OpenAI Outbound] HTTP %d from provider for model %q: %s", resp.StatusCode, model, errMsg)
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("openai", model, errMsg, resp.StatusCode, respBody), resp.Header)
	}

	ch := make(chan *canonical.CanonicalChatStreamEvent)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		// Some providers (e.g. OpenRouter/Novita) repeat the assistant role in
		// every stream chunk. Emit message_start only once per choice so clients
		// do not fragment the message into many tiny segments.
		messageStarted := make(map[int]bool)

		scanner := bufio.NewScanner(canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout))
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 10*1024*1024) // 10MB max line buffer for large reasoning/completion lines

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

			// Check if upstream emitted an in-stream error
			var errChunk ErrorResponse
			if err := json.Unmarshal([]byte(data), &errChunk); err == nil && errChunk.Error.Message != "" {
				logger.Warn.Printf("[OpenAI Outbound] In-stream error chunk from provider for model %q: %s", model, errChunk.Error.Message)
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:  "error",
					Error: canonical.NewGatewayError("openai", model, errChunk.Error.Message, http.StatusBadRequest, []byte(data)),
				}
				return
			}

			var chunk ChatCompletionStreamResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				logger.Warn.Printf("[OpenAI Outbound] Failed to decode stream chunk for model %q: %v (raw: %s)", model, err, data)
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:  "error",
					Error: canonical.NewGatewayError("openai", model, "failed to decode stream chunk: "+err.Error(), http.StatusInternalServerError, nil),
				}
				return
			}

			for _, choice := range chunk.Choices {
				if choice.Delta.Role != "" && !messageStarted[choice.Index] {
					messageStarted[choice.Index] = true
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "message_start",
						MessageIndex: choice.Index,
					}
				}

				var reasoningText *string
				if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
					reasoningText = choice.Delta.ReasoningContent
				} else if choice.Delta.Reasoning != nil && *choice.Delta.Reasoning != "" {
					reasoningText = choice.Delta.Reasoning
				}

				if reasoningText != nil && *reasoningText != "" {
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "content_delta",
						MessageIndex: choice.Index,
						Delta: &canonical.CanonicalContentPart{
							Type: "reasoning",
							Reasoning: &canonical.CanonicalReasoning{
								Content: *reasoningText,
							},
						},
					}
				}

				if choice.Delta.Content != nil && *choice.Delta.Content != "" {
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
			logger.Warn.Printf("[OpenAI Outbound] Stream scanner error for model %q: %v", model, err)
			ch <- &canonical.CanonicalChatStreamEvent{
				Type:  "error",
				Error: canonical.NewGatewayError("openai", model, "stream read error: "+err.Error(), http.StatusInternalServerError, nil),
			}
		}
	}()

	return ch, nil
}
