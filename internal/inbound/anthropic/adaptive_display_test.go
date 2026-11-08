package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	outboundanthropic "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
	outboundresponses "github.com/mayer-tech/llmbrane/internal/outbound/openairesponses"
)

func TestAdaptiveDisplayNativeEndToEnd(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		options      string
		modelDefault string
		aliasDefault string
		extensions   map[string]any
		thinking     string
		output       string
		status       int
	}{
		{name: "unset", model: "p/m", status: 200},
		{name: "adaptive", model: "p/m", options: `,"thinking":{"type":"adaptive"}`, thinking: `{"type":"adaptive"}`, status: 200},
		{name: "summarized", model: "p/m", options: `,"thinking":{"type":"adaptive","display":"summarized"}`, thinking: `{"type":"adaptive","display":"summarized"}`, status: 200},
		{name: "omitted", model: "p/m", options: `,"thinking":{"type":"adaptive","display":"omitted"}`, thinking: `{"type":"adaptive","display":"omitted"}`, status: 200},
		{name: "explicit effort", model: "alias", options: `,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"low"}`, modelDefault: "medium", aliasDefault: "high", thinking: `{"type":"adaptive","display":"summarized"}`, output: `{"effort":"low"}`, status: 200},
		{name: "model default", model: "p/m", options: `,"thinking":{"type":"adaptive","display":"omitted"}`, modelDefault: "medium", thinking: `{"type":"adaptive","display":"omitted"}`, output: `{"effort":"medium"}`, status: 200},
		{name: "alias default", model: "alias", options: `,"thinking":{"type":"adaptive","display":"summarized"}`, modelDefault: "medium", aliasDefault: "high", thinking: `{"type":"adaptive","display":"summarized"}`, output: `{"effort":"high"}`, status: 200},
		{name: "mapped effort", model: "alias", options: `,"thinking":{"type":"adaptive"}`, aliasDefault: "high", extensions: map[string]any{"reasoning_effort_mapping": map[string]string{"high": "low"}}, thinking: `{"type":"adaptive"}`, output: `{"effort":"low"}`, status: 200},
		{name: "manual display on budget route", model: "p/m", options: `,"thinking":{"type":"enabled","budget_tokens":1024,"display":"omitted"}`, extensions: map[string]any{"thinking_mode": "budget"}, thinking: `{"type":"enabled","budget_tokens":1024,"display":"omitted"}`, status: 200},
		{name: "manual display on adaptive route", model: "p/m", options: `,"thinking":{"type":"enabled","budget_tokens":1024,"display":"summarized"}`, thinking: `{"type":"adaptive","display":"summarized"}`, output: `{"effort":"low"}`, status: 200},
		{name: "budget mode conflict", model: "p/m", options: `,"thinking":{"type":"adaptive"}`, extensions: map[string]any{"thinking_mode": "budget"}, status: 400},
		{name: "default none conflict", model: "p/m", options: `,"thinking":{"type":"adaptive"}`, modelDefault: "none", status: 400},
		{name: "mapped none conflict", model: "p/m", options: `,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`, extensions: map[string]any{"reasoning_effort_mapping": map[string]string{"high": "none"}}, status: 400},
		{name: "disabled display", model: "p/m", options: `,"thinking":{"type":"disabled","display":"summarized"}`, status: 400},
		{name: "manual disabled by mapping", model: "p/m", options: `,"thinking":{"type":"enabled","budget_tokens":1024,"display":"omitted"}`, extensions: map[string]any{"reasoning_effort_mapping": map[string]string{"minimal": "none"}}, status: 400},
		{name: "adaptive budget", model: "p/m", options: `,"thinking":{"type":"adaptive","budget_tokens":1024}`, status: 400},
		{name: "manual effort conflict", model: "p/m", options: `,"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"low"}`, status: 400},
		{name: "unsupported display", model: "p/m", options: `,"thinking":{"type":"adaptive","display":"updates"}`, status: 400},
		{name: "empty display", model: "p/m", options: `,"thinking":{"type":"adaptive","display":""}`, status: 400},
		{name: "null display", model: "p/m", options: `,"thinking":{"type":"adaptive","display":null}`, status: 400},
		{name: "numeric display", model: "p/m", options: `,"thinking":{"type":"adaptive","display":1}`, status: 400},
		{name: "array display", model: "p/m", options: `,"thinking":{"type":"adaptive","display":["summarized"]}`, status: 400},
		{name: "object display", model: "p/m", options: `,"thinking":{"type":"adaptive","display":{}}`, status: 400},
	}
	for _, stream := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tt.name), func(t *testing.T) {
				requests := make(chan map[string]json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var wire map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
						t.Error(err)
					}
					requests <- wire
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"id\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"model\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						fmt.Fprint(w, `{"id":"id","type":"message","role":"assistant","model":"model","content":[]}`)
					}
				}))
				defer server.Close()
				cfg := &config.Config{
					Aliases: map[string]config.Alias{"alias": {Target: "p/m", DefaultReasoningEffort: tt.aliasDefault}},
					Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{
						API: "anthropic-messages", URL: server.URL, Extensions: tt.extensions,
						Models: map[string]config.ModelConf{"m": {Target: "model", DefaultReasoningEffort: tt.modelDefault}},
					}}}},
				}
				gateway := core.NewGateway(cfg)
				gateway.RegisterOutboundAdapter("anthropic-messages", outboundanthropic.NewAdapter())
				body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]%s}`, tt.model, stream, tt.options)
				w := httptest.NewRecorder()
				NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				if w.Code != tt.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
				}
				if tt.status != http.StatusOK {
					select {
					case wire := <-requests:
						t.Fatalf("invalid request forwarded: %v", wire)
					default:
					}
					return
				}
				select {
				case wire := <-requests:
					for key, want := range map[string]string{"thinking": tt.thinking, "output_config": tt.output} {
						got, present := wire[key]
						if want == "" {
							if present {
								t.Fatalf("unexpected %s: %s", key, got)
							}
						} else if !present || string(got) != want {
							t.Fatalf("%s = %s, want %s", key, got, want)
						}
					}
				default:
					t.Fatal("request not forwarded")
				}
			})
		}
	}
}

func TestAdaptiveDisplayResponsesRoute(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, display := range []string{"summarized", "omitted"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, display), func(t *testing.T) {
				var calls atomic.Int32
				requests := make(chan json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var body json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					requests <- body
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\"}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"m\"}]}}\n\n")
					} else {
						fmt.Fprint(w, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
					}
				}))
				defer server.Close()
				gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "openai-responses", URL: server.URL, Models: map[string]config.ModelConf{"m": {Target: "model"}}}}}}})
				gateway.RegisterOutboundAdapter("openai-responses", outboundresponses.NewAdapter())
				body := fmt.Sprintf(`{"model":"p/m","stream":%t,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","display":%q}}`, stream, display)
				w := httptest.NewRecorder()
				NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				if display == "omitted" {
					if w.Code != http.StatusBadRequest || calls.Load() != 0 || !strings.Contains(w.Body.String(), "thinking.display") {
						t.Fatalf("omitted accepted: %d %s, calls=%d", w.Code, w.Body.String(), calls.Load())
					}
					return
				}
				if w.Code != http.StatusOK || calls.Load() != 1 {
					t.Fatalf("status = %d: %s", w.Code, w.Body.String())
				}
				wire := <-requests
				if !strings.Contains(string(wire), `"summary":"auto"`) || strings.Contains(string(wire), `"display"`) || strings.Contains(string(wire), `"effort"`) {
					t.Fatalf("unexpected Responses request: %s", wire)
				}
			})
		}
	}
}
