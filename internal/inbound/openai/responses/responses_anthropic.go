package responses

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

// prepareResponsesRoute translates replay items only after the backend is known.
func prepareResponsesRoute(req *canonical.CanonicalChatRequest, shape string) error {
	if shape == "anthropic-messages" {
		if req.ResponseFormat != nil {
			return fmt.Errorf("structured Responses output is not supported by the Anthropic route")
		}
		if req.ToolChoice != nil && req.ToolChoice.Type == "required" {
			return fmt.Errorf("tool_choice %q is not supported by the Anthropic route", req.ToolChoice.Type)
		}
		if raw, ok := req.Responses.Settings["parallel_tool_calls"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
			return fmt.Errorf("parallel_tool_calls=false is not supported by the Anthropic route")
		}
		if raw, ok := req.Responses.Settings["truncation"]; ok {
			var value string
			_ = json.Unmarshal(raw, &value)
			if value == "auto" {
				return fmt.Errorf("truncation=auto is not supported by the Anthropic route")
			}
		}
		if err := validateAnthropicResponsesSettings(req); err != nil {
			return err
		}
	}
	seenConversation := false
	for i := range req.Messages {
		msg := &req.Messages[i]
		if len(msg.Content) == 0 || msg.Content[len(msg.Content)-1].Native == nil {
			return fmt.Errorf("input item %d has no native item", i)
		}
		native := msg.Content[len(msg.Content)-1].Native
		if shape == "openai-responses" {
			// Preserve native items verbatim unless they contain a reserved envelope.
			var fields map[string]json.RawMessage
			var kind, value string
			if json.Unmarshal(native.Data, &fields) != nil {
				return fmt.Errorf("input item %d: invalid item", i)
			}
			_ = json.Unmarshal(fields["type"], &kind)
			_ = json.Unmarshal(fields["encrypted_content"], &value)
			if kind != "reasoning" || !canonical.IsReasoningEnvelope(value) {
				continue
			}
			decoded, err := canonical.DecodeReasoningEnvelope(value)
			if err != nil {
				return fmt.Errorf("input item %d: invalid reasoning envelope: %w", i, err)
			}
			if decoded == nil || decoded.Provider != "openai" || decoded.Format != "responses.item" {
				return fmt.Errorf("input item %d: reasoning artifact is incompatible with Responses backend", i)
			}
			var original map[string]json.RawMessage
			var ciphertext string
			if json.Unmarshal(decoded.Data, &original) != nil || json.Unmarshal(original["encrypted_content"], &ciphertext) != nil || canonical.IsReasoningEnvelope(ciphertext) {
				return fmt.Errorf("input item %d: invalid native Responses ciphertext", i)
			}
			delete(original, "status")
			native.Data, _ = json.Marshal(original)
			continue
		}
		if msg.Role != canonical.RoleSystem && msg.Role != "developer" {
			seenConversation = true
		}
		var item struct {
			Type      string  `json:"type"`
			Encrypted string  `json:"encrypted_content"`
			CallID    string  `json:"call_id"`
			Name      string  `json:"name"`
			Arguments *string `json:"arguments"`
			Output    string  `json:"output"`
		}
		if err := json.Unmarshal(native.Data, &item); err != nil {
			return fmt.Errorf("input item %d: invalid item", i)
		}
		if err := validateAnthropicInputItem(native.Data, item.Type); err != nil {
			return fmt.Errorf("input item %d: %w", i, err)
		}
		if item.Type == "reasoning" && canonical.IsReasoningEnvelope(item.Encrypted) {
			decoded, err := canonical.DecodeReasoningEnvelope(item.Encrypted)
			if err != nil {
				return fmt.Errorf("input item %d: invalid reasoning envelope: %w", i, err)
			}
			if err := validateAnthropicReasoning(decoded); err != nil {
				return fmt.Errorf("input item %d: %w", i, err)
			}
			var block struct {
				Thinking string `json:"thinking"`
			}
			_ = json.Unmarshal(decoded.Data, &block)
			msg.Content = []canonical.CanonicalContentPart{{Type: "reasoning", Reasoning: &canonical.CanonicalReasoning{Content: block.Thinking}, Native: decoded}}
			continue
		}
		switch item.Type {
		case "reasoning":
			return fmt.Errorf("input item %d: Anthropic reasoning replay requires a compatible gateway envelope", i)
		case "function_call":
			if item.Arguments == nil || !json.Valid([]byte(*item.Arguments)) {
				return fmt.Errorf("input item %d: function arguments must be valid JSON", i)
			}
			var object map[string]json.RawMessage
			if json.Unmarshal([]byte(*item.Arguments), &object) != nil || object == nil {
				return fmt.Errorf("input item %d: function arguments must be a JSON object", i)
			}
			msg.Content = []canonical.CanonicalContentPart{{Type: "tool_call", ToolCall: &canonical.CanonicalToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(*item.Arguments)}}}
		case "function_call_output":
			msg.Content = []canonical.CanonicalContentPart{{Type: "tool_result", ToolResult: &canonical.CanonicalToolResult{ToolCallID: item.CallID, Content: []canonical.CanonicalContentPart{{Type: "text", Text: item.Output}}}}}
		default:
			var check struct {
				Content []struct {
					Type    string `json:"type"`
					Refusal string `json:"refusal"`
				} `json:"content"`
			}
			content := msg.Content[:len(msg.Content)-1]
			if json.Unmarshal(native.Data, &check) == nil && check.Content != nil {
				ordered := make([]canonical.CanonicalContentPart, 0, len(check.Content))
				index := 0
				for _, part := range check.Content {
					if part.Type == "refusal" {
						ordered = append(ordered, canonical.CanonicalContentPart{Type: "text", Text: part.Refusal})
					} else {
						if index >= len(content) {
							return fmt.Errorf("input item %d: inconsistent message content", i)
						}
						ordered = append(ordered, content[index])
						index++
					}
				}
				if index != len(content) {
					return fmt.Errorf("input item %d: inconsistent message content", i)
				}
				content = ordered
			}
			msg.Content = content
			if len(msg.Content) == 0 {
				return fmt.Errorf("input item %d: empty message content is not supported", i)
			}
			for _, part := range msg.Content {
				if part.Type == "text" && part.Text == "" && msg.Role != canonical.RoleSystem && msg.Role != "developer" {
					return fmt.Errorf("input item %d: empty text is not supported by the Anthropic route", i)
				}
				if part.Image != nil {
					if part.Image.Detail != "" && part.Image.Detail != "auto" {
						return fmt.Errorf("input item %d: image detail %q is not supported by the Anthropic route", i, part.Image.Detail)
					}
					if strings.HasPrefix(part.Image.URL, "data:") {
						header, data, ok := strings.Cut(part.Image.URL, ";base64,")
						if !ok || !strings.HasPrefix(header, "data:image/") || strings.Contains(header, ";") || data == "" {
							return fmt.Errorf("input item %d: unsupported image data URL", i)
						}
						if len(data) > 32<<20 {
							return fmt.Errorf("input item %d: image data exceeds size limit", i)
						}
						if _, err := base64.StdEncoding.DecodeString(data); err != nil {
							return fmt.Errorf("input item %d: invalid image base64 data", i)
						}
					}
				}
			}
			if msg.Role == canonical.RoleSystem || msg.Role == "developer" {
				if seenConversation {
					return fmt.Errorf("input item %d: system and developer messages must precede conversation messages on the Anthropic route", i)
				}
				var text strings.Builder
				for _, part := range msg.Content {
					if part.Type != "text" {
						return fmt.Errorf("input item %d: system content must be text", i)
					}
					text.WriteString(part.Text)
				}
				if len(req.System) > 0 {
					req.System = append(req.System, canonical.CanonicalContentPart{Type: "text", Text: "\n\n"})
				}
				req.System = append(req.System, canonical.CanonicalContentPart{Type: "text", Text: text.String()})
				msg.Role = canonical.RoleSystem
			}
		}
		if msg.Role != canonical.RoleSystem {
			seenConversation = true
		}
	}
	return nil
}

func validateAnthropicInputItem(raw json.RawMessage, kind string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return fmt.Errorf("expected an item object")
	}
	if rawStatus, ok := fields["status"]; ok {
		var status string
		if json.Unmarshal(rawStatus, &status) != nil || status != "completed" {
			return fmt.Errorf("only completed input items can be replayed on the Anthropic route")
		}
	}
	allowed := map[string]bool{"type": true}
	switch kind {
	case "", "message":
		for _, key := range []string{"role", "content", "id", "status"} {
			allowed[key] = true
		}
		var parts []map[string]json.RawMessage
		if len(fields["content"]) > 0 && bytes.TrimSpace(fields["content"])[0] == '[' {
			if json.Unmarshal(fields["content"], &parts) != nil {
				return fmt.Errorf("invalid message content")
			}
			for _, part := range parts {
				var partType string
				_ = json.Unmarshal(part["type"], &partType)
				if partType == "input_text" || partType == "output_text" {
					var text *string
					if json.Unmarshal(part["text"], &text) != nil || text == nil {
						return fmt.Errorf("message text must be a string")
					}
				}
				if partType == "refusal" {
					var role, refusal string
					if json.Unmarshal(fields["role"], &role) != nil || role != "assistant" || json.Unmarshal(part["refusal"], &refusal) != nil || strings.TrimSpace(refusal) == "" {
						return fmt.Errorf("refusal requires an assistant role and non-empty refusal text")
					}
				}
				for key, value := range part {
					switch {
					case key == "type", key == "refusal" && partType == "refusal", key == "text" && (partType == "input_text" || partType == "output_text"), key == "image_url" && partType == "input_image", key == "detail" && partType == "input_image":
					case key == "annotations" && partType == "output_text":
						var annotations []json.RawMessage
						if json.Unmarshal(value, &annotations) != nil || len(annotations) != 0 {
							return fmt.Errorf("output text annotations are not supported")
						}
					case key == "logprobs" && partType == "output_text":
						var logprobs []json.RawMessage
						if json.Unmarshal(value, &logprobs) != nil || len(logprobs) != 0 {
							return fmt.Errorf("output text logprobs are not supported")
						}
					default:
						return fmt.Errorf("message content field %q is not supported by the Anthropic route", key)
					}
				}
			}
		}
	case "reasoning":
		for _, key := range []string{"id", "status", "summary", "encrypted_content"} {
			allowed[key] = true
		}
	case "function_call":
		for _, key := range []string{"id", "status", "call_id", "name", "arguments"} {
			allowed[key] = true
		}
	case "function_call_output":
		for _, key := range []string{"id", "status", "call_id", "output"} {
			allowed[key] = true
		}
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("item field %q is not supported by the Anthropic route", key)
		}
	}
	return nil
}

func validateAnthropicResponsesSettings(req *canonical.CanonicalChatRequest) error {
	for _, key := range []string{"reasoning", "text"} {
		raw, ok := req.Responses.Settings[key]
		if !ok {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return fmt.Errorf("%s must be an object", key)
		}
		for name, value := range fields {
			if key == "reasoning" && name == "effort" {
				continue
			}
			if key == "text" && name == "format" {
				var format map[string]json.RawMessage
				if json.Unmarshal(value, &format) != nil || format == nil {
					return fmt.Errorf("text.format must be an object")
				}
				for field := range format {
					if field != "type" {
						return fmt.Errorf("text.format.%s is not supported by the Anthropic route", field)
					}
				}
				continue
			}
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			return fmt.Errorf("%s.%s is not supported by the Anthropic route", key, name)
		}
	}
	if raw, ok := req.Responses.Settings["metadata"]; ok {
		var metadata map[string]string
		if json.Unmarshal(raw, &metadata) != nil || len(metadata) > 0 {
			return fmt.Errorf("metadata is not supported by the Anthropic route")
		}
	}
	if raw, ok := req.Responses.Settings["tools"]; ok {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tools); err != nil {
			return fmt.Errorf("invalid tools")
		}
		for i, tool := range tools {
			for key, value := range tool {
				switch key {
				case "type", "name", "description", "parameters":
				case "strict":
					if !bytes.Equal(bytes.TrimSpace(value), []byte("false")) && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
						return fmt.Errorf("tools[%d].strict is not supported by the Anthropic route", i)
					}
				default:
					return fmt.Errorf("tools[%d].%s is not supported by the Anthropic route", i, key)
				}
			}
			if req.Tools[i].InputSchema == nil {
				req.Tools[i].InputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
		}
	}
	if req.ToolChoice != nil {
		if len(req.Tools) == 0 {
			if req.ToolChoice.Type != "auto" && req.ToolChoice.Type != "none" {
				return fmt.Errorf("tool_choice requires tools")
			}
			if req.ToolChoice.Type == "auto" {
				req.ToolChoice = nil
			}
		} else if req.ToolChoice.Type == "function" {
			found := false
			for _, tool := range req.Tools {
				if tool.Name == req.ToolChoice.Name {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("tool_choice names an undeclared tool")
			}
		}
	}
	return nil
}

func newResponsesID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate response ID: %w", err)
	}
	return "resp_gw_" + hex.EncodeToString(id[:]), nil
}

func validateAnthropicReasoning(native *canonical.CanonicalProviderData) error {
	if native == nil || native.Provider != "anthropic" || (native.Format != "thinking" && native.Format != "redacted_thinking") {
		return fmt.Errorf("reasoning artifact is incompatible with Anthropic backend")
	}
	var block struct {
		Type      string  `json:"type"`
		Thinking  *string `json:"thinking"`
		Signature string  `json:"signature"`
		Data      string  `json:"data"`
	}
	if json.Unmarshal(native.Data, &block) != nil || block.Type != native.Format ||
		(block.Type == "thinking" && (block.Thinking == nil || block.Signature == "" || canonical.IsReasoningEnvelope(block.Signature))) ||
		(block.Type == "redacted_thinking" && (block.Data == "" || canonical.IsReasoningEnvelope(block.Data))) {
		return fmt.Errorf("Anthropic reasoning artifact is missing or has invalid replay data")
	}
	return nil
}

func responsesUsage(usage *canonical.CanonicalUsage) any {
	if usage == nil {
		return nil
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	return map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "total_tokens": total,
		"input_tokens_details":  map[string]any{"cached_tokens": usage.CachedInputTokens},
		"output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens}}
}

func responsesItem(part canonical.CanonicalContentPart, prefix string, index int) (map[string]any, error) {
	switch part.Type {
	case "reasoning":
		if err := validateAnthropicReasoning(part.Native); err != nil {
			return nil, err
		}
		envelope, err := canonical.EncodeReasoningEnvelope("anthropic", part.Native.Data)
		if err != nil {
			return nil, err
		}
		var block struct {
			Thinking string `json:"thinking"`
		}
		_ = json.Unmarshal(part.Native.Data, &block)
		summary := []any{}
		if block.Thinking != "" {
			summary = append(summary, map[string]any{"type": "summary_text", "text": block.Thinking})
		}
		return map[string]any{"type": "reasoning", "id": fmt.Sprintf("rs_gw_%s_%d", prefix, index), "status": "completed", "summary": summary, "encrypted_content": envelope}, nil
	case "text":
		if part.Text == "" {
			return nil, fmt.Errorf("empty Anthropic text block cannot be replayed")
		}
		return map[string]any{"type": "message", "id": fmt.Sprintf("msg_gw_%s_%d", prefix, index), "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": part.Text, "annotations": []any{}, "logprobs": []any{}}}}, nil
	case "tool_call":
		if part.ToolCall == nil || part.ToolCall.ID == "" || part.ToolCall.Name == "" {
			return nil, fmt.Errorf("invalid tool call")
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(part.ToolCall.Arguments, &object) != nil || object == nil {
			return nil, fmt.Errorf("invalid tool call arguments")
		}
		return map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_gw_%s_%d", prefix, index), "call_id": part.ToolCall.ID, "name": part.ToolCall.Name, "arguments": string(part.ToolCall.Arguments), "status": "completed"}, nil
	default:
		return nil, fmt.Errorf("unsupported Anthropic output block %q", part.Type)
	}
}

func responsesFromCanonical(response *canonical.CanonicalChatResponse, model string) (map[string]any, error) {
	if response == nil || len(response.Choices) != 1 || response.ID == "" {
		return nil, fmt.Errorf("invalid Anthropic response")
	}
	// Even a token-limit stop must not produce an assistant turn that cannot be replayed.
	if len(response.Choices[0].Message.Content) == 0 {
		reason := response.Choices[0].FinishReason
		if response.Stop != nil {
			reason = response.Stop.Reason
		}
		return nil, emptyAnthropicOutputError(reason)
	}
	// The Anthropic adapter retains the raw response but skips unknown block types.
	// Refuse partial translations rather than silently losing provider content.
	if len(response.Raw) > 0 {
		var raw struct {
			Content json.RawMessage `json:"content"`
		}
		var blocks []json.RawMessage
		if json.Unmarshal(response.Raw, &raw) != nil || len(raw.Content) == 0 || bytes.TrimSpace(raw.Content)[0] != '[' || json.Unmarshal(raw.Content, &blocks) != nil || len(blocks) != len(response.Choices[0].Message.Content) {
			return nil, fmt.Errorf("Anthropic response contains unsupported or invalid content")
		}
		for _, block := range blocks {
			var fields struct {
				Type string  `json:"type"`
				Text *string `json:"text"`
			}
			if json.Unmarshal(block, &fields) != nil || (fields.Type == "text" && fields.Text == nil) {
				return nil, fmt.Errorf("Anthropic response contains an invalid content block")
			}
		}
	}
	reason := response.Choices[0].FinishReason
	if response.Stop != nil {
		reason = response.Stop.Reason
	}
	if reason == "refusal" {
		parts := response.Choices[0].Message.Content
		if len(parts) != 1 || parts[0].Type != "text" || strings.TrimSpace(parts[0].Text) == "" {
			return nil, fmt.Errorf("Anthropic refusal must contain exactly one non-empty text block")
		}
	}
	output := []any{}
	id, err := newResponsesID()
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimPrefix(id, "resp_gw_")
	callIDs := make(map[string]bool)
	for i, part := range response.Choices[0].Message.Content {
		if part.Type == "tool_call" && part.ToolCall != nil {
			if callIDs[part.ToolCall.ID] {
				return nil, fmt.Errorf("duplicate Anthropic tool call ID")
			}
			callIDs[part.ToolCall.ID] = true
		}
		item, err := responsesItem(part, prefix, i)
		if err != nil {
			return nil, err
		}
		if reason == "refusal" {
			item["content"] = []any{map[string]any{"type": "refusal", "refusal": part.Text}}
		}
		output = append(output, item)
	}
	status, incomplete, err := anthropicResponsesStatus(reason)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "model": model, "status": status, "output": output, "usage": responsesUsage(response.Usage), "error": nil, "incomplete_details": incomplete}, nil
}

func emptyAnthropicOutputError(reason string) error {
	if reason == "length" || reason == "max_tokens" {
		return fmt.Errorf("empty Anthropic output cannot be replayed: provider reached the output token limit")
	}
	return fmt.Errorf("empty Anthropic output cannot be replayed")
}

func anthropicResponsesStatus(reason string) (string, any, error) {
	switch reason {
	case "end_turn", "stop_sequence", "tool_calls", "tool_use", "refusal":
		return "completed", nil, nil
	case "length", "max_tokens":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}, nil
	default:
		return "", nil, fmt.Errorf("unsupported Anthropic stop reason %q", reason)
	}
}

// The Anthropic adapter emits done only after a complete message_stop.
func streamAnthropicResponses(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, stream <-chan *canonical.CanonicalChatStreamEvent, model string) {
	s := newResponsesStream(w, flusher, model)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-stream:
			if ctx.Err() != nil {
				return
			}
			if !ok {
				s.fail(fmt.Errorf("Provider stream ended before completion"))
				return
			}
			if event == nil {
				continue
			}
			finished, err := s.accept(event)
			if err != nil {
				s.fail(err)
				return
			}
			if finished {
				return
			}
		}
	}
}
