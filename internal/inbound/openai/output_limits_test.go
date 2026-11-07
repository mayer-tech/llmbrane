package openai

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

func TestAnthropicOutputLimitThroughChatCompletions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tt := range []struct {
			name         string
			modelDefault *int
			clientField  string
			want         string
		}{
			{name: "fallback", want: "16384"},
			{name: "configured", modelDefault: testOutputLimit(6000), want: "6000"},
			{name: "client wins", modelDefault: testOutputLimit(6000), clientField: `,"max_completion_tokens":1000`, want: "1000"},
			{name: "explicit zero", modelDefault: testOutputLimit(6000), clientField: `,"max_tokens":0`, want: "0"},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tt.name), func(t *testing.T) {
				requests := make(chan map[string]json.RawMessage, 1)
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode: %v", err)
					}
					requests <- payload
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
					} else {
						_, _ = w.Write([]byte(`{"id":"id","model":"model","content":[{"type":"text","text":"ok"}],"stop_reason":"max_tokens"}`))
					}
				}))
				defer provider.Close()
				endpoint := config.Endpoint{API: "anthropic-messages", URL: provider.URL, Models: map[string]config.ModelConf{"model": {Target: "model", DefaultMaxOutputTokens: tt.modelDefault}}}
				gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"provider": {Endpoints: []config.Endpoint{endpoint}}}})
				gateway.RegisterOutboundAdapter(endpoint.API, outbound.NewAdapter())
				body := fmt.Sprintf(`{"model":"provider/model","messages":[{"role":"user","content":"hello"}],"stream":%t%s}`, stream, tt.clientField)
				response := httptest.NewRecorder()
				NewHandler(gateway).HandleChatCompletions(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", response.Code, response.Body.String())
				}
				if !strings.Contains(response.Body.String(), `"finish_reason":"length"`) {
					t.Fatalf("missing finish reason: %s", response.Body.String())
				}
				wire := <-requests
				if string(wire["max_tokens"]) != tt.want {
					t.Fatalf("provider payload: %#v", wire)
				}
			})
		}
	}
}

func testOutputLimit(value int) *int { return &value }
