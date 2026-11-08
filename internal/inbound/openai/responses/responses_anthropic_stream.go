package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

// Account for retained content across all blocks, including completed output.
// The per-block allowance includes raw start metadata and delta bytes; reasoning
// is limited separately because its replay envelope is limited to 1 MiB.
const (
	maxResponsesStreamBytes         = 16 << 20
	maxResponsesStreamBlockBytes    = 8 << 20
	maxResponsesReasoningBlockBytes = 1 << 20
	responsesStreamBlockOverhead    = 1024
)

type responsesStreamBlock struct {
	index       int
	id          string
	kind        string
	text        strings.Builder
	signature   strings.Builder
	callID      string
	name        string
	arguments   strings.Builder
	summaryOpen bool
	artifact    map[string]json.RawMessage
	buffered    int
}

type responsesStream struct {
	w            http.ResponseWriter
	flusher      http.Flusher
	model        string
	id           string
	prefix       string
	created      int64
	sequence     int
	written      int
	started      bool
	emitted      bool
	stop         *canonical.CanonicalStop
	usage        canonical.CanonicalUsage
	blocks       map[int]*responsesStreamBlock
	seenBlocks   map[int]bool
	callIDs      map[string]bool
	output       []any
	buffered     int
	pending      []pendingResponsesEvent
	pendingBytes int
}

type pendingResponsesEvent struct {
	kind string
	data []byte
}

func newResponsesStream(w http.ResponseWriter, flusher http.Flusher, model string) *responsesStream {
	return &responsesStream{w: w, flusher: flusher, model: model, created: time.Now().Unix(), blocks: make(map[int]*responsesStreamBlock), seenBlocks: make(map[int]bool), callIDs: make(map[string]bool), output: []any{}}
}

// Reservations are never released: completed blocks remain in the final output.
func (s *responsesStream) reserve(b *responsesStreamBlock, n int) error {
	blockLimit := maxResponsesStreamBlockBytes
	if b.kind == "reasoning" {
		blockLimit = maxResponsesReasoningBlockBytes
	}
	if n < 0 || n > blockLimit-b.buffered || n > maxResponsesStreamBytes-s.buffered {
		return fmt.Errorf("provider stream buffering exceeds size limit")
	}
	b.buffered += n
	s.buffered += n
	return nil
}

func (s *responsesStream) emit(kind string, fields map[string]any) error {
	fields["type"] = kind
	fields["sequence_number"] = s.sequence
	s.sequence++
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if s.stop == nil && kind != "response.created" {
		if len(data) > maxResponsesStreamBytes-s.pendingBytes {
			return fmt.Errorf("provider stream event buffering exceeds size limit")
		}
		s.pendingBytes += len(data)
		s.pending = append(s.pending, pendingResponsesEvent{kind: kind, data: data})
		return nil
	}
	return s.writeEvent(kind, data)
}

func (s *responsesStream) writeEvent(kind string, data []byte) error {
	if !s.emitted {
		// Keep the HTTP status available until the first SSE write.
		setResponsesStreamHeaders(s.w)
		s.emitted = true
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", kind, data); err != nil {
		return err
	}
	s.written++
	s.flusher.Flush()
	return nil
}

func (s *responsesStream) fail(err error) {
	if !s.emitted {
		gatewayError, ok := err.(*canonical.GatewayError)
		if !ok || gatewayError.StatusCode < 400 || gatewayError.StatusCode > 599 {
			message := err.Error()
			if ok {
				message = gatewayError.Message
			}
			gatewayError = canonical.NewGatewayError("anthropic", s.model, message, http.StatusBadGateway, nil)
		}
		writeGatewayError(s.w, gatewayError)
		return
	}
	// Buffered events have not been delivered and must not consume sequence numbers.
	s.sequence = s.written
	message := err.Error()
	if gatewayError, ok := err.(*canonical.GatewayError); ok {
		message = "provider stream failed: " + gatewayError.Message
	}
	writeResponsesStreamError(s.w, s.flusher, s.sequence, message)
}

func (s *responsesStream) response(status string) map[string]any {
	return map[string]any{"id": s.id, "object": "response", "created_at": s.created, "status": status, "model": s.model, "output": s.output, "usage": responsesUsage(&s.usage), "error": nil, "incomplete_details": nil}
}

func (s *responsesStream) updateUsage(u *canonical.CanonicalUsage) {
	if u == nil {
		return
	}
	if u.InputTokens > s.usage.InputTokens {
		s.usage.InputTokens = u.InputTokens
	}
	if u.OutputTokens > s.usage.OutputTokens {
		s.usage.OutputTokens = u.OutputTokens
	}
	s.usage.TotalTokens = s.usage.InputTokens + s.usage.OutputTokens
	if u.CachedInputTokens > s.usage.CachedInputTokens {
		s.usage.CachedInputTokens = u.CachedInputTokens
	}
	if u.ReasoningTokens > s.usage.ReasoningTokens {
		s.usage.ReasoningTokens = u.ReasoningTokens
	}
}

func (s *responsesStream) accept(event *canonical.CanonicalChatStreamEvent) (bool, error) {
	s.updateUsage(event.Usage)
	if event.Type == "error" {
		if event.Error != nil {
			return false, event.Error
		}
		return false, fmt.Errorf("provider stream failed")
	}
	if event.Type == "message_start" {
		if s.started || event.ResponseID == "" {
			return false, fmt.Errorf("invalid provider message_start")
		}
		id, err := newResponsesID()
		if err != nil {
			return false, err
		}
		s.started = true
		s.id = id
		s.prefix = strings.TrimPrefix(id, "resp_gw_")
		return false, s.emit("response.created", map[string]any{"response": s.response("in_progress")})
	}
	if !s.started {
		return false, fmt.Errorf("provider stream has no message_start")
	}
	switch event.Type {
	case "content_start", "tool_call_start":
		if s.stop != nil {
			return false, fmt.Errorf("content started after message_delta")
		}
		if event.ContentIndex < 0 || s.seenBlocks[event.ContentIndex] {
			return false, fmt.Errorf("invalid or duplicate content block")
		}
		b := &responsesStreamBlock{index: len(s.output)}
		if event.Type == "tool_call_start" {
			if event.ToolCall == nil || event.ToolCall.ID == "" || event.ToolCall.Name == "" || s.callIDs[event.ToolCall.ID] {
				return false, fmt.Errorf("invalid or duplicate tool call start")
			}
			b.kind = "function_call"
			b.callID = event.ToolCall.ID
			b.name = event.ToolCall.Name
			b.id = fmt.Sprintf("fc_gw_%s_%d", s.prefix, b.index)
			if err := s.reserve(b, responsesStreamBlockOverhead); err != nil {
				return false, err
			}
			if err := s.reserve(b, len(b.callID)); err != nil {
				return false, err
			}
			if err := s.reserve(b, len(b.name)); err != nil {
				return false, err
			}
			if event.Delta != nil && event.Delta.Native != nil {
				if err := s.reserve(b, len(event.Delta.Native.Data)); err != nil {
					return false, err
				}
				var block struct {
					Input json.RawMessage `json:"input"`
				}
				if json.Unmarshal(event.Delta.Native.Data, &block) != nil {
					return false, fmt.Errorf("invalid tool call block")
				}
				if len(block.Input) > 0 {
					var input map[string]json.RawMessage
					if json.Unmarshal(block.Input, &input) != nil || input == nil {
						return false, fmt.Errorf("invalid tool call input")
					}
					if len(input) > 0 {
						b.arguments.Write(block.Input)
					}
				}
			}
			s.callIDs[event.ToolCall.ID] = true
		} else {
			if event.Delta == nil || event.Delta.Native == nil || event.Delta.Native.Provider != "anthropic" {
				return false, fmt.Errorf("invalid content block start")
			}
			native := event.Delta.Native
			switch native.Format {
			case "text":
				b.kind = "message"
			case "thinking", "redacted_thinking":
				b.kind = "reasoning"
			default:
				return false, fmt.Errorf("unsupported content block %q", native.Format)
			}
			if err := s.reserve(b, responsesStreamBlockOverhead); err != nil {
				return false, err
			}
			if err := s.reserve(b, len(native.Data)); err != nil {
				return false, err
			}
			switch native.Format {
			case "text":
				var block struct {
					Type string  `json:"type"`
					Text *string `json:"text"`
				}
				if json.Unmarshal(native.Data, &block) != nil || block.Type != "text" || block.Text == nil {
					return false, fmt.Errorf("invalid text block")
				}
				b.id = fmt.Sprintf("msg_gw_%s_%d", s.prefix, b.index)
				b.text.WriteString(*block.Text)
			case "thinking", "redacted_thinking":
				b.id = fmt.Sprintf("rs_gw_%s_%d", s.prefix, b.index)
				if json.Unmarshal(native.Data, &b.artifact) != nil {
					return false, fmt.Errorf("invalid reasoning block")
				}
				var block struct {
					Type      string  `json:"type"`
					Thinking  *string `json:"thinking"`
					Signature *string `json:"signature"`
					Data      *string `json:"data"`
				}
				if json.Unmarshal(native.Data, &block) != nil || block.Type != native.Format {
					return false, fmt.Errorf("invalid reasoning block type")
				}
				if block.Type == "thinking" {
					if block.Thinking == nil {
						return false, fmt.Errorf("invalid thinking block")
					}
					b.text.WriteString(*block.Thinking)
					if block.Signature != nil {
						b.signature.WriteString(*block.Signature)
					}
				}
			}
		}
		s.seenBlocks[event.ContentIndex] = true
		s.blocks[event.ContentIndex] = b
		s.output = append(s.output, nil)
		var item map[string]any
		switch b.kind {
		case "message":
			item = map[string]any{"type": "message", "id": b.id, "role": "assistant", "status": "in_progress", "content": []any{}}
		case "reasoning":
			item = map[string]any{"type": "reasoning", "id": b.id, "status": "in_progress", "summary": []any{}}
		case "function_call":
			item = map[string]any{"type": "function_call", "id": b.id, "call_id": b.callID, "name": b.name, "arguments": "", "status": "in_progress"}
		}
		if err := s.emit("response.output_item.added", map[string]any{"output_index": b.index, "item": item}); err != nil {
			return false, err
		}
		if b.kind == "message" {
			if err := s.emit("response.content_part.added", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}}); err != nil {
				return false, err
			}
			if b.text.Len() != 0 {
				return false, s.textDelta(b, b.text.String())
			}
		} else if b.kind == "reasoning" && b.text.Len() != 0 {
			return false, s.reasoningDelta(b, b.text.String())
		} else if b.kind == "function_call" && b.arguments.Len() != 0 {
			return false, s.emit("response.function_call_arguments.delta", map[string]any{"item_id": b.id, "output_index": b.index, "delta": b.arguments.String()})
		}
	case "content_delta", "tool_call_delta":
		b := s.blocks[event.ContentIndex]
		if b == nil || s.stop != nil {
			return false, fmt.Errorf("delta for unopened block")
		}
		switch b.kind {
		case "message":
			if event.Type != "content_delta" || event.Delta == nil || event.Delta.Type != "text" {
				return false, fmt.Errorf("invalid text delta")
			}
			if err := s.reserve(b, len(event.Delta.Text)); err != nil {
				return false, err
			}
			b.text.WriteString(event.Delta.Text)
			return false, s.textDelta(b, event.Delta.Text)
		case "reasoning":
			if event.Type != "content_delta" || event.Delta == nil || event.Delta.Native == nil || event.Delta.Native.Provider != "anthropic" {
				return false, fmt.Errorf("invalid reasoning delta")
			}
			if err := s.reserve(b, len(event.Delta.Native.Data)); err != nil {
				return false, err
			}
			var delta struct {
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			}
			if json.Unmarshal(event.Delta.Native.Data, &delta) != nil {
				return false, fmt.Errorf("invalid reasoning delta")
			}
			var typ string
			_ = json.Unmarshal(b.artifact["type"], &typ)
			if typ != "thinking" {
				return false, fmt.Errorf("delta for redacted reasoning block")
			}
			switch event.Delta.Native.Format {
			case "thinking_delta":
				b.text.WriteString(delta.Thinking)
				return false, s.reasoningDelta(b, delta.Thinking)
			case "signature_delta":
				b.signature.WriteString(delta.Signature)
			default:
				return false, fmt.Errorf("unsupported reasoning delta")
			}
		case "function_call":
			if event.Type != "tool_call_delta" || event.ToolCall == nil {
				return false, fmt.Errorf("invalid tool delta")
			}
			if err := s.reserve(b, len(event.ToolCall.ArgumentsDelta)); err != nil {
				return false, err
			}
			b.arguments.WriteString(event.ToolCall.ArgumentsDelta)
			return false, s.emit("response.function_call_arguments.delta", map[string]any{"item_id": b.id, "output_index": b.index, "delta": event.ToolCall.ArgumentsDelta})
		}
	case "content_stop":
		b := s.blocks[event.ContentIndex]
		if b == nil {
			return false, fmt.Errorf("stop for unopened block")
		}
		if err := s.finishBlock(b); err != nil {
			return false, err
		}
		delete(s.blocks, event.ContentIndex)
	case "done":
		if s.stop != nil || event.Stop == nil || event.Stop.Reason == "" || len(s.blocks) != 0 {
			return false, fmt.Errorf("provider completed without a valid stop reason or with open blocks")
		}
		if len(s.output) == 0 {
			return false, emptyAnthropicOutputError(event.Stop.Reason)
		}
		status, incomplete, err := anthropicResponsesStatus(event.Stop.Reason)
		if err != nil {
			return false, err
		}
		if event.Stop.Reason == "refusal" {
			if len(s.output) != 1 {
				return false, fmt.Errorf("Anthropic refusal must contain exactly one non-empty text block")
			}
			item, ok := s.output[0].(map[string]any)
			if !ok || item["type"] != "message" {
				return false, fmt.Errorf("Anthropic refusal must contain exactly one non-empty text block")
			}
			parts := item["content"].([]any)
			text := parts[0].(map[string]any)["text"].(string)
			if strings.TrimSpace(text) == "" {
				return false, fmt.Errorf("Anthropic refusal must contain exactly one non-empty text block")
			}
			item["content"] = []any{map[string]any{"type": "refusal", "refusal": text}}
		}
		s.stop = event.Stop
		for _, pending := range s.pending {
			kind, data := pending.kind, pending.data
			if s.stop.Reason == "refusal" {
				kind, data, err = refusalStreamEvent(kind, data)
				if err != nil {
					return false, err
				}
			}
			if err := s.writeEvent(kind, data); err != nil {
				return false, err
			}
		}
		s.pending = nil
		kind := "response.completed"
		if status == "incomplete" {
			kind = "response.incomplete"
		}
		response := s.response(status)
		response["incomplete_details"] = incomplete
		return true, s.emit(kind, map[string]any{"response": response})
	case "usage":
	default:
		return false, fmt.Errorf("unsupported provider stream event %q", event.Type)
	}
	return false, nil
}

func refusalStreamEvent(kind string, data []byte) (string, []byte, error) {
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", nil, err
	}
	convertPart := func(part map[string]any) {
		text, _ := part["text"].(string)
		for key := range part {
			delete(part, key)
		}
		part["type"] = "refusal"
		part["refusal"] = text
	}
	switch kind {
	case "response.content_part.added", "response.content_part.done":
		convertPart(fields["part"].(map[string]any))
	case "response.output_text.delta":
		kind = "response.refusal.delta"
		delete(fields, "logprobs")
	case "response.output_text.done":
		kind = "response.refusal.done"
		fields["refusal"] = fields["text"]
		delete(fields, "text")
		delete(fields, "logprobs")
	case "response.output_item.done":
		item := fields["item"].(map[string]any)
		parts := item["content"].([]any)
		convertPart(parts[0].(map[string]any))
	}
	fields["type"] = kind
	data, err := json.Marshal(fields)
	return kind, data, err
}

func (s *responsesStream) textDelta(b *responsesStreamBlock, text string) error {
	return s.emit("response.output_text.delta", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "delta": text, "logprobs": []any{}})
}

func (s *responsesStream) reasoningDelta(b *responsesStreamBlock, text string) error {
	if text == "" {
		return nil
	}
	if !b.summaryOpen {
		b.summaryOpen = true
		if err := s.emit("response.reasoning_summary_part.added", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}); err != nil {
			return err
		}
	}
	return s.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "delta": text})
}

func (s *responsesStream) finishBlock(b *responsesStreamBlock) error {
	var item map[string]any
	switch b.kind {
	case "message":
		text := b.text.String()
		if text == "" {
			return fmt.Errorf("empty Anthropic text block cannot be replayed")
		}
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
		if err := s.emit("response.output_text.done", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "text": text, "logprobs": []any{}}); err != nil {
			return err
		}
		if err := s.emit("response.content_part.done", map[string]any{"item_id": b.id, "output_index": b.index, "content_index": 0, "part": part}); err != nil {
			return err
		}
		item = map[string]any{"type": "message", "id": b.id, "role": "assistant", "status": "completed", "content": []any{part}}
	case "reasoning":
		var typ string
		_ = json.Unmarshal(b.artifact["type"], &typ)
		if typ == "thinking" {
			b.artifact["thinking"], _ = json.Marshal(b.text.String())
			b.artifact["signature"], _ = json.Marshal(b.signature.String())
		}
		data, err := json.Marshal(b.artifact)
		if err != nil {
			return err
		}
		native := &canonical.CanonicalProviderData{Provider: "anthropic", Format: typ, Data: data}
		if err := validateAnthropicReasoning(native); err != nil {
			return err
		}
		envelope, err := canonical.EncodeReasoningEnvelope("anthropic", data)
		if err != nil {
			return err
		}
		summary := []any{}
		if b.summaryOpen {
			text := b.text.String()
			part := map[string]any{"type": "summary_text", "text": text}
			summary = append(summary, part)
			if err := s.emit("response.reasoning_summary_text.done", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "text": text}); err != nil {
				return err
			}
			if err := s.emit("response.reasoning_summary_part.done", map[string]any{"item_id": b.id, "output_index": b.index, "summary_index": 0, "part": part}); err != nil {
				return err
			}
		}
		item = map[string]any{"type": "reasoning", "id": b.id, "status": "completed", "summary": summary, "encrypted_content": envelope}
	case "function_call":
		if b.arguments.Len() == 0 {
			if err := s.reserve(b, 2); err != nil {
				return err
			}
			b.arguments.WriteString("{}")
		}
		arguments := b.arguments.String()
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(arguments), &object) != nil || object == nil {
			return fmt.Errorf("incomplete tool arguments")
		}
		if err := s.emit("response.function_call_arguments.done", map[string]any{"item_id": b.id, "output_index": b.index, "arguments": arguments}); err != nil {
			return err
		}
		item = map[string]any{"type": "function_call", "id": b.id, "call_id": b.callID, "name": b.name, "arguments": arguments, "status": "completed"}
	}
	s.output[b.index] = item
	return s.emit("response.output_item.done", map[string]any{"output_index": b.index, "item": item})
}
