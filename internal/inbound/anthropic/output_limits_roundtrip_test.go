package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	outbound "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
)

func TestOutputLimitThroughMessages(t *testing.T) {
	modelDefault := 6000
	for _, stream := range []bool{false, true} {
		for _, tt := range []struct{ name, clientField, want string }{
			{"missing", "", "6000"},
			{"zero", `,"max_tokens":0`, "0"},
			{"positive", `,"max_tokens":1000`, "1000"},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tt.name), func(t *testing.T) {
				requests := make(chan map[string]json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode: %v", err)
					}
					requests <- payload
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
					} else {
						_, _ = w.Write([]byte(`{"id":"id","model":"model","content":[],"stop_reason":"max_tokens"}`))
					}
				}))
				defer server.Close()
				ep := config.Endpoint{API: "anthropic-messages", URL: server.URL, Models: map[string]config.ModelConf{"model": {Target: "model", DefaultMaxOutputTokens: &modelDefault}}}
				g := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"provider": {Endpoints: []config.Endpoint{ep}}}})
				g.RegisterOutboundAdapter(ep.API, outbound.NewAdapter())
				body := fmt.Sprintf(`{"model":"provider/model","messages":[{"role":"user","content":"hello"}],"stream":%t%s}`, stream, tt.clientField)
				response := httptest.NewRecorder()
				NewHandler(g).HandleMessages(response, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", response.Code, response.Body.String())
				}
				if !strings.Contains(response.Body.String(), `"stop_reason":"max_tokens"`) {
					t.Fatalf("missing stop reason: %s", response.Body.String())
				}
				wire := <-requests
				if string(wire["max_tokens"]) != tt.want {
					t.Fatalf("provider payload: %#v", wire)
				}
			})
		}
	}
}
