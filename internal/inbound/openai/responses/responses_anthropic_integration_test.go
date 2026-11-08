package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesAnthropicStreamReplayEndToEnd(t *testing.T) {
	requests := make(chan map[string]any, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests <- request
		if request["stream"] != true {
			_, _ = fmt.Fprint(w, `{"id":"next","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, data := range []string{
			`{"type":"message_start","message":{"id":"msg"}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call","name":"f","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":2}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer backend.Close()
	handler := anthropicResponsesHandler(backend.URL)
	w := postResponses(handler, map[string]any{"model": "p/m", "input": "hello", "stream": true})
	<-requests
	var items []any
	completed := false
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		event := decodeObject(t, []byte(strings.TrimPrefix(line, "data: ")))
		if event["type"] == "response.output_item.done" {
			items = append(items, event["item"])
		}
		if event["type"] == "response.completed" {
			completed = true
		}
	}
	if !completed || len(items) != 2 {
		t.Fatalf("stream: %s", w.Body)
	}
	input := []any{map[string]any{"role": "user", "content": "hello"}}
	input = append(input, items...)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": "call", "output": "2"})
	w = postResponses(handler, map[string]any{"model": "p/m", "input": input})
	if w.Code != http.StatusOK {
		t.Fatalf("replay %d: %s", w.Code, w.Body)
	}
	messages := (<-requests)["messages"].([]any)
	blocks := messages[1].(map[string]any)["content"].([]any)
	thinking := blocks[0].(map[string]any)
	tool := blocks[1].(map[string]any)
	if thinking["thinking"] != "reason" || thinking["signature"] != "sig" || tool["id"] != "call" || tool["input"].(map[string]any)["x"] != float64(2) {
		t.Fatalf("replayed blocks: %#v", blocks)
	}
}
