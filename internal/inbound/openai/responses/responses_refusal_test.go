package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesAnthropicRefusal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n"+
						"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
						"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"+
						"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
						"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n"+
						"data: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hello"}],"stop_reason":"refusal","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":3}}`)
			}))
			defer provider.Close()
			w := httptest.NewRecorder()
			anthropicResponsesHandler(provider.URL).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"p/m","input":"hello","stream":%t}`, stream))))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			body := w.Body.String()
			if !strings.Contains(body, `"type":"refusal"`) || !strings.Contains(body, `"refusal":"hello"`) {
				t.Fatalf("missing refusal: %s", body)
			}
			if stream {
				expected := []string{"response.created", "response.output_item.added", "response.content_part.added", "response.refusal.delta", "response.refusal.done", "response.content_part.done", "response.output_item.done", "response.completed"}
				pos := 0
				for _, kind := range expected {
					i := strings.Index(body[pos:], "event: "+kind+"\n")
					if i < 0 {
						t.Fatalf("missing ordered %s: %s", kind, body)
					}
					pos += i + len(kind)
				}
				if strings.Contains(body, "response.output_text.") {
					t.Fatalf("unexpected text event: %s", body)
				}
			}
		})
	}
}

func TestResponsesRefusalReplayValidation(t *testing.T) {
	for _, tc := range []struct {
		name, content, role string
		valid               bool
	}{
		{"ordered", `[{"type":"output_text","text":"a"},{"type":"refusal","refusal":"no"},{"type":"output_text","text":"b"}]`, "assistant", true},
		{"role", `[{"type":"refusal","refusal":"no"}]`, "user", false},
		{"empty", `[{"type":"refusal","refusal":" "}]`, "assistant", false},
		{"missing", `[{"type":"refusal"}]`, "assistant", false},
		{"unknown", `[{"type":"refusal","refusal":"no","extra":1}]`, "assistant", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(fmt.Appendf(nil, `{"model":"p/m","input":[{"role":%q,"content":%s}]}`, tc.role, tc.content), &fields)
			req, err := translateResponsesRequest(fields)
			if err == nil {
				err = prepareResponsesRoute(req, "anthropic-messages")
			}
			if (err == nil) != tc.valid {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.valid {
				parts := req.Messages[0].Content
				if len(parts) != 3 || parts[0].Text != "a" || parts[1].Text != "no" || parts[2].Text != "b" {
					t.Fatalf("order: %#v", parts)
				}
			}
		})
	}
}
