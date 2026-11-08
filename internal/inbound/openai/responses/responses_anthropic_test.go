package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
)

func anthropicResponsesHandler(url string) http.Handler {
	endpoint := config.Endpoint{API: "anthropic-messages", URL: url, Models: map[string]config.ModelConf{"m": {Target: "claude"}}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{endpoint}}}})
	gateway.RegisterOutboundAdapter("anthropic-messages", anthropic.NewAdapter())
	mux := http.NewServeMux()
	NewHandler(gateway).RegisterRoutes(mux, "/openai")
	return mux
}

func TestResponsesAnthropicNonStreamReplayAndTools(t *testing.T) {
	requests := make(chan map[string]any, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"thinking","thinking":"think","signature":"sig","extra":42},{"type":"text","text":"hello"},{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"call_1","name":"multiply","input":{"x":2}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`)
	}))
	defer backend.Close()
	handler := anthropicResponsesHandler(backend.URL)
	w := postResponses(handler, map[string]any{"model": "p/m", "input": "question", "tools": []any{map[string]any{"type": "function", "name": "multiply", "parameters": map[string]any{"type": "object"}}}})
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	<-requests
	result := decodeObject(t, w.Body.Bytes())
	output := result["output"].([]any)
	if result["status"] != "completed" || len(output) != 4 {
		t.Fatalf("unexpected output: %#v", result)
	}
	if output[3].(map[string]any)["call_id"] != "call_1" {
		t.Fatalf("tool call: %#v", output[3])
	}
	input := []any{map[string]any{"role": "user", "content": "question"}}
	input = append(input, output...)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"}, map[string]any{"role": "user", "content": "next"})
	w = postResponses(handler, map[string]any{"model": "p/m", "input": input})
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	upstream := <-requests
	messages := upstream["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages: %#v", messages)
	}
	assistant := messages[1].(map[string]any)["content"].([]any)
	want := []any{map[string]any{"type": "thinking", "thinking": "think", "signature": "sig", "extra": float64(42)}, map[string]any{"type": "text", "text": "hello"}, map[string]any{"type": "redacted_thinking", "data": "opaque"}, map[string]any{"type": "tool_use", "id": "call_1", "name": "multiply", "input": map[string]any{"x": float64(2)}}}
	if !reflect.DeepEqual(assistant, want) {
		t.Fatalf("replay: got %#v want %#v", assistant, want)
	}
	user := messages[2].(map[string]any)["content"].([]any)
	if user[0].(map[string]any)["tool_use_id"] != "call_1" || user[1].(map[string]any)["text"] != "next" {
		t.Fatalf("tool result: %#v", user)
	}
}

func TestResponsesAnthropicDisabledToolChoice(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, withTools := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/tools=%t", stream, withTools), func(t *testing.T) {
				requests := make(chan map[string]any, 1)
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					requests <- request
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						_, _ = fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
					}
				}))
				defer backend.Close()
				body := map[string]any{"model": "p/m", "input": "hello", "tool_choice": "none", "stream": stream}
				if withTools {
					body["tools"] = []any{map[string]any{"type": "function", "name": "f", "parameters": map[string]any{"type": "object"}}}
				}
				response := postResponses(anthropicResponsesHandler(backend.URL), body)
				if response.Code != http.StatusOK || (stream && !strings.Contains(response.Body.String(), "response.completed")) {
					t.Fatalf("response: %d %s", response.Code, response.Body)
				}
				upstream := <-requests
				choice, ok := upstream["tool_choice"].(map[string]any)
				if !ok || choice["type"] != "none" {
					t.Fatalf("tool choice: %#v", upstream["tool_choice"])
				}
				tools, hasTools := upstream["tools"].([]any)
				if hasTools != withTools || (withTools && (len(tools) != 1 || tools[0].(map[string]any)["name"] != "f")) {
					t.Fatalf("tools: %#v", upstream["tools"])
				}
			})
		}
	}
}

func TestResponsesAnthropicReplayRejectsInvalidArtifacts(t *testing.T) {
	openaiEnvelope, err := canonical.EncodeReasoningEnvelope("openai", json.RawMessage(`{"type":"reasoning","id":"r","summary":[],"encrypted_content":"cipher"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "opaque", "gw2.bad", "gw1.!", openaiEnvelope} {
		t.Run(value, func(t *testing.T) {
			fields := map[string]json.RawMessage{"model": json.RawMessage(`"p/m"`)}
			fields["input"], _ = json.Marshal([]any{map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": value}})
			req, err := translateResponsesRequest(fields)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepareResponsesRoute(req, "anthropic-messages"); err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
}

func TestResponsesAnthropicNonStreamMissingSignature(t *testing.T) {
	response := &canonical.CanonicalChatResponse{ID: "m", Choices: []canonical.CanonicalChoice{{FinishReason: "end_turn", Message: canonical.CanonicalMessage{Content: []canonical.CanonicalContentPart{{Type: "reasoning", Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: "thinking", Data: json.RawMessage(`{"type":"thinking","thinking":"partial"}`)}}}}}}}
	if _, err := responsesFromCanonical(response, "m"); err == nil {
		t.Fatal("missing signature accepted")
	}
}

func nativePart(format, raw string) *canonical.CanonicalContentPart {
	return &canonical.CanonicalContentPart{Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: format, Data: json.RawMessage(raw)}}
}

func anthropicStreamEvents() []*canonical.CanonicalChatStreamEvent {
	return []*canonical.CanonicalChatStreamEvent{
		{Type: "message_start", ResponseID: "msg_1", Usage: &canonical.CanonicalUsage{InputTokens: 3}},
		{Type: "content_start", ContentIndex: 0, Delta: nativePart("thinking", `{"type":"thinking","thinking":"","signature":""}`)},
		{Type: "content_delta", ContentIndex: 0, Delta: nativePart("thinking_delta", `{"type":"thinking_delta","thinking":"abc"}`)},
		{Type: "content_delta", ContentIndex: 0, Delta: nativePart("signature_delta", `{"type":"signature_delta","signature":"sig"}`)},
		{Type: "content_stop", ContentIndex: 0},
		{Type: "content_start", ContentIndex: 1, Delta: nativePart("redacted_thinking", `{"type":"redacted_thinking","data":"hidden"}`)},
		{Type: "content_stop", ContentIndex: 1},
		{Type: "content_start", ContentIndex: 2, Delta: nativePart("text", `{"type":"text","text":""}`)},
		{Type: "content_delta", ContentIndex: 2, Delta: &canonical.CanonicalContentPart{Type: "text", Text: "hi"}},
		{Type: "content_stop", ContentIndex: 2},
		{Type: "tool_call_start", ContentIndex: 3, ToolCall: &canonical.CanonicalToolCallDelta{ID: "call_1", Name: "f"}, Delta: nativePart("tool_use", `{"type":"tool_use","id":"call_1","name":"f","input":{}}`)},
		{Type: "tool_call_delta", ContentIndex: 3, ToolCall: &canonical.CanonicalToolCallDelta{ArgumentsDelta: `{"x":`}},
		{Type: "tool_call_delta", ContentIndex: 3, ToolCall: &canonical.CanonicalToolCallDelta{ArgumentsDelta: `2}`}},
		{Type: "content_stop", ContentIndex: 3},
		{Type: "done", Stop: &canonical.CanonicalStop{Reason: "tool_calls"}, Usage: &canonical.CanonicalUsage{OutputTokens: 4}},
	}
}

func mappedEvents(t *testing.T, events []*canonical.CanonicalChatStreamEvent) []map[string]any {
	t.Helper()
	ch := make(chan *canonical.CanonicalChatStreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	w := httptest.NewRecorder()
	streamAnthropicResponses(context.Background(), w, w, ch, "p/m")
	var result []map[string]any
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			result = append(result, decodeObject(t, []byte(strings.TrimPrefix(line, "data: "))))
		}
	}
	for i, event := range result {
		if event["sequence_number"] != float64(i) {
			t.Fatalf("sequence %d: %#v", i, event)
		}
	}
	return result
}

func TestResponsesAnthropicStreamLifecycle(t *testing.T) {
	events := mappedEvents(t, anthropicStreamEvents())
	if events[0]["type"] != "response.created" || events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("events: %#v", events)
	}
	var items []map[string]any
	var reasoningDeltas, toolDeltas int
	for _, event := range events {
		switch event["type"] {
		case "response.output_item.done":
			items = append(items, event["item"].(map[string]any))
		case "response.reasoning_summary_text.delta":
			reasoningDeltas++
		case "response.function_call_arguments.delta":
			toolDeltas++
		}
	}
	if len(items) != 4 || reasoningDeltas != 1 || toolDeltas != 2 {
		t.Fatalf("items=%#v events=%#v", items, events)
	}
	for i, want := range []string{"thinking", "redacted_thinking"} {
		decoded, err := canonical.DecodeReasoningEnvelope(items[i]["encrypted_content"].(string))
		if err != nil || decoded.Format != want {
			t.Fatalf("item %d: %#v %v", i, decoded, err)
		}
	}
	if len(items[1]["summary"].([]any)) != 0 || items[3]["arguments"] != `{"x":2}` || items[3]["call_id"] != "call_1" || items[3]["id"] == "call_1" {
		t.Fatalf("items: %#v", items)
	}
	response := events[len(events)-1]["response"].(map[string]any)
	if !reflect.DeepEqual(response["output"], []any{items[0], items[1], items[2], items[3]}) {
		t.Fatalf("final output: %#v", response)
	}
}

func TestResponsesAnthropicStreamInterrupted(t *testing.T) {
	all := anthropicStreamEvents()
	for _, count := range []int{3, 5, len(all) - 1} {
		events := mappedEvents(t, all[:count])
		if events[len(events)-1]["type"] != "error" {
			t.Fatalf("count %d: %#v", count, events)
		}
		for _, event := range events {
			if event["type"] == "response.completed" {
				t.Fatal("interruption reported as success")
			}
			if count == 3 && event["type"] == "response.output_item.done" {
				t.Fatal("partial artifact completed")
			}
		}
	}
}

func TestResponsesAnthropicActualStreamRequiresMessageStop(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
				if complete {
					_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			defer backend.Close()
			w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": "x", "stream": true})
			got := bytes.Contains(w.Body.Bytes(), []byte("event: response.completed"))
			if got != complete || bytes.Contains(w.Body.Bytes(), []byte("event: error")) == complete {
				t.Fatalf("stream: %s", w.Body)
			}
		})
	}
}
