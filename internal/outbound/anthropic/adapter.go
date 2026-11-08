package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mayer-tech/llmbrane/internal/anthropicparams"
	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

const maxResponseBodyBytes = 40 << 20

// readResponseBody bounds buffered responses and reports whether they were truncated.
func readResponseBody(body io.Reader) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBodyBytes+1))
	if len(data) > maxResponseBodyBytes {
		return data[:maxResponseBodyBytes:maxResponseBodyBytes], true, err
	}
	return data, false, err
}

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
	reader := canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout)
	defer reader.Close()
	stopClosing := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stopClosing()
	respBody, truncated, readErr := readResponseBody(reader)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp ErrorResponse
		errMsg := "provider error response was incomplete or too large"
		if readErr == nil && !truncated && json.Unmarshal(respBody, &errResp) == nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		} else if readErr == nil && !truncated {
			errMsg = string(respBody)
		}
		return nil, httpretry.ApplyHeaders(canonical.NewGatewayError("anthropic", model, errMsg, resp.StatusCode, respBody), resp.Header)
	}

	if readErr != nil {
		return nil, canonical.NewGatewayError("anthropic", model, "failed to read provider response body", http.StatusBadGateway, nil)
	}
	if truncated {
		return nil, canonical.NewGatewayError("anthropic", model, fmt.Sprintf("response body exceeds %d bytes", maxResponseBodyBytes), http.StatusBadGateway, nil)
	}

	var outResp MessagesResponse
	var envelope struct {
		Content json.RawMessage `json:"content"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(respBody, &outResp) != nil || json.Unmarshal(respBody, &envelope) != nil {
		return nil, canonical.NewGatewayError("anthropic", model, "invalid provider response", http.StatusBadGateway, nil)
	}
	if outResp.Type == "error" || (len(envelope.Error) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null"))) {
		var errorResponse ErrorResponse
		message := "provider returned an error response"
		if json.Unmarshal(respBody, &errorResponse) == nil && errorResponse.Error.Message != "" {
			message = errorResponse.Error.Message
		}
		return nil, canonical.NewGatewayError("anthropic", model, message, http.StatusBadGateway, nil)
	}
	if outResp.Type != "message" || outResp.Role != "assistant" || outResp.ID == "" || outResp.Model == "" || len(bytes.TrimSpace(envelope.Content)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Content), []byte("null")) {
		return nil, canonical.NewGatewayError("anthropic", model, "invalid provider message", http.StatusBadGateway, nil)
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
		} else if reason == "max_tokens" {
			reason = "length"
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
	mode, eff, budget, err := anthropicparams.Thinking(req.Params.ReasoningEffort, endpoint.Extensions)
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := err.(*anthropicparams.ConfigError); ok {
			status = http.StatusInternalServerError
		}
		return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, err.Error(), status, nil)
	}
	adaptive := false
	if value, present := req.Params.ProviderSpecific["llmbrane.anthropic_adaptive"]; present {
		var ok bool
		adaptive, ok = value.(bool)
		if !ok {
			return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, "invalid Anthropic adaptive thinking setting", http.StatusBadRequest, nil)
		}
	}
	var display string
	if value, present := req.Params.ProviderSpecific["llmbrane.anthropic_thinking_display"]; present {
		var ok bool
		display, ok = value.(string)
		if !ok || (display != "summarized" && display != "omitted") {
			return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, "thinking.display must be summarized or omitted", http.StatusBadRequest, nil)
		}
	}
	if adaptive && mode != "adaptive" {
		return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, "adaptive thinking conflicts with endpoint thinking_mode: budget", http.StatusBadRequest, nil)
	}
	if adaptive && eff == "none" {
		return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, "adaptive thinking conflicts with effective reasoning effort: none", http.StatusBadRequest, nil)
	}
	if eff != "" {
		if eff == "none" {
			thinking = &ThinkingConfig{Type: "disabled"}
		} else if mode == "budget" {
			thinking = &ThinkingConfig{Type: "enabled", BudgetTokens: budget}
		} else {
			if eff == "minimal" {
				eff = "low"
			}
			thinking = &ThinkingConfig{Type: "adaptive"}
			outputConfig = &OutputConfig{Effort: eff}
		}
	} else if adaptive {
		thinking = &ThinkingConfig{Type: "adaptive"}
	}
	if display != "" {
		if thinking == nil || thinking.Type == "disabled" {
			return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, "thinking.display requires active thinking after endpoint mapping", http.StatusBadRequest, nil)
		}
		thinking.Display = display
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

	// Apply the endpoint's explicit compatibility policy without modifying the canonical request.
	omitParam := func(param string) {
		switch param {
		case "temperature":
			outReq.Temperature = nil
		case "top_p":
			outReq.TopP = nil
		}
	}
	switch params := endpoint.Extensions["omit_params"].(type) {
	case []string:
		for _, param := range params {
			omitParam(param)
		}
	case []any:
		for _, param := range params {
			if name, ok := param.(string); ok {
				omitParam(name)
			}
		}
	}

	maxTokens, _, err := anthropicparams.MaxOutputTokens(req.Params.MaxOutputTokens, nil, budget)
	if err != nil {
		return MessagesRequest{}, canonical.NewGatewayError("anthropic", model, err.Error(), http.StatusBadRequest, nil)
	}
	outReq.MaxTokens = maxTokens

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
		respBody, _, _ := readResponseBody(resp.Body)
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
	go a.readChatStream(ctx, resp.Body, model, eventChan)
	return eventChan, nil
}
