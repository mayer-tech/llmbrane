package responses

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
	chat "github.com/mayer-tech/llmbrane/internal/outbound/openai"
	responseadapter "github.com/mayer-tech/llmbrane/internal/outbound/openairesponses"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

var replayItems = []json.RawMessage{
	json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"opaque","extra":{"future":42}}`),
	json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"before","annotations":[],"extra":"kept"}]}`),
	json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"x\":1}","status":"completed","future":true}`),
	json.RawMessage(`{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"opaque2"}`),
	json.RawMessage(`{"type":"message","id":"msg_2","role":"assistant","status":"completed","content":[{"type":"output_text","text":"after","annotations":[]}]}`),
}

func responsesGateway(t *testing.T, backend string, policies map[string]any) http.Handler {
	t.Helper()
	limit := 777
	endpoint := config.Endpoint{
		API: "openai-responses", URL: backend, Headers: map[string]string{"Authorization": "Bearer test"},
		Models:       map[string]config.ModelConf{"model": {Target: "native-model", DefaultMaxOutputTokens: &limit, DefaultReasoningEffort: "medium"}},
		Capabilities: []string{"tools", "streaming", "vision", "json_schema", "json_object"},
		Extensions:   map[string]any{"reasoning_effort_mapping": map[string]any{"high": "mapped-high"}},
	}
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"provider": {Endpoints: []config.Endpoint{endpoint}},
			"other":    {Endpoints: []config.Endpoint{{API: "anthropic-messages", URL: backend, Models: map[string]config.ModelConf{"model": {Target: "other"}}}}},
			"chat":     {Endpoints: []config.Endpoint{{API: "openai-chat-completions", URL: backend, Models: map[string]config.ModelConf{"model": {Target: "chat-model"}}}}},
		},
		Aliases:  map[string]config.Alias{"alias": {Target: "provider/model"}, "cross": {Target: "other/model"}, "unsupported": {Target: "chat/model"}},
		Policies: policies,
	}
	gateway := core.NewGateway(cfg)
	gateway.RegisterOutboundAdapter("openai-responses", responseadapter.NewAdapterWithPolicy(time.Second, nil, httpretry.Policy{}))
	gateway.RegisterOutboundAdapter("openai-chat-completions", chat.NewAdapterWithPolicy(time.Second, nil, httpretry.Policy{}))
	mux := http.NewServeMux()
	for _, route := range NewHandler(gateway).InferenceRoutes() {
		mux.HandleFunc(route.Method+" /openai"+route.Path, route.Handler)
	}
	return mux
}

func postResponses(handler http.Handler, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", bytes.NewReader(data))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func responseObject(items []json.RawMessage) map[string]any {
	return map[string]any{"id": "resp_1", "object": "response", "created_at": 123, "model": "native-model", "status": "completed", "output": items, "usage": map[string]int{"input_tokens": 4, "output_tokens": 8, "total_tokens": 12}, "unknown": map[string]any{"preserved": true}}
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	return result
}

func TestResponsesNativeRoundTrip(t *testing.T) {
	requests := make(chan map[string]any, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing configured authorization")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responseObject(replayItems))
	}))
	defer backend.Close()
	handler := responsesGateway(t, backend.URL, map[string]any{"unsupported_features": "reject", "unknown_fields": "reject"})
	first := map[string]any{
		"model": "alias", "input": "hello", "instructions": "be brief", "temperature": 0.2, "max_output_tokens": 321,
		"reasoning":   map[string]any{"effort": "high", "summary": "detailed", "future": "retained"},
		"tools":       []any{map[string]any{"type": "function", "name": "lookup", "strict": true, "parameters": map[string]any{"type": "object"}}},
		"tool_choice": "required", "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "result", "schema": map[string]any{"type": "object"}, "strict": true}},
		"parallel_tool_calls": false, "metadata": map[string]string{"key": "value"},
	}
	w := postResponses(handler, first)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	upstream := <-requests
	if upstream["model"] != "native-model" || upstream["store"] != false || upstream["stream"] != false || upstream["max_output_tokens"] != float64(321) || upstream["temperature"] != 0.2 {
		t.Fatalf("unexpected outbound settings: %#v", upstream)
	}
	if !reflect.DeepEqual(upstream["include"], []any{"reasoning.encrypted_content"}) {
		t.Fatalf("include = %#v", upstream["include"])
	}
	if !reflect.DeepEqual(upstream["reasoning"], map[string]any{"effort": "mapped-high", "summary": "detailed", "future": "retained"}) {
		t.Fatalf("reasoning = %#v", upstream["reasoning"])
	}
	if !reflect.DeepEqual(upstream["tools"], first["tools"]) || upstream["tool_choice"] != "required" || !reflect.DeepEqual(upstream["text"], first["text"]) {
		t.Fatalf("settings changed: %#v", upstream)
	}
	out := decodeObject(t, w.Body.Bytes())
	if out["unknown"].(map[string]any)["preserved"] != true {
		t.Fatalf("response metadata lost: %s", w.Body)
	}
	output := out["output"].([]any)
	input := []any{map[string]any{"type": "message", "role": "user", "content": "hello"}}
	input = append(input, output...)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok", "future": "kept"}, map[string]any{"type": "message", "role": "user", "content": "continue"})
	w = postResponses(handler, map[string]any{"model": "provider/model", "input": input})
	if w.Code != http.StatusOK {
		t.Fatalf("continuation status %d: %s", w.Code, w.Body.String())
	}
	upstream = <-requests
	if !reflect.DeepEqual(upstream["input"], input) {
		t.Fatalf("input order or fields changed:\ngot %#v\nwant %#v", upstream["input"], input)
	}
	if upstream["max_output_tokens"] != float64(777) || upstream["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatalf("defaults lost: %#v", upstream)
	}
}

func TestResponsesStreamReplay(t *testing.T) {
	requests := make(chan map[string]any, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests <- request
		if request["stream"] != true {
			_ = json.NewEncoder(w).Encode(responseObject(replayItems))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fmt.Fprint(w, ": keepalive\n\nevent: response.created\ndata: {\"type\":\"response.created\",\ndata: \"response\":{\"id\":\"resp_1\"},\"sequence_number\":0}\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"content_index\":0,\"output_index\":1,\"delta\":\"before\",\"logprobs\":[],\"future\":\"kept\"}\n\n")
		fmt.Fprint(w, "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"sequence_number\":2,\"item_id\":\"fc_1\",\"output_index\":2,\"delta\":\"{\\\"x\\\":1}\"}\n\n")
		// The deliberately unknown extension is retained verbatim in event data.
		for i, item := range replayItems {
			fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":%d,\"output_index\":%d,\"item\":%s,\"extension\":{\"x\":1}}\n\n", i+3, i, item)
		}
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 8, "response": responseObject(replayItems)})
		fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", data)
	}))
	defer backend.Close()
	handler := responsesGateway(t, backend.URL, nil)
	w := postResponses(handler, map[string]any{"model": "alias", "input": "hello", "stream": true})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type %q", w.Header().Get("Content-Type"))
	}
	<-requests
	var items []any
	var types []string
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var eventName string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			event := decodeObject(t, []byte(strings.TrimPrefix(line, "data: ")))
			if event["type"] != eventName {
				t.Fatalf("SSE event mismatch: %q %#v", eventName, event)
			}
			types = append(types, eventName)
			if eventName == "response.output_text.delta" && (event["delta"] != "before" || event["future"] != "kept") {
				t.Fatalf("delta metadata lost: %#v", event)
			}
			if eventName == "response.function_call_arguments.delta" && event["delta"] != `{"x":1}` {
				t.Fatalf("arguments lost: %#v", event)
			}
			if eventName == "response.output_item.done" {
				if event["extension"].(map[string]any)["x"] != float64(1) {
					t.Fatal("event metadata lost")
				}
				items = append(items, event["item"])
			}
		}
	}
	if len(types) != len(replayItems)+4 || types[len(types)-1] != "response.completed" {
		t.Fatalf("unexpected stream events %#v: %s", types, w.Body.String())
	}
	input := append([]any{map[string]any{"role": "user", "content": "hello"}}, items...)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"})
	w = postResponses(handler, map[string]any{"model": "alias", "input": input})
	if w.Code != http.StatusOK {
		t.Fatalf("continuation status %d: %s", w.Code, w.Body)
	}
	if got := (<-requests)["input"]; !reflect.DeepEqual(got, input) {
		t.Fatalf("stream replay differs: %#v", got)
	}
}

func TestResponsesAnthropicCrossRoute(t *testing.T) {
	requests := make(chan map[string]any, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		if request["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n"+
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"hello\"}}\n\n"+
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"+
				"data: {\"type\":\"message_stop\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
	}))
	defer backend.Close()

	endpoint := config.Endpoint{API: "anthropic-messages", URL: backend.URL, Models: map[string]config.ModelConf{"model": {Target: "claude"}}}
	gateway := core.NewGateway(&config.Config{
		Providers: map[string]config.Provider{"other": {Endpoints: []config.Endpoint{endpoint}}},
		Aliases:   map[string]config.Alias{"cross": {Target: "other/model"}},
	})
	gateway.RegisterOutboundAdapter(endpoint.API, anthropic.NewAdapterWithPolicy(time.Second, nil, httpretry.Policy{}))
	mux := http.NewServeMux()
	for _, route := range NewHandler(gateway).InferenceRoutes() {
		mux.HandleFunc(route.Method+" /openai"+route.Path, route.Handler)
	}

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			w := postResponses(mux, map[string]any{"model": "cross", "input": "question", "stream": stream})
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var result map[string]any
			if stream {
				if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
					t.Fatalf("content type %q", w.Header().Get("Content-Type"))
				}
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					event := decodeObject(t, []byte(strings.TrimPrefix(line, "data: ")))
					if event["type"] == "error" {
						t.Fatalf("stream error: %s", w.Body)
					}
					if event["type"] == "response.completed" {
						result, _ = event["response"].(map[string]any)
					}
				}
			} else {
				result = decodeObject(t, w.Body.Bytes())
			}
			if result == nil || result["model"] != "cross" || result["status"] != "completed" {
				t.Fatalf("response: %s", w.Body)
			}
			output, ok := result["output"].([]any)
			if !ok || len(output) != 1 {
				t.Fatalf("output: %#v", result["output"])
			}
			item, ok := output[0].(map[string]any)
			if !ok || item["type"] != "message" {
				t.Fatalf("output item: %#v", output[0])
			}
			content, ok := item["content"].([]any)
			if !ok || len(content) != 1 || !reflect.DeepEqual(content[0], map[string]any{"type": "output_text", "text": "hello", "annotations": []any{}, "logprobs": []any{}}) {
				t.Fatalf("output content: %#v", item["content"])
			}
			var upstream map[string]any
			select {
			case upstream = <-requests:
			default:
				t.Fatal("request did not reach Anthropic backend")
			}
			messages, ok := upstream["messages"].([]any)
			if upstream["model"] != "claude" || (upstream["stream"] == true) != stream || !ok || len(messages) != 1 {
				t.Fatalf("upstream request: %#v", upstream)
			}
			message, ok := messages[0].(map[string]any)
			if !ok || message["role"] != "user" || !reflect.DeepEqual(message["content"], []any{map[string]any{"type": "text", "text": "question"}}) {
				t.Fatalf("upstream message: %#v", messages[0])
			}
		})
	}
}

func TestResponsesRejectAndErrors(t *testing.T) {
	calls := make(chan bool, 20)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- true
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer backend.Close()
	handler := responsesGateway(t, backend.URL, nil)
	for _, body := range []map[string]any{
		{"model": "missing", "input": "x"},
		{"model": "unsupported", "input": "x"},
		{"model": "alias", "input": "x", "previous_response_id": "resp_1"},
		{"model": "alias", "input": "x", "store": true},
		{"model": "alias", "input": []any{map[string]any{"type": "computer_call"}}},
		{"model": "alias", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_audio"}}}}},
		{"model": "alias", "input": "x", "tools": []any{map[string]any{"type": "web_search"}}},
	} {
		w := postResponses(handler, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for %#v, got %d: %s", body, w.Code, w.Body)
		}
	}
	select {
	case <-calls:
		t.Fatal("invalid request reached backend")
	default:
	}
	w := postResponses(handler, map[string]any{"model": "alias", "input": "x"})
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "3" || !strings.Contains(w.Body.String(), "slow down") {
		t.Fatalf("backend error: %d %#v %s", w.Code, w.Header(), w.Body)
	}
}

func TestResponsesStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, stream, expect string
		wantCompletion       bool
	}{
		{"truncated", "event: response.created\ndata: {\"type\":\"response.created\"}\n\n", "event: error", false},
		{"partial event", "data: {\"type\":\"response.completed\"}", "event: error", false},
		{"invalid json", "data: {broken}\n\n", "event: error", false},
		{"mismatched event", "event: response.failed\ndata: {\"type\":\"response.completed\"}\n\n", "event: error", false},
		{"failed", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"oops\"}}}\n\n", "event: response.failed", false},
		{"incomplete", "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"output\":[],\"status\":\"incomplete\"}}\n\n", "event: response.incomplete", false},
		{"incomplete error before stream", "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"output\":[],\"status\":\"incomplete\",\"error\":{\"message\":\"oops\"}}}\n\n", "event: error", false},
		{"incomplete error after stream", "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"output\":[],\"status\":\"incomplete\",\"error\":{\"message\":\"oops\"}}}\n\n", "event: error", false},
		{"invalid completion", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", "event: error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.stream)
			}))
			defer backend.Close()
			w := postResponses(responsesGateway(t, backend.URL, nil), map[string]any{"model": "alias", "input": "x", "stream": true})
			if !strings.Contains(w.Body.String(), tc.expect) || strings.Contains(w.Body.String(), "event: response.completed") != tc.wantCompletion || (tc.expect == "event: error" && strings.Contains(w.Body.String(), "event: response.incomplete")) {
				t.Fatalf("unexpected stream: %s", w.Body.String())
			}
		})
	}
}
