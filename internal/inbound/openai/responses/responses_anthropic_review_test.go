package responses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
	"github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

func TestResponsesAnthropicRequestTranslationValidation(t *testing.T) {
	var calls atomic.Int32
	requests := make(chan map[string]any, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		_, _ = fmt.Fprint(w, `{"id":"provider-id","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer backend.Close()
	handler := anthropicResponsesHandler(backend.URL)

	valid := map[string]any{
		"model": "p/m", "instructions": "first", "tool_choice": "auto",
		"tools": []any{map[string]any{"type": "function", "name": "f"}},
		"input": []any{map[string]any{"role": "developer", "content": "second"}, map[string]any{"role": "user", "content": "hello"}},
	}
	w := postResponses(handler, valid)
	if w.Code != http.StatusOK {
		t.Fatalf("valid request: %d %s", w.Code, w.Body)
	}
	upstream := <-requests
	if upstream["system"] != "first\n\nsecond" {
		t.Fatalf("system: %#v", upstream["system"])
	}
	tool := upstream["tools"].([]any)[0].(map[string]any)
	if tool["input_schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("tool: %#v", tool)
	}

	w = postResponses(handler, map[string]any{"model": "p/m", "input": "hello", "tool_choice": "auto"})
	if w.Code != http.StatusOK {
		t.Fatalf("auto without tools: %d %s", w.Code, w.Body)
	}
	if _, ok := (<-requests)["tool_choice"]; ok {
		t.Fatal("sent tool_choice without tools")
	}

	invalid := []map[string]any{
		{"model": "p/m", "input": "hello", "tool_choice": "required", "tools": []any{map[string]any{"type": "function", "name": "f"}}},
		{"model": "p/m", "input": []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "system", "content": "late"}}},
		{"model": "p/m", "input": "hello", "reasoning": map[string]any{"summary": "detailed"}},
		{"model": "p/m", "input": "hello", "text": map[string]any{"verbosity": "high"}},
		{"model": "p/m", "input": "hello", "metadata": map[string]any{"key": "value"}},
		{"model": "p/m", "input": "hello", "tools": []any{map[string]any{"type": "function", "name": "f", "strict": true}}},
		{"model": "p/m", "input": "hello", "tool_choice": map[string]any{"type": "function", "name": "missing"}, "tools": []any{map[string]any{"type": "function", "name": "f"}}},
		{"model": "p/m", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,!!!"}}}}},
		{"model": "p/m", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "https://example.com/a.png", "detail": "high"}}}}},
		{"model": "p/m", "input": []any{map[string]any{"role": "user", "content": []any{}}}},
		{"model": "p/m", "input": []any{map[string]any{"type": "function_call", "call_id": "call", "name": "f", "arguments": "{}", "status": "in_progress"}}},
		{"model": "p/m", "input": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "x", "annotations": []any{map[string]any{"type": "url_citation"}}}}}}},
		{"model": "p/m", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "https://example.com/a.png", "file_id": "file_1"}}}}},
	}
	for _, body := range invalid {
		w := postResponses(handler, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("request %#v: %d %s", body, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"p/m","input":"x","parallel_tool_calls": false }`))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("parallel_tool_calls: %d %s", w.Code, w.Body)
	}
	if calls.Load() != 2 {
		t.Fatalf("invalid requests reached backend: %d", calls.Load())
	}
}

func TestResponsesAnthropicProviderResponseValidation(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"malformed JSON", `{`},
		{"HTTP 200 error object", `{"type":"error","error":{"type":"api_error","message":"provider failed"}}`},
		{"missing content", `{"id":"m","stop_reason":"end_turn"}`},
		{"null content", `{"id":"m","content":null,"stop_reason":"end_turn"}`},
		{"missing text", `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text"}],"stop_reason":"end_turn"}`},
		{"scalar tool input", `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"tool_use","id":"call","name":"f","input":42}],"stop_reason":"tool_use"}`},
		{"duplicate tool IDs", `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"tool_use","id":"call","name":"f","input":{}},{"type":"tool_use","id":"call","name":"f","input":{}}],"stop_reason":"tool_use"}`},
		{"unknown block", `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"future"}],"stop_reason":"end_turn"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, tc.body) }))
			defer backend.Close()
			w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": "hello"})
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "provider_error") {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestResponsesAnthropicGatewayIDs(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["stream"] == true {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"same-provider-id\"}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"same-provider-id","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer backend.Close()
	handler := anthropicResponsesHandler(backend.URL)
	ids := map[string]bool{}
	for _, stream := range []bool{false, false, true, true} {
		w := postResponses(handler, map[string]any{"model": "p/m", "input": "hello", "stream": stream})
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var id string
		if !stream {
			id = decodeObject(t, w.Body.Bytes())["id"].(string)
		} else {
			var created, completed string
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				event := decodeObject(t, []byte(strings.TrimPrefix(line, "data: ")))
				if event["type"] == "response.created" {
					created = event["response"].(map[string]any)["id"].(string)
				}
				if event["type"] == "response.completed" {
					completed = event["response"].(map[string]any)["id"].(string)
				}
			}
			if created == "" || created != completed {
				t.Fatalf("inconsistent stream IDs: %s", w.Body)
			}
			id = created
		}
		if !strings.HasPrefix(id, "resp_gw_") || ids[id] {
			t.Fatalf("invalid or duplicate response ID %q", id)
		}
		ids[id] = true
	}
}

func TestResponsesAnthropicStreamToolInputAndLifecycle(t *testing.T) {
	events := []*canonical.CanonicalChatStreamEvent{
		{Type: "message_start", ResponseID: "provider"},
		{Type: "tool_call_start", ContentIndex: 0, ToolCall: &canonical.CanonicalToolCallDelta{ID: "call", Name: "f"}, Delta: nativePart("tool_use", `{"type":"tool_use","id":"call","name":"f","input":{ }}`)},
		{Type: "tool_call_delta", ContentIndex: 0, ToolCall: &canonical.CanonicalToolCallDelta{ArgumentsDelta: `{"x":2}`}},
		{Type: "content_stop", ContentIndex: 0},
		{Type: "done", Stop: &canonical.CanonicalStop{Reason: "tool_calls"}},
	}
	mapped := mappedEvents(t, events)
	if mapped[len(mapped)-1]["type"] != "response.completed" {
		t.Fatalf("events: %#v", mapped)
	}
	var item map[string]any
	for _, event := range mapped {
		if event["type"] == "response.output_item.done" {
			item = event["item"].(map[string]any)
		}
	}
	if item["arguments"] != `{"x":2}` || item["call_id"] != "call" || item["id"] == "call" {
		t.Fatalf("tool item: %#v", item)
	}

	reused := append([]*canonical.CanonicalChatStreamEvent{}, events[:4]...)
	reused = append(reused, events[1], events[3], events[4])
	mapped = mappedEvents(t, reused)
	if mapped[len(mapped)-1]["type"] != "error" {
		t.Fatalf("duplicate index accepted: %#v", mapped)
	}
	duplicateCall := append([]*canonical.CanonicalChatStreamEvent{}, events[:4]...)
	duplicateCall = append(duplicateCall, &canonical.CanonicalChatStreamEvent{Type: "tool_call_start", ContentIndex: 1, ToolCall: &canonical.CanonicalToolCallDelta{ID: "call", Name: "f"}}, events[4])
	mapped = mappedEvents(t, duplicateCall)
	if mapped[len(mapped)-1]["type"] != "error" {
		t.Fatalf("duplicate call ID accepted: %#v", mapped)
	}
}

func TestResponsesAnthropicMalformedStreamRejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call\",\"name\":\"f\",\"input\":42}}\n\n")
	}))
	defer backend.Close()
	w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": "hello", "stream": true})
	if !bytes.Contains(w.Body.Bytes(), []byte("event: error")) || bytes.Contains(w.Body.Bytes(), []byte("event: response.completed")) {
		t.Fatalf("stream: %s", w.Body)
	}
}

func TestResponsesAnthropicAliasRoutingAndPolicy(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request["model"] != "provider-target" {
			t.Errorf("model = %#v", request["model"])
		}
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer backend.Close()
	ep := config.Endpoint{API: "anthropic-messages", URL: backend.URL, Models: map[string]config.ModelConf{"m": {Target: "provider-target"}}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{ep}}}, Aliases: map[string]config.Alias{"alias": {Target: "p/m"}}, Policies: map[string]any{"unsupported_features": "reject"}})
	gateway.RegisterOutboundAdapter("anthropic-messages", anthropic.NewAdapter())
	mux := http.NewServeMux()
	NewHandler(gateway).RegisterRoutes(mux, "/openai")
	w := postResponses(mux, map[string]any{"model": "alias", "input": "hello"})
	if w.Code != http.StatusOK || decodeObject(t, w.Body.Bytes())["model"] != "alias" || calls.Load() != 1 {
		t.Fatalf("alias: %d %s, calls %d", w.Code, w.Body, calls.Load())
	}
	w = postResponses(mux, map[string]any{"model": "alias", "input": "hello", "stream": true})
	if w.Code != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("policy: %d %s, calls %d", w.Code, w.Body, calls.Load())
	}
}

func TestResponsesAnthropicClientCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer backend.Close()
	server := httptest.NewServer(anthropicResponsesHandler(backend.URL))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/openai/v1/responses", strings.NewReader(`{"model":"p/m","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("backend request was not cancelled")
	}
}

func TestResponsesAnthropicProviderErrorStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer backend.Close()
	ep := config.Endpoint{API: "anthropic-messages", URL: backend.URL, Models: map[string]config.ModelConf{"m": {Target: "claude"}}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{ep}}}})
	gateway.RegisterOutboundAdapter("anthropic-messages", anthropic.NewAdapterWithPolicy(time.Second, nil, retry.Policy{}))
	mux := http.NewServeMux()
	NewHandler(gateway).RegisterRoutes(mux, "/openai")
	for _, stream := range []bool{false, true} {
		w := postResponses(mux, map[string]any{"model": "p/m", "input": "hello", "stream": stream})
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "3" || !strings.Contains(w.Body.String(), "slow down") {
			t.Fatalf("stream=%v: %d %s", stream, w.Code, w.Body)
		}
	}
}

func TestResponsesAnthropicParallelToolsReplay(t *testing.T) {
	requests := make(chan map[string]any, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests <- request
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"tool_use","id":"call_a","name":"f","input":{"x":1}},{"type":"tool_use","id":"call_b","name":"f","input":{"x":2}}],"stop_reason":"tool_use"}`)
	}))
	defer backend.Close()
	handler := anthropicResponsesHandler(backend.URL)
	w := postResponses(handler, map[string]any{"model": "p/m", "input": "hello"})
	if w.Code != http.StatusOK {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	<-requests
	items := decodeObject(t, w.Body.Bytes())["output"].([]any)
	a, b := items[0].(map[string]any), items[1].(map[string]any)
	if a["id"] == b["id"] || a["id"] == a["call_id"] || b["id"] == b["call_id"] {
		t.Fatalf("IDs: %#v", items)
	}
	input := []any{map[string]any{"role": "user", "content": "hello"}, a, b, map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "two"}, map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "one"}}
	w = postResponses(handler, map[string]any{"model": "p/m", "input": input})
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	messages := (<-requests)["messages"].([]any)
	assistant := messages[1].(map[string]any)["content"].([]any)
	results := messages[2].(map[string]any)["content"].([]any)
	if assistant[0].(map[string]any)["id"] != "call_a" || assistant[1].(map[string]any)["id"] != "call_b" || results[0].(map[string]any)["tool_use_id"] != "call_b" || results[1].(map[string]any)["tool_use_id"] != "call_a" {
		t.Fatalf("replay: %#v", messages)
	}
}
