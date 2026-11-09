package openairesponses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

// nativeBody combines validated native settings with gateway-applied defaults and policies.
func (a *Adapter) nativeBody(req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) ([]byte, error) {
	body := make(map[string]json.RawMessage, len(req.Responses.Settings)+8)
	for key, value := range req.Responses.Settings {
		body[key] = value
	}
	set := func(key string, value any) { body[key], _ = json.Marshal(value) }
	set("model", model)
	set("store", false)
	set("stream", req.Stream)
	items := make([]json.RawMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		found := false
		for _, part := range msg.Content {
			if part.Native != nil && part.Native.Provider == "openai" && part.Native.Format == "responses.item" && json.Valid(part.Native.Data) {
				if found {
					return nil, fmt.Errorf("multiple native Responses items in one message")
				}
				items = append(items, part.Native.Data)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("missing native Responses item")
		}
	}
	set("input", items)
	if req.Params.Temperature != nil {
		set("temperature", *req.Params.Temperature)
	}
	if req.Params.TopP != nil {
		set("top_p", *req.Params.TopP)
	}
	if req.Params.MaxOutputTokens != nil {
		set("max_output_tokens", *req.Params.MaxOutputTokens)
	}
	for _, name := range omitParams(endpoint) {
		delete(body, name)
	}
	if req.Params.ReasoningEffort != nil {
		reasoning := make(map[string]json.RawMessage)
		if raw, ok := body["reasoning"]; ok {
			if err := json.Unmarshal(raw, &reasoning); err != nil || reasoning == nil {
				return nil, fmt.Errorf("invalid reasoning settings")
			}
		}
		effort := *req.Params.ReasoningEffort
		switch mapping := endpoint.Extensions["reasoning_effort_mapping"].(type) {
		case map[string]any:
			if v, ok := mapping[effort].(string); ok {
				effort = v
			}
		case map[string]string:
			if v, ok := mapping[effort]; ok {
				effort = v
			}
		}
		reasoning["effort"], _ = json.Marshal(effort)
		set("reasoning", reasoning)
	}
	var include []string
	if raw, ok := body["include"]; ok {
		_ = json.Unmarshal(raw, &include)
	}
	found := false
	for _, item := range include {
		if item == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found && endpoint.Extensions["disable_encrypted_reasoning"] != true {
		include = append(include, "reasoning.encrypted_content")
	}
	if len(include) > 0 {
		set("include", include)
	}
	return json.Marshal(body)
}

func (a *Adapter) nativeHTTP(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*http.Response, error) {
	body, err := a.nativeBody(req, model, endpoint)
	if err != nil {
		return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadRequest, nil)
	}
	url := strings.ReplaceAll(endpoint.URL, "{target_model}", model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range endpoint.Headers {
		httpReq.Header.Set(k, v)
	}
	if err := a.authorize(ctx, httpReq, endpoint, model); err != nil {
		return nil, err
	}
	resp, err := a.doRequest(ctx, httpReq, endpoint)
	if err != nil {
		return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadGateway, nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reader := canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout)
		defer reader.Close()
		stopClosing := context.AfterFunc(ctx, func() { _ = reader.Close() })
		defer stopClosing()
		body, truncated, readErr := readBoundedResponse(reader, maxErrorBodyBytes)
		var envelope ErrorResponse
		message := "provider error response was incomplete or too large"
		if readErr == nil && !truncated && json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
			message = envelope.Error.Message
		} else if readErr == nil && !truncated {
			message = string(body)
		}
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("openai-responses", model, message, resp.StatusCode, body), resp.Header)
	}
	return resp, nil
}

func nativeUsage(usage *ResponseUsage) *canonical.CanonicalUsage {
	if usage == nil {
		return nil
	}
	result := &canonical.CanonicalUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
	if usage.InputTokensDetails != nil {
		result.CachedInputTokens = usage.InputTokensDetails.CachedTokens
	}
	if usage.OutputTokensDetails != nil {
		result.ReasoningTokens = usage.OutputTokensDetails.ReasoningTokens
	}
	return result
}

func (a *Adapter) processNative(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	resp, err := a.nativeHTTP(ctx, req, model, endpoint)
	if err != nil {
		return nil, err
	}
	reader := canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout)
	defer reader.Close()
	stopClosing := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stopClosing()
	body, truncated, err := readBoundedResponse(reader, maxResponseBodyBytes)
	if err != nil {
		return nil, canonical.NewGatewayError("openai-responses", model, "failed to read response body", http.StatusBadGateway, nil)
	}
	if truncated {
		return nil, canonical.NewGatewayError("openai-responses", model, "provider response body exceeds 40 MiB", http.StatusBadGateway, nil)
	}
	var result struct {
		ID     string          `json:"id"`
		Model  string          `json:"model"`
		Status string          `json:"status"`
		Output json.RawMessage `json:"output"`
		Usage  *ResponseUsage  `json:"usage"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &result) != nil || !validTerminalResponse(body) || (result.Status != "completed" && result.Status != "incomplete") || (len(result.Error) > 0 && !bytes.Equal(result.Error, []byte("null"))) {
		return nil, canonical.NewGatewayError("openai-responses", model, "provider returned an invalid or failed response", http.StatusBadGateway, body)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(result.Output, &items); err != nil {
		return nil, canonical.NewGatewayError("openai-responses", model, "provider returned invalid output items", http.StatusBadGateway, body)
	}
	message := canonical.CanonicalMessage{Role: canonical.RoleAssistant}
	for _, item := range items {
		message.Content = append(message.Content, canonical.CanonicalContentPart{Type: "native", Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.item", Data: item}})
	}
	out := &canonical.CanonicalChatResponse{ID: result.ID, Model: result.Model, Usage: nativeUsage(result.Usage), Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.response", Data: body}, Choices: []canonical.CanonicalChoice{{Message: message}}}
	if result.Status == "incomplete" {
		out.Stop = &canonical.CanonicalStop{Reason: "length"}
	}
	return out, nil
}

func validStreamContentType(header string, endpoint config.Endpoint) bool {
	if strings.TrimSpace(header) == "" && isChatGPTEndpoint(endpoint) {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(header)
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

func (a *Adapter) processNativeStream(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	resp, err := a.nativeHTTP(ctx, req, model, endpoint)
	if err != nil {
		return nil, err
	}
	if !validStreamContentType(resp.Header.Get("Content-Type"), endpoint) {
		resp.Body.Close()
		return nil, canonical.NewGatewayError("openai-responses", model, "provider did not return an SSE stream", http.StatusBadGateway, nil)
	}
	ch := make(chan *canonical.CanonicalChatStreamEvent)
	go func() {
		defer close(ch)
		reader := canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout)
		defer reader.Close()
		send := func(event *canonical.CanonicalChatStreamEvent) bool {
			select {
			case ch <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		fail := func(message string) {
			send(&canonical.CanonicalChatStreamEvent{Type: "error", Error: canonical.NewGatewayError("openai-responses", model, message, http.StatusBadGateway, nil)})
		}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
		var data []string
		var eventName string
		dataSize := 0
		dispatch := func() (bool, bool) {
			if len(data) == 0 {
				eventName = ""
				return false, true
			}
			payload := []byte(strings.Join(data, "\n"))
			data = nil
			dataSize = 0
			name := eventName
			eventName = ""
			var event struct {
				Type     string `json:"type"`
				Response *struct {
					Status string          `json:"status"`
					Usage  *ResponseUsage  `json:"usage"`
					Error  json.RawMessage `json:"error"`
				} `json:"response"`
			}
			if json.Unmarshal(payload, &event) != nil || !validEventType(event.Type) || (name != "" && name != event.Type) {
				fail("invalid provider SSE event")
				return true, false
			}
			var envelope struct {
				Response json.RawMessage `json:"response"`
				Item     json.RawMessage `json:"item"`
			}
			_ = json.Unmarshal(payload, &envelope)
			if event.Type == "response.output_item.added" || event.Type == "response.output_item.done" {
				if err := validateNativeOutputItem(envelope.Item, event.Type == "response.output_item.done"); err != nil {
					fail(err.Error())
					return true, false
				}
			}
			out := &canonical.CanonicalChatStreamEvent{Type: "native", Delta: &canonical.CanonicalContentPart{Type: "native", Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.event", Data: payload}}}
			switch event.Type {
			case "response.completed":
				if event.Response == nil || event.Response.Status != "completed" || (len(event.Response.Error) > 0 && !bytes.Equal(event.Response.Error, []byte("null"))) || !validTerminalResponse(envelope.Response) {
					fail("invalid response.completed event")
					return true, false
				}
				out.Type = "done"
				out.Usage = nativeUsage(event.Response.Usage)
			case "response.incomplete":
				if isChatGPTEndpoint(endpoint) {
					fail("provider response was incomplete")
					return true, false
				}
				if event.Response == nil || event.Response.Status != "incomplete" || (len(event.Response.Error) > 0 && !bytes.Equal(event.Response.Error, []byte("null"))) || !validTerminalResponse(envelope.Response) {
					fail("invalid response.incomplete event")
					return true, false
				}
				out.Type = "done"
				out.Usage = nativeUsage(event.Response.Usage)
				out.Stop = &canonical.CanonicalStop{Reason: "length"}
			case "response.failed", "error":
				out.Type = "error"
			}
			if !send(out) {
				return true, false
			}
			return out.Type == "done" || out.Type == "error", true
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				terminal, _ := dispatch()
				if terminal {
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			name, value, hasColon := strings.Cut(line, ":")
			if hasColon && strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			if name == "event" {
				eventName = value
			}
			if name == "data" {
				dataSize += len(value) + 1
				if dataSize > 10*1024*1024 {
					fail("provider SSE event is too large")
					return
				}
				data = append(data, value)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := scanner.Err(); err != nil {
			fail("provider stream read error: " + err.Error())
			return
		}
		// An unterminated SSE event is not dispatched by the SSE specification.
		fail("provider stream ended before a terminal event")
	}()
	return ch, nil
}

func validateNativeOutputItem(raw json.RawMessage, complete bool) error {
	var item struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments *string         `json:"arguments"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return fmt.Errorf("invalid provider output item")
	}
	switch item.Type {
	case "message":
		if complete && (item.Role != "assistant" || len(item.Content) == 0 || bytes.Equal(item.Content, []byte("null"))) {
			return fmt.Errorf("invalid provider message item")
		}
		var parts []struct {
			Type string `json:"type"`
		}
		if len(item.Content) > 0 && json.Unmarshal(item.Content, &parts) != nil {
			return fmt.Errorf("invalid provider message content")
		}
		for _, part := range parts {
			if part.Type != "output_text" && part.Type != "refusal" {
				return fmt.Errorf("unsupported provider output content type %q", part.Type)
			}
		}
	case "function_call":
		if complete && (item.CallID == "" || item.Name == "" || item.Arguments == nil) {
			return fmt.Errorf("invalid provider function call")
		}
	case "reasoning":
	default:
		return fmt.Errorf("unsupported provider output item type %q", item.Type)
	}
	return nil
}

func validTerminalResponse(raw json.RawMessage) bool {
	var shape struct {
		ID     string          `json:"id"`
		Object string          `json:"object"`
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &shape) != nil || shape.ID == "" || shape.Object != "response" || len(shape.Output) == 0 || bytes.Equal(shape.Output, []byte("null")) {
		return false
	}
	var output []json.RawMessage
	if json.Unmarshal(shape.Output, &output) != nil {
		return false
	}
	for _, item := range output {
		if validateNativeOutputItem(item, true) != nil {
			return false
		}
	}
	return true
}

func validEventType(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_') {
			return false
		}
	}
	return true
}
