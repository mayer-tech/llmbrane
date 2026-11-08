package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

// readChatStream forwards only complete, structurally valid provider events.
// It does not synthesize a successful terminal event for a truncated stream.
func (a *Adapter) readChatStream(ctx context.Context, body io.ReadCloser, model string, events chan<- *canonical.CanonicalChatStreamEvent) {
	defer close(events)
	reader := canonical.NewIdleTimeoutReader(body, a.idleTimeout)
	defer reader.Close()
	stopClosing := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stopClosing()

	var currentRaw []byte
	send := func(event *canonical.CanonicalChatStreamEvent) bool {
		if event.Raw == nil {
			event.Raw = currentRaw
		}
		select {
		case <-ctx.Done():
			return false
		case events <- event:
			return true
		}
	}
	fail := func(message string) {
		send(&canonical.CanonicalChatStreamEvent{Type: "error", Error: canonical.NewGatewayError("anthropic", model, message, http.StatusBadGateway, currentRaw)})
	}

	openBlocks := make(map[int]string)
	seenBlocks := make(map[int]bool)
	toolIndices := make(map[int]int)
	nextToolIndex := 0
	started := false
	var stop *canonical.CanonicalStop
	var usage *canonical.CanonicalUsage
	var eventName string
	var dataLines []string
	var dataSize int

	// SSE events can span multiple data lines; limit both lines and frames.
	const maxEventSize = 4 << 20
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxEventSize)
	dispatch := func() bool {
		if len(dataLines) == 0 {
			eventName = ""
			return true
		}
		data := strings.Join(dataLines, "\n")
		currentRaw = []byte(data)
		dataLines = nil
		dataSize = 0
		name := eventName
		eventName = ""
		if data == "[DONE]" {
			fail("anthropic stream ended before message_stop")
			return false
		}
		var event StreamEvent
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			fail(fmt.Sprintf("failed to parse anthropic stream event: %v", err))
			return false
		}
		if err := json.Unmarshal([]byte(data), &fields); err != nil || fields == nil || event.Type == "" || (name != "" && name != event.Type) {
			fail("invalid anthropic stream event type")
			return false
		}
		invalid := func() bool {
			fail(fmt.Sprintf("malformed anthropic %s event", event.Type))
			return false
		}
		indexValid := func() bool { return event.Index != nil && *event.Index >= 0 }
		switch event.Type {
		case "message_start":
			if started || event.Message == nil || event.Message.ID == "" {
				return invalid()
			}
			started = true
			var startUsage *canonical.CanonicalUsage
			if event.Message.Usage != nil {
				u := event.Message.Usage
				startUsage = &canonical.CanonicalUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.InputTokens + u.OutputTokens}
				copy := *startUsage
				usage = &copy
			}
			return send(&canonical.CanonicalChatStreamEvent{Type: "message_start", ResponseID: event.Message.ID, Usage: startUsage})
		case "content_block_start":
			if !started || !indexValid() || event.ContentBlock == nil || event.ContentBlock.Type == "" || seenBlocks[*event.Index] || stop != nil {
				return invalid()
			}
			idx, block := *event.Index, event.ContentBlock
			seenBlocks[idx] = true
			openBlocks[idx] = block.Type
			switch block.Type {
			case "tool_use":
				if block.ID == "" || block.Name == "" {
					return invalid()
				}
				toolIndex := nextToolIndex
				nextToolIndex++
				toolIndices[idx] = toolIndex
				return send(&canonical.CanonicalChatStreamEvent{Type: "tool_call_start", ContentIndex: idx,
					Delta:    &canonical.CanonicalContentPart{Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: block.Type, Data: block.Native}},
					ToolCall: &canonical.CanonicalToolCallDelta{Index: toolIndex, ID: block.ID, Name: block.Name}})
			case "text", "thinking", "redacted_thinking":
				part := &canonical.CanonicalContentPart{Type: "text", Text: block.Text, Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: block.Type, Data: block.Native}}
				if block.Type != "text" {
					part.Type = "reasoning"
					part.Reasoning = &canonical.CanonicalReasoning{Content: block.Thinking}
				}
				return send(&canonical.CanonicalChatStreamEvent{Type: "content_start", ContentIndex: idx, Delta: part})
			default:
				fail(fmt.Sprintf("unsupported anthropic content block type %q", block.Type))
				return false
			}
		case "content_block_delta":
			if !indexValid() || event.Delta == nil || event.Delta.Type == "" || stop != nil {
				return invalid()
			}
			idx, delta := *event.Index, event.Delta
			blockType, exists := openBlocks[idx]
			if !exists {
				return invalid()
			}
			// A present string is required even when the provider sends an empty delta.
			var deltaFields map[string]json.RawMessage
			if json.Unmarshal(fields["delta"], &deltaFields) != nil {
				return invalid()
			}
			validString := func(key string) bool {
				raw := deltaFields[key]
				return len(raw) > 0 && raw[0] == '"'
			}
			switch delta.Type {
			case "text_delta":
				if blockType != "text" || !validString("text") {
					return invalid()
				}
				return send(&canonical.CanonicalChatStreamEvent{Type: "content_delta", ContentIndex: idx, Delta: &canonical.CanonicalContentPart{Type: "text", Text: delta.Text, Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: delta.Type, Data: delta.Native}}})
			case "thinking_delta", "signature_delta":
				key := "thinking"
				if delta.Type == "signature_delta" {
					key = "signature"
				}
				if blockType != "thinking" || !validString(key) {
					return invalid()
				}
				return send(&canonical.CanonicalChatStreamEvent{Type: "content_delta", ContentIndex: idx, Delta: &canonical.CanonicalContentPart{Type: "reasoning", Reasoning: &canonical.CanonicalReasoning{Content: delta.Thinking}, Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: delta.Type, Data: delta.Native}}})
			case "input_json_delta":
				if blockType != "tool_use" || !validString("partial_json") {
					return invalid()
				}
				return send(&canonical.CanonicalChatStreamEvent{Type: "tool_call_delta", ContentIndex: idx, Delta: &canonical.CanonicalContentPart{Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: delta.Type, Data: delta.Native}}, ToolCall: &canonical.CanonicalToolCallDelta{Index: toolIndices[idx], ArgumentsDelta: delta.PartialJson}})
			default:
				fail(fmt.Sprintf("unsupported anthropic content delta type %q", delta.Type))
				return false
			}
		case "content_block_stop":
			if !indexValid() {
				return invalid()
			}
			idx := *event.Index
			if _, exists := openBlocks[idx]; !exists {
				return invalid()
			}
			delete(openBlocks, idx)
			return send(&canonical.CanonicalChatStreamEvent{Type: "content_stop", ContentIndex: idx})
		case "message_delta":
			if !started || len(openBlocks) != 0 || event.Delta == nil || (event.Delta.StopReason == nil && event.Usage == nil) {
				return invalid()
			}
			if event.Delta.StopReason != nil {
				if stop != nil {
					return invalid()
				}
				reason := *event.Delta.StopReason
				if reason == "" {
					return invalid()
				}
				switch reason {
				case "tool_use":
					reason = "tool_calls"
				case "max_tokens":
					reason = "length"
				}
				stop = &canonical.CanonicalStop{Reason: reason, Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: "message_delta", Data: currentRaw}}
			}
			if event.Usage != nil {
				next := canonical.CanonicalUsage{}
				if usage != nil {
					next = *usage
				}
				next.OutputTokens = event.Usage.OutputTokens
				next.TotalTokens = next.InputTokens + next.OutputTokens
				usage = &next
				if event.Delta.StopReason == nil {
					return send(&canonical.CanonicalChatStreamEvent{Type: "usage", Usage: usage})
				}
			}
		case "message_stop":
			if !started || stop == nil || len(openBlocks) != 0 {
				fail("anthropic message_stop received without a complete message")
				return false
			}
			send(&canonical.CanonicalChatStreamEvent{Type: "done", Stop: stop, Usage: usage})
			return false
		case "error":
			var providerError ErrorResponse
			if json.Unmarshal(currentRaw, &providerError) != nil || providerError.Error.Message == "" {
				fail("anthropic stream reported an invalid provider error")
				return false
			}
			status := http.StatusBadGateway
			switch providerError.Error.Type {
			case "invalid_request_error":
				status = http.StatusBadRequest
			case "authentication_error":
				status = http.StatusUnauthorized
			case "permission_error":
				status = http.StatusForbidden
			case "not_found_error":
				status = http.StatusNotFound
			case "rate_limit_error":
				status = http.StatusTooManyRequests
			case "overloaded_error":
				status = http.StatusServiceUnavailable
			}
			send(&canonical.CanonicalChatStreamEvent{Type: "error", Error: canonical.NewGatewayError("anthropic", model, providerError.Error.Message, status, currentRaw)})
			return false
		case "ping":
			return true
		default:
			// Anthropic may add event types; ignore those we do not translate.
			return true
		}
		return true
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !dispatch() {
				return
			}
			currentRaw = nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			dataSize += len(value) + 1
			if dataSize > maxEventSize {
				fail("anthropic stream event exceeds size limit")
				return
			}
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			fail(fmt.Sprintf("anthropic stream read error: %v", err))
		}
		return
	}
	// An unterminated SSE frame is incomplete, even if its JSON happens to parse.
	if ctx.Err() == nil {
		currentRaw = nil
		fail("anthropic stream ended before message_stop")
	}
}
