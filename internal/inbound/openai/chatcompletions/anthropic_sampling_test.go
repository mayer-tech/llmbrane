package chatcompletions

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

func TestAnthropicSamplingPolicyThroughChatCompletions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, omit := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/omit=%t", stream, omit), func(t *testing.T) {
				requests := make(chan map[string]json.RawMessage, 1)
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode provider request: %v", err)
					}
					requests <- body
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\"}}\n\n" +
							"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
							"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
					} else {
						_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
					}
				}))
				defer provider.Close()

				endpoint := config.Endpoint{
					API: "anthropic-messages", URL: provider.URL,
					Models: map[string]config.ModelConf{"opus": {Target: "claude-opus-5-5"}},
				}
				if omit {
					endpoint.Extensions = map[string]any{"omit_params": []string{"temperature", "top_p"}}
				}
				gateway := core.NewGateway(&config.Config{
					Aliases:   map[string]config.Alias{"opus": {Target: "anthropic/opus"}},
					Providers: map[string]config.Provider{"anthropic": {Endpoints: []config.Endpoint{endpoint}}},
				})
				gateway.RegisterOutboundAdapter("anthropic-messages", outbound.NewAdapter())
				handler := NewHandler(gateway)
				body := fmt.Sprintf(`{"model":"opus","messages":[{"role":"user","content":"Hello"}],"temperature":0.1,"top_p":1,"stream":%t,"reasoning_effort":"high"}`, stream)
				response := httptest.NewRecorder()
				handler.HandleChatCompletions(response, httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body)))
				if response.Code != http.StatusOK {
					t.Fatalf("response: %d %s", response.Code, response.Body.String())
				}
				if stream && (!strings.Contains(response.Body.String(), `"finish_reason":"stop"`) || !strings.Contains(response.Body.String(), "data: [DONE]\n\n")) {
					t.Fatalf("stream did not complete successfully: %s", response.Body.String())
				}
				wire := <-requests
				if string(wire["model"]) != `"claude-opus-5-5"` || string(wire["thinking"]) != `{"type":"adaptive"}` || string(wire["output_config"]) != `{"effort":"high"}` {
					t.Fatalf("unexpected translated request: %#v", wire)
				}
				if omit {
					if _, ok := wire["temperature"]; ok {
						t.Fatal("temperature was sent")
					}
					if _, ok := wire["top_p"]; ok {
						t.Fatal("top_p was sent")
					}
				} else if string(wire["temperature"]) != "0.1" || string(wire["top_p"]) != "1" {
					t.Fatalf("sampling values not preserved: %#v", wire)
				}
			})
		}
	}
}
