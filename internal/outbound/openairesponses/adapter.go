package openairesponses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/chatgptlogin"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/logger"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

const maxResponseBodyBytes = 40 << 20
const maxErrorBodyBytes = 1 << 20

func readBoundedResponse(body io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if int64(len(data)) > limit {
		return data[:limit:limit], true, err
	}
	return data, false, err
}

type Adapter struct {
	client       *http.Client
	idleTimeout  time.Duration
	retryPolicy  httpretry.Policy
	tokenFactory func(string) (tokenSource, error)
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
		client:       &http.Client{Transport: transport},
		idleTimeout:  idleTimeout,
		retryPolicy:  retryPolicy,
		tokenFactory: func(path string) (tokenSource, error) { return chatgptlogin.NewTokenManager(path, nil) },
	}
}

func newAdapterWithTokenManagerFactory(idleTimeout time.Duration, transport http.RoundTripper, retryPolicy httpretry.Policy, factory func(string) (tokenSource, error)) *Adapter {
	a := NewAdapterWithPolicy(idleTimeout, transport, retryPolicy)
	if factory != nil {
		a.tokenFactory = factory
	}
	return a
}

type tokenSource interface {
	Token(context.Context) (string, error)
}

func (a *Adapter) authorize(ctx context.Context, request *http.Request, endpoint config.Endpoint, model string) error {
	if endpoint.Auth == nil {
		return nil
	}
	fail := func(message string) error {
		return canonical.NewGatewayError("openai-responses", model, message, http.StatusServiceUnavailable, nil)
	}
	if endpoint.Auth.Type != "chatgpt" || strings.TrimSpace(endpoint.Auth.CredentialsFile) == "" {
		return fail("unsupported endpoint authentication configuration")
	}
	for name := range endpoint.Headers {
		if strings.EqualFold(name, "Authorization") {
			return fail("ChatGPT authentication conflicts with Authorization header")
		}
	}
	u := request.URL
	if u.Scheme != "https" || u.Host != "api.openai.com" || u.User != nil || u.Path != "/v1/responses" || u.RawQuery != "" || u.Fragment != "" {
		return fail("ChatGPT authentication is restricted to the official OpenAI Responses endpoint")
	}
	manager, err := a.tokenFactory(endpoint.Auth.CredentialsFile)
	if err != nil || manager == nil {
		return fail("could not initialize ChatGPT credentials")
	}
	token, err := manager.Token(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return fail("could not obtain ChatGPT access token")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (a *Adapter) doRequest(ctx context.Context, request *http.Request, endpoint config.Endpoint) (*http.Response, error) {
	client := a.client
	if endpoint.Auth != nil {
		copy := *client
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	return a.retryPolicy.Do(ctx, client, request)
}

func (a *Adapter) translateCanonicalRequest(req *canonical.CanonicalChatRequest, model string, stream bool, endpoint config.Endpoint) (ResponsesRequest, error) {
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
	if isAnthropicInbound(req) {
		if endpoint.Extensions["disable_encrypted_reasoning"] != true {
			outReq.Include = []string{"reasoning.encrypted_content"}
		}
		if outReq.Reasoning == nil && req.Params.ProviderSpecific["llmbrane.anthropic_adaptive"] == true {
			outReq.Reasoning = &Reasoning{}
		}
		if outReq.Reasoning != nil && outReq.Reasoning.Effort != "none" {
			outReq.Reasoning.Summary = "auto"
		}
		if value, present := req.Params.ProviderSpecific["llmbrane.anthropic_disable_parallel_tool_use"]; present {
			disabled, ok := value.(bool)
			if !ok {
				return outReq, fmt.Errorf("invalid Anthropic parallel tool setting")
			}
			parallel := !disabled
			outReq.ParallelToolCalls = &parallel
		}
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
			tool := Tool{
				Type:        "function",
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			}
			if isAnthropicInbound(req) {
				strict := false
				if value, present := t.Metadata["llmbrane.anthropic_strict"]; present {
					var ok bool
					strict, ok = value.(bool)
					if !ok {
						return outReq, fmt.Errorf("invalid Anthropic tool strict setting")
					}
				}
				tool.Strict = &strict
			}
			outReq.Tools = append(outReq.Tools, tool)
		}
	}

	// Tool choice
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "none", "auto":
			outReq.ToolChoice = req.ToolChoice.Type
		case "any":
			outReq.ToolChoice = "required"
		case "function", "tool":
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

	// Flush adjacent text and image parts before each separate item. This preserves
	// the order of reasoning, messages, calls, and results within a turn.
	for _, msg := range req.Messages {
		var contentParts []ContentPart
		var textContent string
		var hasImage bool
		flush := func() {
			if len(contentParts) == 0 {
				return
			}
			item := InputItem{Type: "message", Role: string(msg.Role)}
			if hasImage {
				item.Content = contentParts
			} else {
				item.Content = textContent
			}
			outReq.Input = append(outReq.Input, item)
			contentParts, textContent, hasImage = nil, "", false
		}
		for _, part := range msg.Content {
			switch part.Type {
			case "text":
				textContent += part.Text
				kind := "input_text"
				if msg.Role == canonical.RoleAssistant {
					kind = "output_text"
				}
				contentParts = append(contentParts, ContentPart{Type: kind, Text: part.Text})
			case "image":
				if part.Image != nil {
					hasImage = true
					contentParts = append(contentParts, ContentPart{Type: "input_image", ImageURL: part.Image.URL, Detail: part.Image.Detail})
				}
			case "reasoning":
				flush()
				if msg.Role != canonical.RoleAssistant {
					return outReq, fmt.Errorf("reasoning artifact requires an assistant message")
				}
				if part.Native == nil || part.Native.Provider != "openai" || part.Native.Format != "responses.item" {
					return outReq, fmt.Errorf("reasoning artifact is incompatible with OpenAI Responses")
				}
				raw, err := reasoningInput(part.Native.Data)
				if err != nil {
					return outReq, err
				}
				outReq.Input = append(outReq.Input, InputItem{Raw: raw})
			case "tool_call":
				flush()
				if part.ToolCall != nil {
					if isAnthropicInbound(req) {
						var arguments map[string]json.RawMessage
						if part.ToolCall.ID == "" || part.ToolCall.Name == "" || json.Unmarshal(part.ToolCall.Arguments, &arguments) != nil || arguments == nil {
							return outReq, fmt.Errorf("invalid tool call")
						}
					}
					outReq.Input = append(outReq.Input, InputItem{Type: "function_call", CallID: part.ToolCall.ID, Name: part.ToolCall.Name, Arguments: string(part.ToolCall.Arguments)})
				}
			case "tool_result":
				flush()
				if part.ToolResult != nil {
					if isAnthropicInbound(req) && part.ToolResult.ToolCallID == "" {
						return outReq, fmt.Errorf("tool result is missing its call ID")
					}
					var output string
					for _, result := range part.ToolResult.Content {
						if result.Type == "text" {
							output += result.Text
						} else if isAnthropicInbound(req) {
							return outReq, fmt.Errorf("unsupported tool result content %q", result.Type)
						}
					}
					if isAnthropicInbound(req) && part.ToolResult.IsError {
						output = "Tool execution failed:\n" + output
					}
					outReq.Input = append(outReq.Input, InputItem{Type: "function_call_output", CallID: part.ToolResult.ToolCallID, Output: output})
				}
			}
		}
		flush()
	}

	return outReq, nil
}

func isAnthropicInbound(req *canonical.CanonicalChatRequest) bool {
	return req.Params.ProviderSpecific["llmbrane.anthropic_inbound"] == true
}

func outputLimitIncomplete(details any) bool {
	m, ok := details.(map[string]any)
	return ok && m["reason"] == "max_output_tokens"
}

func emptyResponsesOutputMessage(tokenLimit bool) string {
	if tokenLimit {
		return "empty Responses output cannot be replayed: provider reached the output token limit"
	}
	return "empty Responses output cannot be replayed"
}

// reasoningInput adapts output metadata while preserving all native replay fields.
func reasoningInput(raw json.RawMessage) (json.RawMessage, error) {
	if _, err := canonical.EncodeReasoningEnvelope("openai", raw); err != nil {
		return nil, fmt.Errorf("invalid OpenAI reasoning item: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("invalid OpenAI reasoning item")
	}
	var kind, id, encrypted string
	_ = json.Unmarshal(fields["type"], &kind)
	_ = json.Unmarshal(fields["id"], &id)
	_ = json.Unmarshal(fields["encrypted_content"], &encrypted)
	if kind != "reasoning" || id == "" || encrypted == "" {
		return nil, fmt.Errorf("OpenAI reasoning item lacks replay data")
	}
	delete(fields, "status")
	return json.Marshal(fields)
}

func isChatGPTEndpoint(endpoint config.Endpoint) bool {
	return endpoint.Auth != nil && endpoint.Auth.Type == "chatgpt"
}

func chatGPTStreamingOnlyError(model string) error {
	return canonical.NewGatewayError("openai-responses", model, "ChatGPT authentication requires streaming responses", http.StatusBadRequest, nil)
}

func (a *Adapter) ProcessChat(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	if isChatGPTEndpoint(endpoint) {
		return nil, chatGPTStreamingOnlyError(model)
	}
	if req.Responses != nil {
		return a.processNative(ctx, req, model, endpoint)
	}
	outReq, err := a.translateCanonicalRequest(req, model, false, endpoint)
	if err != nil {
		return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadRequest, nil)
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
	reader := canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout)
	defer reader.Close()
	stopClosing := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stopClosing()
	limit := int64(maxResponseBodyBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limit = maxErrorBodyBytes
	}
	respBody, truncated, readErr := readBoundedResponse(reader, limit)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp ErrorResponse
		errMsg := "provider error response was incomplete or too large"
		if readErr == nil && !truncated && json.Unmarshal(respBody, &errResp) == nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		} else if readErr == nil && !truncated {
			errMsg = string(respBody)
		}
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("openai-responses", model, errMsg, resp.StatusCode, respBody), resp.Header)
	}
	if readErr != nil {
		return nil, canonical.NewGatewayError("openai-responses", model, "failed to read response body", http.StatusBadGateway, nil)
	}
	if truncated {
		return nil, canonical.NewGatewayError("openai-responses", model, "provider response body exceeds 40 MiB", http.StatusBadGateway, nil)
	}

	var outResp ResponsesResponse
	if err := json.Unmarshal(respBody, &outResp); err != nil {
		return nil, canonical.NewGatewayError("openai-responses", model, "failed to unmarshal response: "+err.Error(), http.StatusBadGateway, respBody)
	}

	if outResp.Error != nil && (isAnthropicInbound(req) || outResp.Error.Message != "") {
		message := outResp.Error.Message
		if message == "" {
			message = "provider response failed"
		}
		return nil, canonical.NewGatewayError("openai-responses", model, message, http.StatusBadGateway, respBody)
	}
	if isAnthropicInbound(req) && (outResp.ID == "" || (outResp.Status != "completed" && (outResp.Status != "incomplete" || !outputLimitIncomplete(outResp.IncompleteDetails)))) {
		return nil, canonical.NewGatewayError("openai-responses", model, "provider response failed or was interrupted", http.StatusBadGateway, respBody)
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

	var rawOutput struct {
		Output json.RawMessage `json:"output"`
	}
	var outputItems []json.RawMessage
	if err := json.Unmarshal(respBody, &rawOutput); err != nil || (isAnthropicInbound(req) && (len(rawOutput.Output) == 0 || string(rawOutput.Output) == "null")) || (len(rawOutput.Output) != 0 && json.Unmarshal(rawOutput.Output, &outputItems) != nil) {
		return nil, canonical.NewGatewayError("openai-responses", model, "invalid output", http.StatusBadGateway, respBody)
	}
	// Empty output would become an Anthropic assistant turn that replay rejects,
	// including when the provider stopped at its output token limit.
	if isAnthropicInbound(req) && len(outputItems) == 0 {
		return nil, canonical.NewGatewayError("openai-responses", model, emptyResponsesOutputMessage(outResp.Status == "incomplete" && outputLimitIncomplete(outResp.IncompleteDetails)), http.StatusBadGateway, respBody)
	}
	callIDs := make(map[string]bool)
	for itemIndex, item := range outResp.Output {
		if isAnthropicInbound(req) {
			if item.Type == "function_call" {
				if callIDs[item.CallID] {
					return nil, canonical.NewGatewayError("openai-responses", model, "duplicate function call ID", http.StatusBadGateway, outputItems[itemIndex])
				}
				callIDs[item.CallID] = true
			}
			if item.Status != "" && item.Status != "completed" && !(outResp.Status == "incomplete" && item.Status == "incomplete") {
				return nil, canonical.NewGatewayError("openai-responses", model, "provider returned unfinished output item", http.StatusBadGateway, outputItems[itemIndex])
			}
			if item.Status == "incomplete" && item.Type != "message" {
				return nil, canonical.NewGatewayError("openai-responses", model, "incomplete reasoning or tool call cannot be replayed", http.StatusBadGateway, outputItems[itemIndex])
			}
			if err := validateNativeOutputItem(outputItems[itemIndex], true); err != nil {
				return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadGateway, outputItems[itemIndex])
			}
		}
		switch item.Type {
		case "message":
			if isAnthropicInbound(req) {
				if err := validateTranslatedMessage(outputItems[itemIndex]); err != nil {
					return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadGateway, outputItems[itemIndex])
				}
			}
			for _, cp := range item.Content {
				if isAnthropicInbound(req) && cp.Type != "output_text" {
					return nil, canonical.NewGatewayError("openai-responses", model, "unsupported message content: "+cp.Type, http.StatusBadGateway, outputItems[itemIndex])
				}
				if cp.Type == "output_text" && cp.Text != "" {
					canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{Type: "text", Text: cp.Text})
				}
			}
		case "function_call":
			if isAnthropicInbound(req) {
				var arguments map[string]json.RawMessage
				if json.Unmarshal([]byte(item.Arguments), &arguments) != nil || arguments == nil {
					return nil, canonical.NewGatewayError("openai-responses", model, "invalid function call arguments", http.StatusBadGateway, outputItems[itemIndex])
				}
			}
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
			if isAnthropicInbound(req) {
				raw := outputItems[itemIndex]
				if _, err := reasoningInput(raw); err != nil {
					return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadGateway, raw)
				}
				var summary []string
				for _, s := range item.Summary {
					summary = append(summary, s.Text)
				}
				canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{
					Type: "reasoning", Reasoning: &canonical.CanonicalReasoning{Content: strings.Join(summary, "\n")},
					Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.item", Data: raw},
				})
			} else {
				for _, s := range item.Summary {
					if s.Text != "" {
						canonMsg.Content = append(canonMsg.Content, canonical.CanonicalContentPart{Type: "reasoning", Reasoning: &canonical.CanonicalReasoning{Content: s.Text}})
					}
				}
			}
		}
	}

	if outResp.Status == "incomplete" {
		finishReason = "length"
	}

	canonResp.Stop = &canonical.CanonicalStop{Reason: finishReason}
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
	if isChatGPTEndpoint(endpoint) && req.Responses != nil {
		reqCopy := *req
		reqCopy.Stream = true
		req = &reqCopy
	}
	if req.Responses != nil {
		return a.processNativeStream(ctx, req, model, endpoint)
	}
	outReq, err := a.translateCanonicalRequest(req, model, true, endpoint)
	if err != nil {
		return nil, canonical.NewGatewayError("openai-responses", model, err.Error(), http.StatusBadRequest, nil)
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
		respBody, _ := io.ReadAll(io.LimitReader(reader, 1024*1024))
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

	if isAnthropicInbound(req) || isChatGPTEndpoint(endpoint) {
		mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
			resp.Body.Close()
			return nil, canonical.NewGatewayError("openai-responses", model, "provider did not return an SSE stream", http.StatusBadGateway, nil)
		}
	}
	if isAnthropicInbound(req) {
		return a.anthropicStream(ctx, resp, model, isChatGPTEndpoint(endpoint)), nil
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
				if isChatGPTEndpoint(endpoint) {
					ch <- &canonical.CanonicalChatStreamEvent{Type: "error", Error: canonical.NewGatewayError("openai-responses", model, "provider stream ended before response.completed", http.StatusBadGateway, nil)}
				}
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
				if isChatGPTEndpoint(endpoint) && (event.Response == nil || event.Response.Status != "completed" || event.Response.Error != nil) {
					ch <- &canonical.CanonicalChatStreamEvent{Type: "error", Error: canonical.NewGatewayError("openai-responses", model, "invalid response.completed event", http.StatusBadGateway, nil)}
					return
				}
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
		} else if isChatGPTEndpoint(endpoint) && ctx.Err() == nil {
			ch <- &canonical.CanonicalChatStreamEvent{Type: "error", Error: canonical.NewGatewayError("openai-responses", model, "provider stream ended before response.completed", http.StatusBadGateway, nil)}
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

// validateTranslatedMessage checks fields required to construct Anthropic text blocks.
func validateTranslatedMessage(raw json.RawMessage) error {
	if err := validateNativeOutputItem(raw, true); err != nil {
		return err
	}
	var item struct {
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return fmt.Errorf("invalid message content")
	}
	if len(item.Content) == 0 {
		return fmt.Errorf("empty Responses message content cannot be replayed")
	}
	for _, rawPart := range item.Content {
		var part struct {
			Type        string            `json:"type"`
			Text        json.RawMessage   `json:"text"`
			Annotations []json.RawMessage `json:"annotations"`
		}
		if json.Unmarshal(rawPart, &part) != nil || part.Type != "output_text" || len(part.Text) == 0 || part.Text[0] != '"' || len(part.Annotations) != 0 {
			return fmt.Errorf("unsupported or invalid message content")
		}
		var text string
		if json.Unmarshal(part.Text, &text) != nil {
			return fmt.Errorf("invalid message text")
		}
		if text == "" {
			return fmt.Errorf("empty Responses text block cannot be replayed")
		}
	}
	return nil
}

// sameTerminalItem checks fields supplied by the terminal event against the
// completed item. Some compatible providers emit only type and ID there.
func sameTerminalItem(completed, terminal json.RawMessage) bool {
	var a, b map[string]json.RawMessage
	if json.Unmarshal(completed, &a) != nil || json.Unmarshal(terminal, &b) != nil {
		return false
	}
	for _, key := range []string{"type", "id", "role", "content", "call_id", "name", "arguments", "summary", "encrypted_content"} {
		value, present := b[key]
		if !present {
			continue
		}
		original, present := a[key]
		if !present {
			return false
		}
		var x, y any
		decode := func(raw json.RawMessage, dst *any) error {
			d := json.NewDecoder(bytes.NewReader(raw))
			d.UseNumber()
			return d.Decode(dst)
		}
		if decode(original, &x) != nil || decode(value, &y) != nil || !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// translatedItem queues later output items until earlier blocks have closed.
type translatedItem struct {
	kind, id            string
	completedRaw        json.RawMessage
	callID, name        string
	queuedBytes         int
	pending             []*canonical.CanonicalChatStreamEvent
	complete            bool
	incomplete          bool
	blockOpen           bool
	blockIndex          int
	contentIndex        int
	summaryParts        map[int]bool
	summaryText         map[int]*strings.Builder
	summaryEmittedIndex int
	summaryEmittedBytes int
	summaryBlocked      bool
	textParts           map[int]*strings.Builder
	textDone            map[int]bool
	initialArguments    string
	argumentsDone       bool
	arguments           strings.Builder
	argumentsEmitted    int
}

func (a *Adapter) anthropicStream(ctx context.Context, resp *http.Response, model string, requireCompleted ...bool) <-chan *canonical.CanonicalChatStreamEvent {
	strictCompletion := len(requireCompleted) > 0 && requireCompleted[0]
	ch := make(chan *canonical.CanonicalChatStreamEvent)
	go func() {
		defer close(ch)
		reader := canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout)
		defer reader.Close()
		stopClosing := context.AfterFunc(ctx, func() { _ = reader.Close() })
		defer stopClosing()
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
		items := make(map[int]*translatedItem)
		ids := make(map[string]int)
		callIDs := make(map[string]bool)
		nextItem, nextBlock, pendingBytes, bufferedBytes := 0, 0, 0, 0
		messageStarted, hasTool := false, false
		responseID := ""
		// Assign target indexes only when a queued block is emitted, not when a
		// parallel source item arrives.
		drain := func() bool {
			for {
				item := items[nextItem]
				if item == nil {
					return true
				}
				pendingBytes -= item.queuedBytes
				item.queuedBytes = 0
				for len(item.pending) > 0 {
					e := item.pending[0]
					item.pending = item.pending[1:]
					if e.Type == "content_start" || e.Type == "tool_call_start" {
						item.blockIndex = nextBlock
						nextBlock++
					}
					e.ContentIndex = item.blockIndex
					if e.ToolCall != nil {
						e.ToolCall.Index = item.blockIndex
					}
					if !send(e) {
						return false
					}
				}
				if !item.complete {
					return true
				}
				nextItem++
			}
		}
		queue := func(item *translatedItem, e *canonical.CanonicalChatStreamEvent) {
			item.pending = append(item.pending, e)
		}
		start := func(item *translatedItem, kind string) {
			block := json.RawMessage(`{"type":"text","text":""}`)
			if kind == "thinking" {
				block = json.RawMessage(`{"type":"thinking","thinking":"","signature":""}`)
			}
			queue(item, &canonical.CanonicalChatStreamEvent{Type: "content_start", Delta: &canonical.CanonicalContentPart{Type: kind, Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: kind, Data: block}}})
			item.blockOpen = true
		}
		stop := func(item *translatedItem) {
			if item.blockOpen {
				queue(item, &canonical.CanonicalChatStreamEvent{Type: "content_stop"})
				item.blockOpen = false
			}
		}
		delta := func(item *translatedItem, kind, value string) {
			if kind == "text" {
				queue(item, &canonical.CanonicalChatStreamEvent{Type: "content_delta", Delta: &canonical.CanonicalContentPart{Type: "text", Text: value}})
				return
			}
			field := "thinking"
			if kind == "signature_delta" {
				field = "signature"
			}
			raw, _ := json.Marshal(map[string]string{"type": kind, field: value})
			queue(item, &canonical.CanonicalChatStreamEvent{Type: "content_delta", Delta: &canonical.CanonicalContentPart{Type: "reasoning", Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: kind, Data: raw}}})
		}
		// Some providers send an initial prefix and then either the whole argument
		// string or just its suffix. Emit only the prefix common to both possibilities.
		streamArguments := func(item *translatedItem, complete string, final bool) error {
			if !item.blockOpen {
				return nil
			}
			value := complete
			if !final {
				prefix, fragments := item.initialArguments, item.arguments.String()
				switch {
				case prefix == "":
					value = fragments
				case strings.HasPrefix(prefix, fragments):
					value = prefix
				case strings.HasPrefix(fragments, prefix):
					i := len(prefix)
					for i < len(fragments) && fragments[i] == fragments[i-len(prefix)] {
						i++
					}
					for i > 0 && i < len(fragments) && !utf8.RuneStart(fragments[i]) {
						i--
					}
					value = fragments[:i]
				default:
					value = prefix + fragments
				}
			}
			if len(value) < item.argumentsEmitted {
				return fmt.Errorf("function call arguments do not match emitted prefix")
			}
			if len(value) > item.argumentsEmitted {
				queue(item, &canonical.CanonicalChatStreamEvent{Type: "tool_call_delta", ToolCall: &canonical.CanonicalToolCallDelta{ArgumentsDelta: value[item.argumentsEmitted:]}})
				item.argumentsEmitted = len(value)
			}
			return nil
		}
		var data []string
		var eventName string
		size := 0
		dispatch := func(payload []byte, name string) (bool, error) {
			var event StreamEvent
			var raw struct {
				Item        json.RawMessage `json:"item"`
				OutputIndex *int            `json:"output_index"`
				Response    struct {
					Output json.RawMessage `json:"output"`
				} `json:"response"`
			}
			if json.Unmarshal(payload, &event) != nil || json.Unmarshal(payload, &raw) != nil || !validEventType(event.Type) || (name != "" && name != event.Type) {
				return true, fmt.Errorf("invalid provider SSE event")
			}
			if event.Type == "response.output_text.delta" || event.Type == "response.reasoning_summary_text.delta" || event.Type == "response.function_call_arguments.delta" {
				var fields map[string]json.RawMessage
				if json.Unmarshal(payload, &fields) != nil || len(fields["delta"]) == 0 || fields["delta"][0] != '"' {
					return true, fmt.Errorf("invalid provider delta")
				}
			}
			switch event.Type {
			case "response.created":
				if messageStarted || event.Response == nil || event.Response.ID == "" {
					return true, fmt.Errorf("invalid response.created")
				}
				messageStarted = true
				responseID = event.Response.ID
				if !send(&canonical.CanonicalChatStreamEvent{Type: "message_start", ResponseID: event.Response.ID, Usage: nativeUsage(event.Response.Usage)}) {
					return true, nil
				}
			case "response.output_item.added":
				if !messageStarted || raw.OutputIndex == nil || *raw.OutputIndex < 0 || *raw.OutputIndex > nextItem+1024 || event.Item == nil || event.Item.ID == "" || items[*raw.OutputIndex] != nil {
					return true, fmt.Errorf("invalid output item start")
				}
				if _, ok := ids[event.Item.ID]; ok {
					return true, fmt.Errorf("duplicate output item ID")
				}
				item := &translatedItem{kind: event.Item.Type, id: event.Item.ID, summaryParts: make(map[int]bool), summaryText: make(map[int]*strings.Builder), textParts: make(map[int]*strings.Builder), textDone: make(map[int]bool)}
				items[*raw.OutputIndex], ids[item.id] = item, *raw.OutputIndex
				switch item.kind {
				case "reasoning":
					start(item, "thinking")
				case "message":
					if event.Item.Role != "" && event.Item.Role != "assistant" {
						return true, fmt.Errorf("invalid output message role")
					}
				case "function_call":
					if event.Item.CallID == "" || callIDs[event.Item.CallID] {
						return true, fmt.Errorf("function call has missing or duplicate call ID")
					}
					callIDs[event.Item.CallID] = true
					hasTool = true
					item.callID, item.name = event.Item.CallID, event.Item.Name
					item.initialArguments = event.Item.Arguments
					bufferedBytes += len(item.initialArguments)
					if bufferedBytes > 10*1024*1024 {
						return true, fmt.Errorf("buffered output exceeds limit")
					}
					if item.name != "" {
						item.blockOpen = true
						queue(item, &canonical.CanonicalChatStreamEvent{Type: "tool_call_start", ToolCall: &canonical.CanonicalToolCallDelta{ID: item.callID, Name: item.name}})
						if err := streamArguments(item, "", false); err != nil {
							return true, err
						}
					}
				default:
					return true, fmt.Errorf("unsupported output item type %q", item.kind)
				}
			case "response.output_item.done", "response.output_text.delta", "response.output_text.done", "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.function_call_arguments.done":
				if (event.Type == "response.content_part.added" || event.Type == "response.content_part.done") && event.Part != nil && len(event.Part.Annotations) != 0 {
					return true, fmt.Errorf("unsupported message annotations")
				}
				if raw.OutputIndex == nil || *raw.OutputIndex < 0 {
					return true, fmt.Errorf("missing output index")
				}
				item := items[*raw.OutputIndex]
				if item == nil || item.complete || (event.ItemID != "" && event.ItemID != item.id) {
					return true, fmt.Errorf("event refers to an unknown or completed output item")
				}
				switch event.Type {
				case "response.output_item.done":
					if event.Item == nil || event.Item.Type != item.kind || event.Item.ID != item.id || (event.Item.Status != "" && event.Item.Status != "completed" && event.Item.Status != "incomplete") {
						return true, fmt.Errorf("output item completion does not match its start")
					}
					if event.Item.Status == "incomplete" && item.kind != "message" {
						return true, fmt.Errorf("incomplete reasoning or tool call cannot be replayed")
					}
					if item.kind == "reasoning" {
						if _, err := reasoningInput(raw.Item); err != nil {
							return true, err
						}
						envelope, err := canonical.EncodeReasoningEnvelope("openai", raw.Item)
						if err != nil {
							return true, fmt.Errorf("cannot encode reasoning item: %w", err)
						}
						var summaries []string
						for index, summary := range event.Item.Summary {
							if builder := item.summaryText[index]; builder != nil && builder.String() != summary.Text {
								return true, fmt.Errorf("reasoning summary does not match deltas")
							}
							summaries = append(summaries, summary.Text)
						}
						for index := range item.summaryParts {
							if index >= len(event.Item.Summary) {
								return true, fmt.Errorf("reasoning summary part is missing")
							}
						}
						text := strings.Join(summaries, "\n")
						if item.summaryEmittedBytes > len(text) {
							return true, fmt.Errorf("reasoning summary does not match emitted text")
						}
						if remainder := text[item.summaryEmittedBytes:]; remainder != "" {
							delta(item, "thinking_delta", remainder)
						}
						for _, builder := range item.summaryText {
							bufferedBytes -= builder.Len()
						}
						item.summaryText = nil
						delta(item, "signature_delta", envelope)
					} else if item.kind == "message" {
						if err := validateTranslatedMessage(raw.Item); err != nil {
							return true, err
						}
						for index, part := range event.Item.Content {
							if part.Type != "output_text" {
								return true, fmt.Errorf("unsupported message content %q", part.Type)
							}
							builder := item.textParts[index]
							if builder == nil {
								if index < len(item.textParts) {
									return true, fmt.Errorf("message text parts are out of order")
								}
								if item.blockOpen {
									stop(item)
								}
								start(item, "text")
								if part.Text != "" {
									delta(item, "text", part.Text)
								}
								stop(item)
							} else if builder.String() != part.Text {
								return true, fmt.Errorf("message text does not match deltas")
							}
						}
						for index, builder := range item.textParts {
							if index >= len(event.Item.Content) {
								return true, fmt.Errorf("message content part is missing")
							}
							bufferedBytes -= builder.Len()
						}
						item.textParts = nil
					} else if item.kind == "function_call" {
						var arguments map[string]json.RawMessage
						if event.Item.CallID != item.callID || event.Item.Name == "" || (item.name != "" && event.Item.Name != item.name) || json.Unmarshal([]byte(event.Item.Arguments), &arguments) != nil || arguments == nil {
							return true, fmt.Errorf("invalid completed function call")
						}
						if !item.blockOpen {
							item.name = event.Item.Name
							item.blockOpen = true
							queue(item, &canonical.CanonicalChatStreamEvent{Type: "tool_call_start", ToolCall: &canonical.CanonicalToolCallDelta{ID: item.callID, Name: item.name}})
						}
						deltas := item.arguments.String()
						if deltas != "" && deltas != event.Item.Arguments && item.initialArguments+deltas != event.Item.Arguments {
							return true, fmt.Errorf("function call arguments do not match deltas")
						}
						if item.initialArguments != "" && !strings.HasPrefix(event.Item.Arguments, item.initialArguments) {
							return true, fmt.Errorf("initial function arguments do not match completion")
						}
						if err := streamArguments(item, event.Item.Arguments, true); err != nil {
							return true, err
						}
						bufferedBytes -= item.arguments.Len() + len(item.initialArguments)
						item.arguments.Reset()
						item.initialArguments = ""
					}
					stop(item)
					item.completedRaw = raw.Item
					item.incomplete = event.Item.Status == "incomplete"
					item.complete = true
				case "response.content_part.added":
					if item.kind != "message" || event.Part == nil || event.Part.Type != "output_text" {
						return true, fmt.Errorf("unsupported or overlapping message content")
					}
					if item.blockOpen {
						if item.contentIndex == event.ContentIndex && item.textParts[event.ContentIndex].Len() == 0 {
							break
						}
						return true, fmt.Errorf("unsupported or overlapping message content")
					}
					item.contentIndex = event.ContentIndex
					if event.ContentIndex != len(item.textParts) {
						return true, fmt.Errorf("message content parts are out of order")
					}
					item.textParts[event.ContentIndex] = &strings.Builder{}
					start(item, "text")
				case "response.output_text.delta":
					if item.kind != "message" || item.textDone[event.ContentIndex] {
						return true, fmt.Errorf("text delta for non-message or completed content")
					}
					if !item.blockOpen {
						if event.ContentIndex != len(item.textParts) {
							return true, fmt.Errorf("message content parts are out of order")
						}
						item.contentIndex = event.ContentIndex
						item.textParts[event.ContentIndex] = &strings.Builder{}
						start(item, "text")
					}
					if item.contentIndex != event.ContentIndex {
						return true, fmt.Errorf("overlapping message content")
					}
					bufferedBytes += len(event.Delta)
					if bufferedBytes > 10*1024*1024 {
						return true, fmt.Errorf("buffered output exceeds limit")
					}
					item.textParts[event.ContentIndex].WriteString(event.Delta)
					delta(item, "text", event.Delta)
				case "response.output_text.done":
					if item.kind != "message" {
						return true, fmt.Errorf("text completion for non-message item")
					}
					var fields map[string]json.RawMessage
					if json.Unmarshal(payload, &fields) != nil || len(fields["text"]) == 0 || fields["text"][0] != '"' {
						return true, fmt.Errorf("invalid text completion")
					}
					builder := item.textParts[event.ContentIndex]
					if builder == nil {
						if item.blockOpen || event.ContentIndex != len(item.textParts) {
							return true, fmt.Errorf("text completion out of order")
						}
						item.contentIndex = event.ContentIndex
						builder = &strings.Builder{}
						item.textParts[event.ContentIndex] = builder
						start(item, "text")
						bufferedBytes += len(event.Text)
						if bufferedBytes > 10*1024*1024 {
							return true, fmt.Errorf("buffered output exceeds limit")
						}
						builder.WriteString(event.Text)
						if event.Text != "" {
							delta(item, "text", event.Text)
						}
					} else if builder.String() != event.Text {
						return true, fmt.Errorf("text completion does not match deltas")
					}
					item.textDone[event.ContentIndex] = true
				case "response.content_part.done":
					if item.kind != "message" || (event.Part != nil && event.Part.Type != "output_text") {
						return true, fmt.Errorf("invalid message content completion")
					}
					if !item.blockOpen {
						if event.ContentIndex != len(item.textParts) {
							return true, fmt.Errorf("invalid message content completion")
						}
						item.contentIndex = event.ContentIndex
						item.textParts[event.ContentIndex] = &strings.Builder{}
						start(item, "text")
						if event.Part != nil {
							bufferedBytes += len(event.Part.Text)
							if bufferedBytes > 10*1024*1024 {
								return true, fmt.Errorf("buffered output exceeds limit")
							}
							item.textParts[event.ContentIndex].WriteString(event.Part.Text)
							if event.Part.Text != "" {
								delta(item, "text", event.Part.Text)
							}
						}
					}
					if item.contentIndex != event.ContentIndex || (event.Part != nil && item.textParts[event.ContentIndex].String() != event.Part.Text) {
						return true, fmt.Errorf("invalid message content completion")
					}
					if item.textParts[event.ContentIndex].Len() == 0 {
						return true, fmt.Errorf("empty Responses text block cannot be replayed")
					}
					stop(item)
				case "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta":
					if item.kind != "reasoning" || event.SummaryIndex < 0 {
						return true, fmt.Errorf("invalid reasoning summary")
					}
					if !item.summaryParts[event.SummaryIndex] {
						if event.SummaryIndex != len(item.summaryParts) {
							return true, fmt.Errorf("reasoning summary parts are out of order")
						}
						item.summaryParts[event.SummaryIndex] = true
					}
					if event.SummaryIndex > item.summaryEmittedIndex && !item.summaryBlocked {
						// A preceding part without deltas may still contain text in the
						// completed item, so later parts cannot be emitted yet.
						if item.summaryText[item.summaryEmittedIndex] == nil {
							item.summaryBlocked = true
						} else {
							delta(item, "thinking_delta", "\n")
							item.summaryEmittedBytes++
							item.summaryEmittedIndex = event.SummaryIndex
						}
					}
					if event.Type == "response.reasoning_summary_text.delta" {
						if event.SummaryIndex != len(item.summaryParts)-1 {
							return true, fmt.Errorf("reasoning summary deltas are out of order")
						}
						if item.summaryText[event.SummaryIndex] == nil {
							item.summaryText[event.SummaryIndex] = &strings.Builder{}
						}
						bufferedBytes += len(event.Delta)
						if bufferedBytes > 10*1024*1024 {
							return true, fmt.Errorf("buffered output exceeds limit")
						}
						item.summaryText[event.SummaryIndex].WriteString(event.Delta)
						if !item.summaryBlocked && event.Delta != "" {
							delta(item, "thinking_delta", event.Delta)
							item.summaryEmittedBytes += len(event.Delta)
						}
					}
				case "response.function_call_arguments.done":
					if item.kind != "function_call" {
						return true, fmt.Errorf("arguments completion for non-function item")
					}
					var fields map[string]json.RawMessage
					if json.Unmarshal(payload, &fields) != nil || len(fields["arguments"]) == 0 || fields["arguments"][0] != '"' {
						return true, fmt.Errorf("invalid arguments completion")
					}
					current := item.arguments.String()
					if current == "" {
						bufferedBytes += len(event.Arguments)
						if bufferedBytes > 10*1024*1024 {
							return true, fmt.Errorf("buffered output exceeds limit")
						}
						item.arguments.WriteString(event.Arguments)
					} else if current != event.Arguments && item.initialArguments+current != event.Arguments {
						return true, fmt.Errorf("arguments completion does not match deltas")
					}
					if item.initialArguments != "" && !strings.HasPrefix(event.Arguments, item.initialArguments) {
						return true, fmt.Errorf("initial function arguments do not match completion")
					}
					if err := streamArguments(item, event.Arguments, true); err != nil {
						return true, err
					}
					item.argumentsDone = true
				case "response.function_call_arguments.delta":
					if item.kind != "function_call" || item.argumentsDone {
						return true, fmt.Errorf("arguments delta for non-function or completed item")
					}
					bufferedBytes += len(event.Delta)
					if bufferedBytes > 10*1024*1024 {
						return true, fmt.Errorf("buffered output exceeds limit")
					}
					item.arguments.WriteString(event.Delta)
					if err := streamArguments(item, "", false); err != nil {
						return true, err
					}
				}
			case "response.completed", "response.incomplete":
				if !messageStarted || event.Response == nil || event.Response.ID != responseID || event.Response.Error != nil || (event.Type == "response.completed" && event.Response.Status != "completed") || (event.Type == "response.incomplete" && event.Response.Status != "incomplete") {
					return true, fmt.Errorf("invalid terminal response")
				}
				if event.Type == "response.incomplete" && (strictCompletion || !outputLimitIncomplete(event.Response.IncompleteDetails)) {
					return true, fmt.Errorf("provider response was incomplete")
				}
				var output []json.RawMessage
				if len(raw.Response.Output) == 0 || string(raw.Response.Output) == "null" || json.Unmarshal(raw.Response.Output, &output) != nil || len(output) != len(items) {
					return true, fmt.Errorf("terminal output does not match streamed items")
				}
				if len(output) == 0 {
					return true, fmt.Errorf("%s", emptyResponsesOutputMessage(event.Type == "response.incomplete"))
				}
				for index, result := range output {
					item := items[index]
					var identity struct {
						ID   string `json:"id"`
						Type string `json:"type"`
					}
					if item == nil || !item.complete || json.Unmarshal(result, &identity) != nil || identity.ID != item.id || identity.Type != item.kind || !sameTerminalItem(item.completedRaw, result) {
						return true, fmt.Errorf("terminal output item does not match stream")
					}
				}
				for index, item := range items {
					if !item.complete || index >= nextItem || (item.incomplete && event.Type != "response.incomplete") {
						return true, fmt.Errorf("response completed with unfinished output items")
					}
				}
				reason := "stop"
				if hasTool {
					reason = "tool_calls"
				}
				if event.Type == "response.incomplete" {
					reason = "length"
				}
				send(&canonical.CanonicalChatStreamEvent{Type: "done", Stop: &canonical.CanonicalStop{Reason: reason}, Usage: nativeUsage(event.Response.Usage)})
				return true, nil
			case "response.failed", "error":
				message := event.Message
				if event.Response != nil && event.Response.Error != nil {
					message = event.Response.Error.Message
				}
				if message == "" {
					message = "provider stream failed"
				}
				return true, fmt.Errorf("%s", message)
			default:
				if strings.HasPrefix(event.Type, "response.refusal.") || strings.HasPrefix(event.Type, "response.output_audio.") || strings.HasPrefix(event.Type, "response.audio.") {
					return true, fmt.Errorf("unsupported provider event %q", event.Type)
				}
			}
			if event.Type == "response.output_item.added" || event.Type == "response.output_item.done" || event.Type == "response.output_text.delta" || event.Type == "response.output_text.done" || event.Type == "response.content_part.added" || event.Type == "response.content_part.done" || event.Type == "response.reasoning_summary_part.added" || event.Type == "response.reasoning_summary_text.delta" || event.Type == "response.function_call_arguments.delta" || event.Type == "response.function_call_arguments.done" {
				if raw.OutputIndex != nil && *raw.OutputIndex > nextItem {
					item := items[*raw.OutputIndex]
					if item != nil {
						item.queuedBytes += len(payload)
						pendingBytes += len(payload)
						if pendingBytes > 10*1024*1024 {
							return true, fmt.Errorf("interleaved output exceeds buffer limit")
						}
					}
				}
			}
			if !drain() {
				return true, nil
			}
			return false, nil
		}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			if line == "" {
				if len(data) != 0 {
					terminal, err := dispatch([]byte(strings.Join(data, "\n")), eventName)
					data, eventName, size = nil, "", 0
					if err != nil {
						fail(err.Error())
						return
					}
					if terminal {
						return
					}
				} else {
					eventName = ""
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			name, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			if name == "event" {
				eventName = value
			} else if name == "data" {
				size += len(value) + 1
				if size > 10*1024*1024 {
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
		} else {
			fail("provider stream ended before a terminal event")
		}
	}()
	return ch
}
