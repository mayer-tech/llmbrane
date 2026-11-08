package responses

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

type responsesRepeatReader byte

func (r responsesRepeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestResponsesRequestBodyLimit(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer backend.Close()
	handler := anthropicResponsesHandler(backend.URL)
	const valid = `{"model":"p/m","input":"x"}`
	for _, tc := range []struct {
		name   string
		body   io.Reader
		length int64
		status int
	}{
		{"exact limit with trailing whitespace", io.MultiReader(strings.NewReader(valid), io.LimitReader(responsesRepeatReader(' '), int64(maxResponsesRequestBytes-len(valid)))), -1, http.StatusOK},
		{"oversized trailing whitespace", io.MultiReader(strings.NewReader(valid), io.LimitReader(responsesRepeatReader(' '), int64(maxResponsesRequestBytes-len(valid)+1))), -1, http.StatusRequestEntityTooLarge},
		{"oversized JSON string", io.MultiReader(strings.NewReader(`{"model":"p/m","input":"`), io.LimitReader(responsesRepeatReader('x'), int64(maxResponsesRequestBytes)), strings.NewReader(`"}`)), -1, http.StatusRequestEntityTooLarge},
		{"declared oversized body", strings.NewReader(valid), int64(maxResponsesRequestBytes + 1), http.StatusRequestEntityTooLarge},
		{"malformed", strings.NewReader(`{`), -1, http.StatusBadRequest},
		{"extra JSON", strings.NewReader(valid + ` {}`), -1, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", tc.body)
			r.ContentLength = tc.length
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			if tc.status == http.StatusRequestEntityTooLarge && !strings.Contains(w.Body.String(), "size limit") {
				t.Fatalf("missing limit error: %s", w.Body)
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("backend calls = %d, want 1", calls.Load())
	}
}

func TestResponsesRequestAllowsReasoningEnvelopeAndImage(t *testing.T) {
	artifact, err := json.Marshal(map[string]any{"type": "thinking", "thinking": strings.Repeat("a", (1<<20)-128), "signature": "sig"})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := canonical.EncodeReasoningEnvelope("anthropic", artifact)
	if err != nil {
		t.Fatal(err)
	}
	image := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, 1<<20))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer backend.Close()
	w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": []any{
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": envelope},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + image}}},
	}})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

// Discard large event payloads while retaining event types and errors for assertions.
type responsesLimitWriter struct {
	header    http.Header
	events    []string
	errorBody string
}

func (w *responsesLimitWriter) Header() http.Header { return w.header }
func (w *responsesLimitWriter) WriteHeader(int)     {}
func (w *responsesLimitWriter) Flush()              {}
func (w *responsesLimitWriter) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("event: ")) {
		line, _, _ := bytes.Cut(p, []byte("\n"))
		w.events = append(w.events, string(line[len("event: "):]))
		if bytes.Equal(line, []byte("event: error")) {
			w.errorBody = string(p)
		}
	}
	return len(p), nil
}

func runResponsesLimitStream(t *testing.T, events []*canonical.CanonicalChatStreamEvent) *responsesLimitWriter {
	t.Helper()
	ch := make(chan *canonical.CanonicalChatStreamEvent, len(events)+2)
	ch <- &canonical.CanonicalChatStreamEvent{Type: "message_start", ResponseID: "provider"}
	for _, event := range events {
		ch <- event
	}
	ch <- &canonical.CanonicalChatStreamEvent{Type: "done", Stop: &canonical.CanonicalStop{Reason: "end_turn"}}
	close(ch)
	w := &responsesLimitWriter{header: make(http.Header)}
	streamAnthropicResponses(context.Background(), w, w, ch, "model")
	return w
}

func assertResponsesStreamLimit(t *testing.T, w *responsesLimitWriter) {
	t.Helper()
	if len(w.events) == 0 || w.events[len(w.events)-1] != "error" || !strings.Contains(w.errorBody, "size limit") {
		t.Fatalf("expected size error, events %v, error %s", w.events, w.errorBody)
	}
	for _, event := range w.events {
		if event == "response.completed" || event == "response.incomplete" {
			t.Fatalf("success after limit: %v", w.events)
		}
	}
}

func TestResponsesStreamBufferReservationBoundaries(t *testing.T) {
	w := &responsesLimitWriter{header: make(http.Header)}
	for _, tc := range []struct {
		kind  string
		limit int
	}{{"message", maxResponsesStreamBlockBytes}, {"function_call", maxResponsesStreamBlockBytes}, {"reasoning", maxResponsesReasoningBlockBytes}} {
		t.Run(tc.kind, func(t *testing.T) {
			s := newResponsesStream(w, w, "m")
			b := &responsesStreamBlock{kind: tc.kind}
			if err := s.reserve(b, tc.limit); err != nil {
				t.Fatal(err)
			}
			if err := s.reserve(b, 1); err == nil {
				t.Fatal("accepted data beyond block limit")
			}
			if b.buffered != tc.limit || s.buffered != tc.limit {
				t.Fatal("failed reservation changed counters")
			}
		})
	}
	s := newResponsesStream(w, w, "m")
	for i := 0; i < 2; i++ {
		if err := s.reserve(&responsesStreamBlock{kind: "message"}, maxResponsesStreamBlockBytes); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.reserve(&responsesStreamBlock{kind: "message"}, 1); err == nil {
		t.Fatal("accepted data beyond stream limit")
	}
}

func TestResponsesStreamBlockLimits(t *testing.T) {
	chunk := strings.Repeat("x", 256<<10)
	textStart := &canonical.CanonicalChatStreamEvent{Type: "content_start", Delta: nativePart("text", `{"type":"text","text":""}`)}
	thinkingStart := &canonical.CanonicalChatStreamEvent{Type: "content_start", Delta: nativePart("thinking", `{"type":"thinking","thinking":"","signature":""}`)}
	toolStart := &canonical.CanonicalChatStreamEvent{Type: "tool_call_start", ToolCall: &canonical.CanonicalToolCallDelta{ID: "call", Name: "f"}, Delta: nativePart("tool_use", `{"type":"tool_use","input":{}}`)}
	for _, tc := range []struct {
		name  string
		start *canonical.CanonicalChatStreamEvent
		delta *canonical.CanonicalChatStreamEvent
		count int
	}{
		{"text", textStart, &canonical.CanonicalChatStreamEvent{Type: "content_delta", Delta: &canonical.CanonicalContentPart{Type: "text", Text: chunk}}, maxResponsesStreamBlockBytes/len(chunk) + 1},
		{"tool arguments", toolStart, &canonical.CanonicalChatStreamEvent{Type: "tool_call_delta", ToolCall: &canonical.CanonicalToolCallDelta{ArgumentsDelta: chunk}}, maxResponsesStreamBlockBytes/len(chunk) + 1},
		{"thinking", thinkingStart, &canonical.CanonicalChatStreamEvent{Type: "content_delta", Delta: nativePart("thinking_delta", `{"thinking":"`+chunk+`"}`)}, maxResponsesReasoningBlockBytes/len(chunk) + 1},
		{"signature", thinkingStart, &canonical.CanonicalChatStreamEvent{Type: "content_delta", Delta: nativePart("signature_delta", `{"signature":"`+chunk+`"}`)}, maxResponsesReasoningBlockBytes/len(chunk) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []*canonical.CanonicalChatStreamEvent{tc.start}
			for i := 0; i < tc.count; i++ {
				events = append(events, tc.delta)
			}
			events = append(events, &canonical.CanonicalChatStreamEvent{Type: "content_stop"})
			w := runResponsesLimitStream(t, events)
			assertResponsesStreamLimit(t, w)
			for _, event := range w.events {
				if event == "response.output_item.done" {
					t.Fatal("oversized block completed")
				}
			}
		})
	}
	for _, tc := range []struct {
		name  string
		start *canonical.CanonicalChatStreamEvent
	}{
		{"initial text", &canonical.CanonicalChatStreamEvent{Type: "content_start", Delta: nativePart("text", `{"type":"text","text":"`+strings.Repeat("x", maxResponsesStreamBlockBytes)+`"}`)}},
		{"initial tool input", &canonical.CanonicalChatStreamEvent{Type: "tool_call_start", ToolCall: &canonical.CanonicalToolCallDelta{ID: "call", Name: "f"}, Delta: nativePart("tool_use", `{"type":"tool_use","input":{"x":"`+strings.Repeat("x", maxResponsesStreamBlockBytes)+`"}}`)}},
		{"tool metadata", &canonical.CanonicalChatStreamEvent{Type: "tool_call_start", ToolCall: &canonical.CanonicalToolCallDelta{ID: strings.Repeat("x", maxResponsesStreamBlockBytes), Name: "f"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertResponsesStreamLimit(t, runResponsesLimitStream(t, []*canonical.CanonicalChatStreamEvent{tc.start}))
		})
	}
	for _, format := range []string{"thinking", "redacted_thinking"} {
		t.Run("artifact "+format, func(t *testing.T) {
			start := &canonical.CanonicalChatStreamEvent{Type: "content_start", Delta: nativePart(format, fmt.Sprintf(`{"type":%q,"thinking":"","signature":"sig","data":"opaque","extra":%q}`, format, strings.Repeat("x", maxResponsesReasoningBlockBytes)))}
			w := runResponsesLimitStream(t, []*canonical.CanonicalChatStreamEvent{start})
			assertResponsesStreamLimit(t, w)
			for _, event := range w.events {
				if event == "response.output_item.added" {
					t.Fatal("oversized artifact emitted")
				}
			}
		})
	}
}

func TestResponsesStreamCombinedReasoningLimit(t *testing.T) {
	chunk := strings.Repeat("x", 600<<10)
	events := []*canonical.CanonicalChatStreamEvent{
		{Type: "content_start", Delta: nativePart("thinking", `{"type":"thinking","thinking":"","signature":""}`)},
		{Type: "content_delta", Delta: nativePart("thinking_delta", `{"thinking":"`+chunk+`"}`)},
		{Type: "content_delta", Delta: nativePart("signature_delta", `{"signature":"`+chunk+`"}`)},
		{Type: "content_stop"},
	}
	assertResponsesStreamLimit(t, runResponsesLimitStream(t, events))
}

func TestResponsesStreamCumulativeCompletedBlocks(t *testing.T) {
	chunk := strings.Repeat("x", 1<<20)
	var events []*canonical.CanonicalChatStreamEvent
	for i := 0; i < 3; i++ {
		events = append(events, &canonical.CanonicalChatStreamEvent{Type: "content_start", ContentIndex: i, Delta: nativePart("text", `{"type":"text","text":""}`)})
		for j := 0; j < 6; j++ {
			events = append(events, &canonical.CanonicalChatStreamEvent{Type: "content_delta", ContentIndex: i, Delta: &canonical.CanonicalContentPart{Type: "text", Text: chunk}})
		}
		events = append(events, &canonical.CanonicalChatStreamEvent{Type: "content_stop", ContentIndex: i})
	}
	w := runResponsesLimitStream(t, events)
	assertResponsesStreamLimit(t, w)
	// Even completed blocks remain buffered until the provider's final stop reason.
	for _, event := range w.events {
		if event == "response.output_item.done" {
			t.Fatalf("buffered block was delivered after a size error: %v", w.events)
		}
	}

	// Pending SSE events have a separate limit and can fill first. Exercise the
	// cumulative retained-content budget independently of that event limit.
	s := newResponsesStream(w, w, "model")
	for i := 0; i < 3; i++ {
		b := &responsesStreamBlock{kind: "message"}
		for j := 0; j < 6; j++ {
			err := s.reserve(b, len(chunk))
			if i == 2 && j == 4 {
				if err == nil || !strings.Contains(err.Error(), "size limit") {
					t.Fatalf("cumulative content limit not enforced: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("block %d chunk %d: %v", i, j, err)
			}
		}
	}
	t.Fatal("cumulative content limit was not reached")
}

func TestResponsesStreamBlockMetadataConsumesBudget(t *testing.T) {
	w := &responsesLimitWriter{header: make(http.Header)}
	s := newResponsesStream(w, w, "m")
	if _, err := s.accept(&canonical.CanonicalChatStreamEvent{Type: "message_start", ResponseID: "provider"}); err != nil {
		t.Fatal(err)
	}
	s.buffered = maxResponsesStreamBytes - responsesStreamBlockOverhead + 1
	_, err := s.accept(&canonical.CanonicalChatStreamEvent{Type: "content_start", Delta: nativePart("text", `{"type":"text","text":""}`)})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("block metadata bypassed budget: %v", err)
	}
	if len(s.output) != 0 || len(s.seenBlocks) != 0 {
		t.Fatal("rejected block was retained")
	}
}

func TestResponsesAnthropicActualStreamBufferLimit(t *testing.T) {
	chunk := strings.Repeat("x", 256<<10)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n")
		for i := 0; i < 5; i++ {
			if _, err := fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":%q}}\n\n", chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backend.Close()
	w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": "hello", "stream": true})
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "event: error\n") || !strings.Contains(body, "size limit") || strings.Contains(body, "event: response.completed\n") || strings.Contains(body, "event: response.output_item.done\n") {
		t.Fatalf("stream status=%d, length=%d, tail=%s", w.Code, len(body), body[max(0, len(body)-500):])
	}
}

func TestResponsesAnthropicToolArgumentNumbers(t *testing.T) {
	const arguments = `{"number":1e400,"nested":{"large":9007199254740993}}`
	fields := map[string]json.RawMessage{"model": json.RawMessage(`"p/m"`)}
	fields["input"], _ = json.Marshal([]any{map[string]any{"type": "function_call", "call_id": "call", "name": "f", "arguments": arguments}})
	req, err := translateResponsesRequest(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareResponsesRoute(req, "anthropic-messages"); err != nil {
		t.Fatal(err)
	}
	if string(req.Messages[0].Content[0].ToolCall.Arguments) != arguments {
		t.Fatal("replayed arguments changed")
	}
	item, err := responsesItem(canonical.CanonicalContentPart{Type: "tool_call", ToolCall: &canonical.CanonicalToolCall{ID: "call", Name: "f", Arguments: json.RawMessage(arguments)}}, "prefix", 0)
	if err != nil || item["arguments"] != arguments {
		t.Fatalf("non-stream item: %v %v", item, err)
	}
	for _, initial := range []bool{false, true} {
		start := &canonical.CanonicalChatStreamEvent{Type: "tool_call_start", ToolCall: &canonical.CanonicalToolCallDelta{ID: "call", Name: "f"}}
		var events []*canonical.CanonicalChatStreamEvent
		if initial {
			start.Delta = nativePart("tool_use", `{"type":"tool_use","input":`+arguments+`}`)
			events = []*canonical.CanonicalChatStreamEvent{start}
		} else {
			events = []*canonical.CanonicalChatStreamEvent{start, {Type: "tool_call_delta", ToolCall: &canonical.CanonicalToolCallDelta{ArgumentsDelta: arguments}}}
		}
		events = append(events, &canonical.CanonicalChatStreamEvent{Type: "content_stop"})
		mapped := mappedEvents(t, append([]*canonical.CanonicalChatStreamEvent{{Type: "message_start", ResponseID: "provider"}}, append(events, &canonical.CanonicalChatStreamEvent{Type: "done", Stop: &canonical.CanonicalStop{Reason: "tool_calls"}})...))
		if mapped[len(mapped)-1]["type"] != "response.completed" {
			t.Fatalf("initial=%v: %v", initial, mapped)
		}
		output := mapped[len(mapped)-1]["response"].(map[string]any)["output"].([]any)
		if output[0].(map[string]any)["arguments"] != arguments {
			t.Fatalf("initial=%v: %v", initial, output)
		}
	}
	for _, invalid := range []string{`null`, `[]`, `42`, `{"x":`, `{} {}`} {
		_, err := responsesItem(canonical.CanonicalContentPart{Type: "tool_call", ToolCall: &canonical.CanonicalToolCall{ID: "call", Name: "f", Arguments: json.RawMessage(invalid)}}, "prefix", 0)
		if err == nil {
			t.Fatalf("accepted invalid arguments %q", invalid)
		}
	}
}

func TestResponsesAnthropicToolArgumentNumbersThroughGateway(t *testing.T) {
	const arguments = `{"number":1e400,"nested":{"large":9007199254740993}}`
	requests := make(chan []byte, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"tool_use","id":"call2","name":"f","input":`+arguments+`}],"stop_reason":"tool_use"}`)
	}))
	defer backend.Close()
	w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"type": "function_call", "call_id": "call", "name": "f", "arguments": arguments},
		map[string]any{"type": "function_call_output", "call_id": "call", "output": "ok"},
	}})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if !bytes.Contains(<-requests, []byte(arguments)) {
		t.Fatal("provider did not receive original arguments")
	}
	output := decodeObject(t, w.Body.Bytes())["output"].([]any)
	if output[0].(map[string]any)["arguments"] != arguments {
		t.Fatalf("response arguments: %v", output)
	}
}
