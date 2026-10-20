package openairesponses

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

func (a *Adapter) translateCanonicalRequest(req *canonical.CanonicalChatRequest, model string, stream bool, endpoint config.Endpoint) ResponsesRequest {
	// We always send the full conversation history, so server-side storage is disabled.
	storeFalse := false

	outReq := ResponsesRequest{
		Model:           model,
		Temperature:     req.Params.Temperature,
		TopP:            req.Params.TopP,
		MaxOutputTokens: req.Params.MaxOutputTokens,
		Stream:          stream,
		Store:           &storeFalse,
	}

	// Drop parameters the target model does not support (e.g. temperature on reasoning models).
	for _, p := range omitParams(endpoint) {
		switch p {
		case "temperature":
			outReq.Temperature = nil
		case "top_p":
			outReq.TopP = nil
		case "max_output_tokens":
			outReq.MaxOutputTokens = nil
		}
	}

	// Reasoning effort
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
		outReq.Reasoning = &Reasoning{Effort: eff}
	}

	// Response format → text.format
	if req.ResponseFormat != nil {
		tf := &TextFormat{}
		switch req.ResponseFormat.Type {
		case "json_schema":
			tf.Type = "json_schema"
			tf.Schema = req.ResponseFormat.JSONSchema
			if req.ResponseFormat.Strict {
				t := true
				tf.Strict = &t
			}
		case "json_object":
			tf.Type = "json_object"
		default:
			tf.Type = "text"
		}
		outReq.Text = &TextConfig{Format: tf}
	}

	// Tools
	for _, t := range req.Tools {
		if t.Type == "function" {
			outReq.Tools = append(outReq.Tools, Tool{
				Type:        "function",
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			})
		}
	}

	// Tool choice
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "none", "auto":
			outReq.ToolChoice = req.ToolChoice.Type
		case "function":
			if req.ToolChoice.Name != "" {
				outReq.ToolChoice = map[string]any{
					"type": "function",
					"name": req.ToolChoice.Name,
				}
			}
		}
	}

	// System → instructions
	if len(req.System) > 0 {
		var sysStr string
		for _, part := range req.System {
			if part.Type == "text" {
				sysStr += part.Text
			}
		}
		outReq.Instructions = sysStr
	}

	// Messages → input items
	for _, msg := range req.Messages {
		// Collect tool calls and tool results as separate input items
		var contentParts []ContentPart
		var textContent string
		var hasImage bool

		for _, part := range msg.Content {
			switch part.Type {
			case "text":
				textContent += part.Text
				if part.Text != "" {
					contentParts = append(contentParts, ContentPart{
						Type: "input_text",
						Text: part.Text,
					})
				}
			case "image":
				if part.Image != nil {
					hasImage = true
					cp := ContentPart{
						Type:     "input_image",
						ImageURL: part.Image.URL,
					}
					if part.Image.Detail != "" {
						cp.Detail = part.Image.Detail
					}
					contentParts = append(contentParts, cp)
				}
			case "tool_call":
				if part.ToolCall != nil {
					outReq.Input = append(outReq.Input, InputItem{
						Type:      "function_call",
						CallID:    part.ToolCall.ID,
						Name:      part.ToolCall.Name,
						Arguments: string(part.ToolCall.Arguments),
					})
				}
			case "tool_result":
				if part.ToolResult != nil {
					var output string
					for _, resPart := range part.ToolResult.Content {
						if resPart.Type == "text" {
							output += resPart.Text
						}
					}
					outReq.Input = append(outReq.Input, InputItem{
						Type:   "function_call_output",
						CallID: part.ToolResult.ToolCallID,
						Output: output,
					})
				}
			}
		}

		// Build message input item if there's text/image content
		if textContent != "" || hasImage {
			item := InputItem{
				Type: "message",
				Role: string(msg.Role),
			}
			if hasImage {
				item.Content = contentParts
			} else {
				item.Content = textContent
			}
			outReq.Input = append(outReq.Input, item)
		}
	}

	return outReq
}

func (a *Adapter) ProcessChat(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	outReq := a.translateCanonicalRequest(req, model, false, endpoint)

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
		return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadGateway, nil)
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
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("openai-responses", model, errMsg, resp.StatusCode, respBody), resp.Header)
	}

	var outResp ResponsesResponse
	if err := json.Unmarshal(respBody, &outResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if outResp.Error != nil && outResp.Error.Message != "" {
		return nil, canonical.NewGatewayError("openai-responses", model, outResp.Error.Message, http.StatusBadGateway, respBody)
	}

	canonResp := &canonical.CanonicalChatResponse{
		ID:    outResp.ID,
		Model: outResp.Model,
		Raw:   respBody,
	}

	canonMsg := canonical.CanonicalMessage{
		Role: canonical.RoleAssistant,
	}

	finishReason := "stop"

	for _, item := range outResp.Output {
		switch item.Type {
		case "message":
			for _, cp := range item.Content {
				if cp.Type == "output_text" && cp.Text != "" {
					canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{
						Type: "text",
						Text: cp.Text,
					})
				}
			}
		case "function_call":
			canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{
				Type: "tool_call",
				ToolCall: &canonical.CanonicalToolCall{
					ID:        item.CallID,
					Name:      item.Name,
					Arguments: json.RawMessage(item.Arguments),
				},
			})
			finishReason = "tool_calls"
		case "reasoning":
			for _, s := range item.Summary {
				if s.Text != "" {
					canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{
						Type: "reasoning",
						Reasoning: &canonical.CanonicalReasoning{
							Content: s.Text,
						},
					})
				}
			}
		}
	}

	if outResp.Status == "incomplete" {
		finishReason = "length"
	}

	canonResp.Choices = []canonical.CanonicalChoice{
		{
			Index:        0,
			Message:      canonMsg,
			FinishReason: finishReason,
		},
	}

	if outResp.Usage != nil {
		usage := &canonical.CanonicalUsage{
			InputTokens:  outResp.Usage.InputTokens,
			OutputTokens: outResp.Usage.OutputTokens,
			TotalTokens:  outResp.Usage.TotalTokens,
		}
		if outResp.Usage.InputTokensDetails != nil {
			usage.CachedInputTokens = outResp.Usage.InputTokensDetails.CachedTokens
		}
		if outResp.Usage.OutputTokensDetails != nil {
			usage.ReasoningTokens = outResp.Usage.OutputTokensDetails.ReasoningTokens
		}
		canonResp.Usage = usage
	}

	return canonResp, nil
}

func (a *Adapter) ProcessChatStream(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	outReq := a.translateCanonicalRequest(req, model, true, endpoint)

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
		return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadGateway, nil)
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
		logger.Warn.Printf("[OpenAI Responses Outbound] HTTP %d from provider for model %q: %s", resp.StatusCode, model, errMsg)
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("openai-responses", model, errMsg, resp.StatusCode, respBody), resp.Header)
	}

	ch := make(chan *canonical.CanonicalChatStreamEvent)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout))
		buf := make([]byte, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)

		messageStarted := false
		toolCallIndex := 0

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

			var event StreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				logger.Warn.Printf("[OpenAI Responses Outbound] Failed to decode stream event for model %q: %v (raw: %s)", model, err, data)
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:  "error",
					Error: canonical.NewGatewayError("openai-responses", model, "failed to decode stream event: "+err.Error(), http.StatusInternalServerError, nil),
				}
				return
			}

			switch event.Type {
			case "response.created":
				if !messageStarted {
					messageStarted = true
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "message_start",
						MessageIndex: 0,
					}
				}

			case "response.output_text.delta":
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:         "content_delta",
					MessageIndex: 0,
					Delta: &canonical.CanonicalContentPart{
						Type: "text",
						Text: event.Delta,
					},
				}

			case "response.reasoning_summary_text.delta":
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:         "content_delta",
					MessageIndex: 0,
					Delta: &canonical.CanonicalContentPart{
						Type: "reasoning",
						Reasoning: &canonical.CanonicalReasoning{
							Content: event.Delta,
						},
					},
				}

			case "response.output_item.added":
				if event.Item != nil && event.Item.Type == "function_call" {
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "tool_call_delta",
						MessageIndex: 0,
						ToolCall: &canonical.CanonicalToolCallDelta{
							Index: toolCallIndex,
							ID:    event.Item.CallID,
							Name:  event.Item.Name,
						},
					}
				}

			case "response.function_call_arguments.delta":
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:         "tool_call_delta",
					MessageIndex: 0,
					ToolCall: &canonical.CanonicalToolCallDelta{
						Index:          toolCallIndex,
						ArgumentsDelta: event.Delta,
					},
				}

			case "response.output_item.done":
				if event.Item != nil && event.Item.Type == "function_call" {
					toolCallIndex++
				}

			case "response.completed":
				if event.Response != nil {
					finishReason := "stop"
					for _, item := range event.Response.Output {
						if item.Type == "function_call" {
							finishReason = "tool_calls"
							break
						}
					}
					if event.Response.Status == "incomplete" {
						finishReason = "length"
					}

					if event.Response.Usage != nil {
						usage := &canonical.CanonicalUsage{
							InputTokens:  event.Response.Usage.InputTokens,
							OutputTokens: event.Response.Usage.OutputTokens,
							TotalTokens:  event.Response.Usage.TotalTokens,
						}
						if event.Response.Usage.InputTokensDetails != nil {
							usage.CachedInputTokens = event.Response.Usage.InputTokensDetails.CachedTokens
						}
						if event.Response.Usage.OutputTokensDetails != nil {
							usage.ReasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
						}
						ch <- &canonical.CanonicalChatStreamEvent{
							Type:  "usage",
							Usage: usage,
						}
					}

					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "done",
						MessageIndex: 0,
						Stop:         &canonical.CanonicalStop{Reason: finishReason},
					}
				}
				return

			case "response.failed", "response.incomplete":
				errMsg := "response failed or incomplete"
				if event.Response != nil && event.Response.Error != nil {
					errMsg = event.Response.Error.Message
				}
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:  "error",
					Error: canonical.NewGatewayError("openai-responses", model, errMsg, http.StatusBadGateway, []byte(data)),
				}
				return

			case "error":
				errMsg := event.Message
				if errMsg == "" {
					errMsg = "unknown stream error"
				}
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:  "error",
					Error: canonical.NewGatewayError("openai-responses", model, errMsg, http.StatusBadRequest, []byte(data)),
				}
				return
			}
		}

		if err := scanner.Err(); err != nil {
			logger.Warn.Printf("[OpenAI Responses Outbound] Stream scanner error for model %q: %v", model, err)
			ch <- &canonical.CanonicalChatStreamEvent{
				Type:  "error",
				Error: canonical.NewGatewayError("openai-responses", model, "stream read error: "+err.Error(), http.StatusInternalServerError, nil),
			}
		}
	}()

	return ch, nil
}

// omitParams returns the list of canonical parameter names that should not be sent to the provider.
// Configured via the endpoint extension "omit_params" (list of strings).
func omitParams(endpoint config.Endpoint) []string {
	raw, ok := endpoint.Extensions["omit_params"]
	if !ok {
		return nil
	}
	var params []string
	switch v := raw.(type) {
	case []string:
		params = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				params = append(params, s)
			}
		}
	}
	return params
}
