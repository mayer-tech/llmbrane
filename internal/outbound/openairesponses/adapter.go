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
)

type Adapter struct {
	client      *http.Client
	idleTimeout time.Duration
	maxRetries  int
}

func NewAdapter() *Adapter {
	return NewAdapterWithTimeout(60 * time.Second)
}

func NewAdapterWithTimeout(idleTimeout time.Duration) *Adapter {
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}
	return &Adapter{
		client:      &http.Client{},
		idleTimeout: idleTimeout,
		maxRetries:  3,
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

	logger.Debug.Printf("[OpenAI Responses Outbound] Request POST %s\n%s", url, formatJSON(reqBody))

	maxRetries := maxRetries(endpoint, a.maxRetries)
	var respBody []byte
	var statusCode int

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := retryBackoff(attempt, respBody)
			logger.Info.Printf("[OpenAI Responses Outbound] Retry %d/%d for model %q after %v", attempt, maxRetries, model, backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

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
			return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadGateway, nil)
		}

		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		statusCode = resp.StatusCode
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %w", err)
		}

		logger.Debug.Printf("[OpenAI Responses Outbound] Response HTTP %d\n%s", statusCode, formatJSON(respBody))

		if statusCode == http.StatusTooManyRequests && attempt < maxRetries {
			continue
		}
		break
	}

	if statusCode < 200 || statusCode >= 300 {
		var errResp ErrorResponse
		errMsg := "unknown provider error"
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		} else {
			errMsg = string(respBody)
		}
		return nil, canonical.NewGatewayError("openai-responses", model, errMsg, statusCode, respBody)
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

	logger.Debug.Printf("[OpenAI Responses Outbound] Stream Request POST %s\n%s", url, formatJSON(reqBody))

	maxRetries := maxRetries(endpoint, a.maxRetries)
	var resp *http.Response

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := retryBackoff(attempt, nil)
			logger.Info.Printf("[OpenAI Responses Outbound] Stream retry %d/%d for model %q after %v", attempt, maxRetries, model, backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create http request: %w", err)
		}

		httpReq.Header.Set("Content-Type", "application/json")
		for k, v := range endpoint.Headers {
			httpReq.Header.Set(k, v)
		}

		resp, err = a.client.Do(httpReq)
		if err != nil {
			return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadGateway, nil)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			logger.Warn.Printf("[OpenAI Responses Outbound] HTTP 429 from provider for model %q: %s", model, string(respBody))
			continue
		}
		break
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
		return nil, canonical.NewGatewayError("openai-responses", model, errMsg, resp.StatusCode, respBody)
	}

	logger.Debug.Printf("[OpenAI Responses Outbound] Stream started with HTTP %d for model %q", resp.StatusCode, model)

	// Buffer events until we know if the stream will succeed or fail.
	// This allows us to return an HTTP error before any events are sent to the client.
	var bufferedEvents []*canonical.CanonicalChatStreamEvent
	scanner := bufio.NewScanner(canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout))
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	messageStarted := false
	toolCallIndex := 0
	streamFailed := false
	var streamErr *canonical.GatewayError

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		logger.Debug.Printf("[OpenAI Responses Outbound] Stream Event (HTTP %d)\n%s", resp.StatusCode, formatJSON([]byte(data)))

		var event StreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			logger.Warn.Printf("[OpenAI Responses Outbound] Failed to decode stream event for model %q: %v (raw: %s)", model, err, data)
			streamFailed = true
			streamErr = canonical.NewGatewayError("openai-responses", model, "failed to decode stream event: "+err.Error(), http.StatusInternalServerError, nil)
			break
		}

		// Check for error events before any content
		if event.Type == "error" {
			errMsg := event.Message
			errCode := event.Code
			if event.Error != nil {
				if event.Error.Message != "" {
					errMsg = event.Error.Message
				}
				if event.Error.Code != "" {
					errCode = event.Error.Code
				}
			}
			if errMsg == "" {
				errMsg = "unknown stream error"
			}
			statusCode := http.StatusBadRequest
			if errCode == "rate_limit_exceeded" {
				statusCode = http.StatusTooManyRequests
			}
			streamFailed = true
			streamErr = canonical.NewGatewayError("openai-responses", model, errMsg, statusCode, []byte(data))
			break
		}

		if event.Type == "response.failed" || event.Type == "response.incomplete" {
			errMsg := "response failed or incomplete"
			if event.Response != nil && event.Response.Error != nil {
				errMsg = event.Response.Error.Message
			}
			streamFailed = true
			streamErr = canonical.NewGatewayError("openai-responses", model, errMsg, http.StatusBadGateway, []byte(data))
			break
		}

		// Translate and buffer the event
		translated := a.translateStreamEvent(&event, &messageStarted, &toolCallIndex)
		if translated != nil {
			bufferedEvents = append(bufferedEvents, translated)
		}

		// If we have real content, we can stop buffering and start streaming
		if len(bufferedEvents) > 0 && hasRealContent(bufferedEvents) {
			break
		}
	}

	resp.Body.Close()

	if streamFailed {
		return nil, streamErr
	}

	// Create channel and send buffered events, then continue streaming
	ch := make(chan *canonical.CanonicalChatStreamEvent)

	go func() {
		defer close(ch)

		// Send buffered events first
		for _, ev := range bufferedEvents {
			select {
			case <-ctx.Done():
				return
			case ch <- ev:
			}
		}

		// Continue reading the rest of the stream
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

			logger.Debug.Printf("[OpenAI Responses Outbound] Stream Event (HTTP %d)\n%s", resp.StatusCode, formatJSON([]byte(data)))

			var event StreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				logger.Warn.Printf("[OpenAI Responses Outbound] Failed to decode stream event for model %q: %v (raw: %s)", model, err, data)
				ch <- &canonical.CanonicalChatStreamEvent{
					Type:  "error",
					Error: canonical.NewGatewayError("openai-responses", model, "failed to decode stream event: "+err.Error(), http.StatusInternalServerError, nil),
				}
				return
			}

			translated := a.translateStreamEvent(&event, &messageStarted, &toolCallIndex)
			if translated != nil {
				ch <- translated
			}

			// Check for terminal events
			if event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.incomplete" || event.Type == "error" {
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

// translateStreamEvent converts a StreamEvent to a CanonicalChatStreamEvent.
// Returns nil for events that should not be forwarded.
func (a *Adapter) translateStreamEvent(event *StreamEvent, messageStarted *bool, toolCallIndex *int) *canonical.CanonicalChatStreamEvent {
	switch event.Type {
	case "response.created":
		if !*messageStarted {
			*messageStarted = true
			return &canonical.CanonicalChatStreamEvent{
				Type:         "message_start",
				MessageIndex: 0,
			}
		}

	case "response.output_text.delta":
		return &canonical.CanonicalChatStreamEvent{
			Type:         "content_delta",
			MessageIndex: 0,
			Delta: &canonical.CanonicalContentPart{
				Type: "text",
				Text: event.Delta,
			},
		}

	case "response.reasoning_summary_text.delta":
		return &canonical.CanonicalChatStreamEvent{
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
			return &canonical.CanonicalChatStreamEvent{
				Type:         "tool_call_delta",
				MessageIndex: 0,
				ToolCall: &canonical.CanonicalToolCallDelta{
					Index: *toolCallIndex,
					ID:    event.Item.CallID,
					Name:  event.Item.Name,
				},
			}
		}

	case "response.function_call_arguments.delta":
		return &canonical.CanonicalChatStreamEvent{
			Type:         "tool_call_delta",
			MessageIndex: 0,
			ToolCall: &canonical.CanonicalToolCallDelta{
				Index:          *toolCallIndex,
				ArgumentsDelta: event.Delta,
			},
		}

	case "response.output_item.done":
		if event.Item != nil && event.Item.Type == "function_call" {
			*toolCallIndex++
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
				return &canonical.CanonicalChatStreamEvent{
					Type:  "usage",
					Usage: usage,
				}
			}

			return &canonical.CanonicalChatStreamEvent{
				Type:         "done",
				MessageIndex: 0,
				Stop:         &canonical.CanonicalStop{Reason: finishReason},
			}
		}

	case "response.failed", "response.incomplete":
		errMsg := "response failed or incomplete"
		if event.Response != nil && event.Response.Error != nil {
			errMsg = event.Response.Error.Message
		}
		return &canonical.CanonicalChatStreamEvent{
			Type:  "error",
			Error: canonical.NewGatewayError("openai-responses", "", errMsg, http.StatusBadGateway, nil),
		}

	case "error":
		errMsg := event.Message
		errCode := event.Code
		if event.Error != nil {
			if event.Error.Message != "" {
				errMsg = event.Error.Message
			}
			if event.Error.Code != "" {
				errCode = event.Error.Code
			}
		}
		if errMsg == "" {
			errMsg = "unknown stream error"
		}
		statusCode := http.StatusBadRequest
		if errCode == "rate_limit_exceeded" {
			statusCode = http.StatusTooManyRequests
		}
		return &canonical.CanonicalChatStreamEvent{
			Type:  "error",
			Error: canonical.NewGatewayError("openai-responses", "", errMsg, statusCode, nil),
		}
	}

	return nil
}

// hasRealContent checks if any buffered event contains actual content (not just message_start).
func hasRealContent(events []*canonical.CanonicalChatStreamEvent) bool {
	for _, ev := range events {
		if ev.Type == "content_delta" || ev.Type == "tool_call_delta" {
			return true
		}
	}
	return false
}

// formatJSON returns indented JSON for debug logging, or the raw string if not valid JSON.
func formatJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return string(data)
	}
	return buf.String()
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

// maxRetries returns the configured max retries from the endpoint extension, or the default.
func maxRetries(endpoint config.Endpoint, defaultVal int) int {
	if raw, ok := endpoint.Extensions["max_retries"]; ok {
		switch v := raw.(type) {
		case int:
			return v
		case float64:
			return int(v)
		}
	}
	return defaultVal
}

// retryBackoff calculates the wait duration before the next retry attempt.
// It tries to parse "try again in Xs" from the provider error message, otherwise uses exponential backoff.
func retryBackoff(attempt int, respBody []byte) time.Duration {
	// Try to parse provider-suggested wait time from error message
	if respBody != nil {
		var errResp ErrorResponse
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error.Message != "" {
			if d := parseRetryAfter(errResp.Error.Message); d > 0 {
				return d
			}
		}
	}
	// Exponential backoff: 1s, 2s, 4s, ...
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// parseRetryAfter extracts the wait duration from messages like "Please try again in 3.199s."
func parseRetryAfter(msg string) time.Duration {
	idx := strings.Index(msg, "try again in ")
	if idx < 0 {
		return 0
	}
	rest := msg[idx+len("try again in "):]
	end := strings.Index(rest, "s")
	if end < 0 {
		return 0
	}
	seconds, err := parseFloat(strings.TrimSpace(rest[:end]))
	if err != nil || seconds <= 0 {
		return 0
	}
	// Add a small buffer to the suggested wait time
	return time.Duration(seconds*1000+500) * time.Millisecond
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	return f, err
}
