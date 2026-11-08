package anthropic

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesStreamEmptyMappingErrors(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"r"}}`
	for _, tc := range []struct {
		name, want string
		events     []string
	}{
		{"completed empty", "empty Responses output", []string{`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`}},
		{"incomplete empty", "empty Responses output", []string{`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`}},
		{"empty text", "empty Responses text", []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m","role":"assistant"}}`,
			`{"type":"response.content_part.added","output_index":0,"item_id":"m","content_index":0,"part":{"type":"output_text","text":""}}`,
			`{"type":"response.content_part.done","output_index":0,"item_id":"m","content_index":0,"part":{"type":"output_text","text":""}}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", created)
				for _, event := range tc.events {
					fmt.Fprintf(w, "data: %s\n\n", event)
				}
			}))
			defer server.Close()
			w := httptest.NewRecorder()
			reviewResponsesHandler(server.URL).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(body, "event: error\n") || !strings.Contains(body, tc.want) || strings.Contains(body, "event: message_stop\n") || strings.Contains(body, "event: content_block_stop\n") {
				t.Fatalf("status %d: %s", w.Code, body)
			}
			if got, want := strings.Contains(body, "provider reached the output token limit"), tc.name == "incomplete empty"; got != want {
				t.Fatalf("unexpected token-limit detail: %s", body)
			}
		})
	}
}
