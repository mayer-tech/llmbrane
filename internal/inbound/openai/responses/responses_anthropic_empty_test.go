package responses

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesAnthropicEmptyOutputMapping(t *testing.T) {
	for _, tc := range []struct {
		name, content, stop, want string
	}{
		{"completed empty output", `[]`, "end_turn", "empty Anthropic output"},
		{"max tokens empty output", `[]`, "max_tokens", "empty Anthropic output"},
		{"empty text", `[{"type":"text","text":""}]`, "end_turn", "empty Anthropic text"},
		{"max tokens empty text", `[{"type":"text","text":""}]`, "max_tokens", "empty Anthropic text"},
		{"empty text among artifacts", `[{"type":"text","text":"before"},{"type":"text","text":""},{"type":"tool_use","id":"c","name":"f","input":{}}]`, "end_turn", "empty Anthropic text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":%s,"stop_reason":%q}`, tc.content, tc.stop)
			}))
			defer backend.Close()
			w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": "hi"})
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), tc.want) || !strings.Contains(w.Body.String(), "provider_error") {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if got, want := strings.Contains(w.Body.String(), "provider reached the output token limit"), tc.name == "max tokens empty output"; got != want {
				t.Fatalf("unexpected token-limit detail: %s", w.Body)
			}
		})
	}
}

func TestResponsesAnthropicWhitespaceIncompleteReplay(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":" "}],"stop_reason":"max_tokens"}`)
	}))
	defer backend.Close()
	handler := anthropicResponsesHandler(backend.URL)
	w := postResponses(handler, map[string]any{"model": "p/m", "input": "hi"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	result := decodeObject(t, w.Body.Bytes())
	if result["status"] != "incomplete" {
		t.Fatalf("response: %s", w.Body)
	}
	output := result["output"].([]any)
	input := append([]any{map[string]any{"role": "user", "content": "hi"}}, output...)
	input = append(input, map[string]any{"role": "user", "content": "next"})
	w = postResponses(handler, map[string]any{"model": "p/m", "input": input})
	if w.Code != http.StatusOK {
		t.Fatalf("replay status %d: %s", w.Code, w.Body)
	}
}

func TestResponsesAnthropicStreamEmptyOutputMapping(t *testing.T) {
	start := `{"type":"message_start","message":{"id":"m"}}`
	block := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	delta := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
	stop := `{"type":"content_block_stop","index":0}`
	for _, tc := range []struct {
		name, reason, want string
		blocks             []string
	}{
		{"completed empty output", "end_turn", "empty Anthropic output", nil},
		{"max tokens empty output", "max_tokens", "empty Anthropic output", nil},
		{"empty text", "end_turn", "empty Anthropic text", []string{block, stop}},
		{"empty delta", "max_tokens", "empty Anthropic text", []string{block, delta, stop}},
		{"valid then empty", "end_turn", "empty Anthropic text", []string{
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"ok"}}`,
			`{"type":"content_block_stop","index":1}`, block, stop,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", start)
				for _, event := range tc.blocks {
					fmt.Fprintf(w, "data: %s\n\n", event)
				}
				fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q}}\n\ndata: {\"type\":\"message_stop\"}\n\n", tc.reason)
			}))
			defer backend.Close()
			w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": "hi", "stream": true})
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(body, "event: error\n") || !strings.Contains(body, tc.want) || strings.Contains(body, "event: response.completed\n") || strings.Contains(body, "event: response.incomplete\n") {
				t.Fatalf("status %d: %s", w.Code, body)
			}
			if got, want := strings.Contains(body, "provider reached the output token limit"), tc.name == "max tokens empty output"; got != want {
				t.Fatalf("unexpected token-limit detail: %s", body)
			}
			// No output events are released before the provider's final stop reason.
			if strings.Contains(body, "event: response.output_item.done\n") || strings.Contains(body, "event: response.output_text.done\n") {
				t.Fatalf("buffered output was delivered after an invalid block: %s", body)
			}
		})
	}
}
