package anthropic

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

func (a *Adapter) ProcessChat(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	outReq, err := a.translateRequest(req, model, endpoint)
	if err != nil {
		return nil, err
	}

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
	httpReq.Header.Set("anthropic-version", "2023-06-01") // Default version

	for k, v := range endpoint.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := a.retryPolicy.Do(ctx, a.client, httpReq)
	if err != nil {
		return nil, canonical.NewGatewayError("anthropic", model, err.Error(), http.StatusBadGateway, nil)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp ErrorResponse
		errMsg := "unknown provider error"
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		} else {
			errMsg = string(respBody)
		}
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("anthropic", model, errMsg, resp.StatusCode, respBody), resp.Header)
	}

	var outResp MessagesResponse
	if err := json.Unmarshal(respBody, &outResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	canonResp := &canonical.CanonicalChatResponse{
		ID:    outResp.ID,
		Model: outResp.Model,
		Raw:   respBody,
		Choices: []canonical.CanonicalChoice{
			{
				Index: 0,
				Message: canonical.CanonicalMessage{
					Role: canonical.RoleAssistant,
				},
			},
		},
	}

	if outResp.StopReason != nil {
		reason := *outResp.StopReason
		if reason == "tool_use" {
			reason = "tool_calls"
		}
		canonResp.Choices[0].FinishReason = reason
		canonResp.Stop = &canonical.CanonicalStop{Reason: reason}
	}

	for _, block := range outResp.Content {
		if block.Type == "text" {
			canonResp.Choices[0].Message.Content = append(canonResp.Choices[0].Message.Content, canonical.CanonicalContentPart{
				Type: "text",
				Text: block.Text,
			})
		} else if block.Type == "thinking" || block.Type == "redacted_thinking" {
			canonResp.Choices[0].Message.Content = append(canonResp.Choices[0].Message.Content, canonical.CanonicalContentPart{
				Type:      "reasoning",
				Reasoning: &canonical.CanonicalReasoning{Content: block.Thinking},
				Native:    &canonical.CanonicalProviderData{Provider: "anthropic", Format: block.Type, Data: block.Native},
			})
		} else if block.Type == "tool_use" {
			canonResp.Choices[0].Message.Content = append(canonResp.Choices[0].Message.Content, canonical.CanonicalContentPart{
				Type: "tool_call",
				ToolCall: &canonical.CanonicalToolCall{
					ID:        block.ID,
					Name:      block.Name,
					Arguments: block.Input,
				},
			})
		}
	}

	if outResp.Usage != nil {
		canonResp.Usage = &canonical.CanonicalUsage{
			InputTokens:  outResp.Usage.InputTokens,
			OutputTokens: outResp.Usage.OutputTokens,
			TotalTokens:  outResp.Usage.InputTokens + outResp.Usage.OutputTokens,
		}
	}

	return canonResp, nil
}

func (a *Adapter) translateRequest(req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (MessagesRequest, error) {
	var thinking *ThinkingConfig
	var outputConfig *OutputConfig
	mode := "adaptive"
	if raw, ok := endpoint.Extensions["thinking_mode"]; ok {
		var valid bool
		mode, valid = raw.(string)
		if !valid || (mode != "adaptive" && mode != "budget") {
			return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, "invalid thinking_mode: expected adaptive or budget", http.StatusInternalServerError, nil)
		}
	}
	if req.Params.ReasoningEffort != nil {
		eff := *req.Params.ReasoningEffort
		if !canonical.IsValidReasoningEffort(eff) {
			return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, fmt.Sprintf("invalid reasoning_effort %q", eff), http.StatusBadRequest, nil)
		}
		// A mapping is explicit, including mappings from none to another level.
		switch mapping := endpoint.Extensions["reasoning_effort_mapping"].(type) {
		case map[string]any:
			if value, ok := mapping[eff]; ok {
				var valid bool
				eff, valid = value.(string)
				if !valid {
					return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, "reasoning_effort_mapping values must be strings", http.StatusInternalServerError, nil)
				}
			}
		case map[string]string:
			if value, ok := mapping[eff]; ok {
				eff = value
			}
		}
		if !canonical.IsValidReasoningEffort(eff) {
			return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, fmt.Sprintf("invalid mapped reasoning_effort %q", eff), http.StatusInternalServerError, nil)
		}
		if eff == "none" {
			thinking = &ThinkingConfig{Type: "disabled"}
		} else if mode == "budget" {
			thinking = &ThinkingConfig{Type: "enabled", BudgetTokens: canonical.ReasoningTokenBudget(eff)}
		} else {
			if eff == "minimal" {
				eff = "low"
			}
			thinking = &ThinkingConfig{Type: "adaptive"}
			outputConfig = &OutputConfig{Effort: eff}
		}
	}

	outReq := MessagesRequest{
		Model:         model,
		StopSequences: req.Params.Stop,
		Stream:        req.Stream,
		Temperature:   req.Params.Temperature,
		TopP:          req.Params.TopP,
		Thinking:      thinking,
		OutputConfig:  outputConfig,
	}

	if req.Params.MaxOutputTokens != nil {
		outReq.MaxTokens = *req.Params.MaxOutputTokens
	} else {
		outReq.MaxTokens = 4096 // Anthropic requires max_tokens
	}

	if len(req.System) > 0 {
		var sysStr string
		for _, part := range req.System {
			if part.Type == "text" {
				sysStr += part.Text
			}
		}
		outReq.System = sysStr
	}

	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			if t.Type == "function" {
				outReq.Tools = append(outReq.Tools, Tool{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.InputSchema,
				})
			}
		}
	}

	if req.ToolChoice != nil {
		if req.ToolChoice.Type == "none" || req.ToolChoice.Type == "auto" {
			outReq.ToolChoice = &ToolChoice{Type: req.ToolChoice.Type}
		} else if req.ToolChoice.Type == "function" && req.ToolChoice.Name != "" {
			outReq.ToolChoice = &ToolChoice{Type: "tool", Name: req.ToolChoice.Name}
		}
	}

	var messages []Message
	for _, msg := range req.Messages {
		if msg.Role == canonical.RoleSystem {
			continue // System handled above
		}

		var blocks []ContentBlock
		var textStr string

		for _, part := range msg.Content {
			if part.Type == "text" {
				textStr += part.Text
			} else if part.Type == "reasoning" && part.Native != nil && part.Native.Provider == "anthropic" && (part.Native.Format == "thinking" || part.Native.Format == "redacted_thinking") {
				if textStr != "" {
					blocks = append(blocks, ContentBlock{Type: "text", Text: textStr})
					textStr = ""
				}
				blocks = append(blocks, ContentBlock{Type: part.Native.Format, Native: part.Native.Data})
			} else if part.Type == "image" && part.Image != nil {
				if textStr != "" {
					blocks = append(blocks, ContentBlock{Type: "text", Text: textStr})
					textStr = ""
				}
				if strings.HasPrefix(part.Image.URL, "data:") {
					idx := strings.Index(part.Image.URL, ";base64,")
					if idx != -1 {
						mediaType := part.Image.URL[5:idx]
						base64Data := part.Image.URL[idx+8:]
						blocks = append(blocks, ContentBlock{
							Type: "image",
							Source: &ImageSource{
								Type:      "base64",
								MediaType: mediaType,
								Data:      base64Data,
							},
						})
					}
				} else if part.Image.URL != "" {
					blocks = append(blocks, ContentBlock{
						Type: "image",
						Source: &ImageSource{
							Type: "url",
							URL:  part.Image.URL,
						},
					})
				}
			} else if part.Type == "tool_call" && part.ToolCall != nil {
				// flush accumulated text before adding tool_use block
				if textStr != "" {
					blocks = append(blocks, ContentBlock{Type: "text", Text: textStr})
					textStr = ""
				}
				input := part.ToolCall.Arguments
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, ContentBlock{
					Type:  "tool_use",
					ID:    part.ToolCall.ID,
					Name:  part.ToolCall.Name,
					Input: input,
				})
			} else if part.Type == "tool_result" && part.ToolResult != nil {
				// flush accumulated text before adding tool_result block
				if textStr != "" {
					blocks = append(blocks, ContentBlock{Type: "text", Text: textStr})
					textStr = ""
				}
				var resContent string
				for _, resPart := range part.ToolResult.Content {
					if resPart.Type == "text" {
						resContent += resPart.Text
					}
				}
				var isError *bool
				if part.ToolResult.IsError {
					value := true
					isError = &value
				}
				blocks = append(blocks, ContentBlock{
					Type:      "tool_result",
					ToolUseID: part.ToolResult.ToolCallID,
					Content:   resContent,
					IsError:   isError,
				})
			}
		}

		if textStr != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: textStr})
		}

		role := string(msg.Role)
		if role == "tool" {
			role = "user" // Anthropic expects tool_result in user message
		} else if role != "user" && role != "assistant" {
			role = "user" // Fallback to user for unknown roles
		}

		if len(messages) > 0 && messages[len(messages)-1].Role == role {
			// Combine consecutive messages of the same role
			prevBlocks, ok := messages[len(messages)-1].Content.([]ContentBlock)
			if ok {
				messages[len(messages)-1].Content = append(prevBlocks, blocks...)
			} else {
				// Should not happen with new structure, but safe check
				if prevContentStr, okStr := messages[len(messages)-1].Content.(string); okStr {
					newBlocks := []ContentBlock{{Type: "text", Text: prevContentStr}}
					messages[len(messages)-1].Content = append(newBlocks, blocks...)
				}
			}
		} else {
			messages = append(messages, Message{
				Role:    role,
				Content: blocks, // Use blocks directly
			})
		}
	}

	// Anthropic strictly requires starting with a user message (if not empty)
	if len(messages) > 0 && messages[0].Role != "user" {
		messages = append([]Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: " "}}}}, messages...)
	}

	outReq.Messages = messages
	return outReq, nil
}

func (a *Adapter) ProcessChatStream(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	outReq, err := a.translateRequest(req, model, endpoint)
	if err != nil {
		return nil, err
	}
	outReq.Stream = true

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
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	for k, v := range endpoint.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := a.retryPolicy.Do(ctx, a.client, httpReq)
	if err != nil {
		return nil, canonical.NewGatewayError("anthropic", model, err.Error(), http.StatusBadGateway, nil)
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
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("anthropic", model, errMsg, resp.StatusCode, respBody), resp.Header)
	}

	eventChan := make(chan *canonical.CanonicalChatStreamEvent)

	go func() {
		defer resp.Body.Close()
		defer close(eventChan)

		reader := bufio.NewReader(canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout))
		var eventType string
		openBlocks := make(map[int]bool)

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					eventChan <- &canonical.CanonicalChatStreamEvent{
						Type:  "error",
						Error: canonical.NewGatewayError("anthropic", model, fmt.Sprintf("stream read error: %v", err), http.StatusInternalServerError, nil),
					}
				}
				break
			}

			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
				continue
			}

			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					break // Shouldn't happen for Anthropic usually, but safety
				}

				var event StreamEvent
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					eventChan <- &canonical.CanonicalChatStreamEvent{
						Type:  "error",
						Error: canonical.NewGatewayError("anthropic", model, fmt.Sprintf("failed to parse event data: %v", err), http.StatusInternalServerError, nil),
					}
					continue
				}

				// If eventType wasn't in event:, fallback to event.Type
				if eventType == "" {
					eventType = event.Type
				}

				switch eventType {
				case "message_start":
					if event.Message != nil {
						usage := event.Message.Usage
						var canonUsage *canonical.CanonicalUsage
						if usage != nil {
							canonUsage = &canonical.CanonicalUsage{
								InputTokens:  usage.InputTokens,
								OutputTokens: usage.OutputTokens,
								TotalTokens:  usage.InputTokens + usage.OutputTokens,
							}
						}
						eventChan <- &canonical.CanonicalChatStreamEvent{
							Type:       "message_start",
							ResponseID: event.Message.ID,
							Usage:      canonUsage,
						}
					}
				case "content_block_start":
					if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
						idx := 0
						if event.Index != nil {
							idx = *event.Index
						}
						openBlocks[idx] = true
						eventChan <- &canonical.CanonicalChatStreamEvent{
							Type:         "tool_call_start",
							MessageIndex: 0,
							ContentIndex: idx,
							Delta: &canonical.CanonicalContentPart{
								Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: "tool_use", Data: event.ContentBlock.Native},
							},
							ToolCall: &canonical.CanonicalToolCallDelta{
								Index: idx,
								ID:    event.ContentBlock.ID,
								Name:  event.ContentBlock.Name,
							},
						}
					} else if event.ContentBlock != nil && (event.ContentBlock.Type == "text" || event.ContentBlock.Type == "thinking" || event.ContentBlock.Type == "redacted_thinking") {
						idx := 0
						if event.Index != nil {
							idx = *event.Index
						}
						openBlocks[idx] = true
						block := event.ContentBlock
						part := &canonical.CanonicalContentPart{
							Type:   "text",
							Text:   block.Text,
							Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: block.Type, Data: block.Native},
						}
						if block.Type != "text" {
							part.Type = "reasoning"
							part.Reasoning = &canonical.CanonicalReasoning{Content: block.Thinking}
						}
						eventChan <- &canonical.CanonicalChatStreamEvent{Type: "content_start", ContentIndex: idx, Delta: part}
					}
				case "content_block_delta":
					if event.Delta != nil {
						idx := 0
						if event.Index != nil {
							idx = *event.Index
						}
						if event.Delta.Type == "text_delta" {
							eventChan <- &canonical.CanonicalChatStreamEvent{
								Type:         "content_delta",
								MessageIndex: 0,
								ContentIndex: idx,
								Delta: &canonical.CanonicalContentPart{
									Type: "text",
									Text: event.Delta.Text,
								},
							}
						} else if event.Delta.Type == "thinking_delta" || event.Delta.Type == "signature_delta" {
							eventChan <- &canonical.CanonicalChatStreamEvent{
								Type:         "content_delta",
								ContentIndex: idx,
								Delta: &canonical.CanonicalContentPart{
									Type:      "reasoning",
									Reasoning: &canonical.CanonicalReasoning{Content: event.Delta.Thinking},
									Native:    &canonical.CanonicalProviderData{Provider: "anthropic", Format: event.Delta.Type, Data: event.Delta.Native},
								},
							}
						} else if event.Delta.Type == "input_json_delta" {
							eventChan <- &canonical.CanonicalChatStreamEvent{
								Type:         "tool_call_delta",
								MessageIndex: 0,
								ContentIndex: idx,
								ToolCall: &canonical.CanonicalToolCallDelta{
									Index:          idx,
									ArgumentsDelta: event.Delta.PartialJson,
								},
							}
						}
					}
				case "content_block_stop":
					idx := 0
					if event.Index != nil {
						idx = *event.Index
					}
					if openBlocks[idx] {
						delete(openBlocks, idx)
						eventChan <- &canonical.CanonicalChatStreamEvent{Type: "content_stop", ContentIndex: idx}
					}
				case "message_delta":
					if event.Delta != nil && event.Delta.StopReason != nil {
						finishReason := *event.Delta.StopReason
						if finishReason == "tool_use" {
							finishReason = "tool_calls"
						}
						var usage *canonical.CanonicalUsage
						if event.Usage != nil {
							usage = &canonical.CanonicalUsage{OutputTokens: event.Usage.OutputTokens}
						}
						eventChan <- &canonical.CanonicalChatStreamEvent{
							Type:  "done",
							Stop:  &canonical.CanonicalStop{Reason: finishReason},
							Usage: usage,
						}
					} else if event.Usage != nil {
						eventChan <- &canonical.CanonicalChatStreamEvent{
							Type:  "usage",
							Usage: &canonical.CanonicalUsage{OutputTokens: event.Usage.OutputTokens},
						}
					}
				case "error":
					eventChan <- &canonical.CanonicalChatStreamEvent{
						Type:  "error",
						Error: canonical.NewGatewayError("anthropic", model, fmt.Sprintf("anthropic stream error: %s", data), http.StatusInternalServerError, nil),
					}
					return
				}
				eventType = "" // Reset for next event
			}
		}
	}()

	return eventChan, nil
}
