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
	outbound "github.com/mayer-tech/llmbrane/internal/outbound/openairesponses"
)

func reviewResponsesHandler(url string) *Handler {
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "openai-responses", URL: url, Capabilities: []string{"streaming"}, Models: map[string]config.ModelConf{"m": {Target: "provider"}}}}}}})
	gateway.RegisterOutboundAdapter("openai-responses", outbound.NewAdapter())
	return NewHandler(gateway)
}

func TestResponsesRejectUnrepresentableInput(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprint(w, `{"id":"r","status":"completed","output":[]}`)
	}))
	defer server.Close()
	h := reviewResponsesHandler(server.URL)
	cases := []string{
		`{"model":"p/m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"image","source":{}}]}]}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":42}]}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","is_error":"yes"}]}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":[{"type":"unknown"}]}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"other"}}]}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":[{"type":"image","source":{"type":[]}}]}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":[]}]}]}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":"hi"}],"system":[{"type":[]}]}`,
		`{"model":"p/m","messages":[]}`,
		`{"model":"p/m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"f","input":[]}]}]}`,
		`{"model":"p/m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"f"}]}]}`,
		`{"model":"p/m","messages":[{"role":"invalid","content":"hi"}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":[]}]}`,
		`{"model":"p/m","messages":[{"role":"user","content":"hi"}],"top_k":10}`,
		`{"model":"p/m","messages":[{"role":"user","content":"hi"}]} {}`,
	}
	for _, body := range cases {
		w := httptest.NewRecorder()
		h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("accepted %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if requests != 0 {
		t.Fatalf("forwarded %d invalid requests", requests)
	}
}

func TestResponsesStreamErrorStatusBeforeAndAfterStart(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%t", started), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if started {
					fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")
				}
				fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"oops\"}}}\n\n")
			}))
			defer server.Close()
			w := httptest.NewRecorder()
			reviewResponsesHandler(server.URL).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
			if started {
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "event: error\n") || strings.Contains(w.Body.String(), "event: message_stop\n") {
					t.Fatalf("stream: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "event: error") {
				t.Fatalf("initial error: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestResponsesToolErrorAndSchemaPrecision(t *testing.T) {
	forwarded := make(chan json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		forwarded <- body
		fmt.Fprint(w, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer server.Close()
	body := `{"model":"p/m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","is_error":true,"content":"failed"}]}],"tools":[{"name":"f","input_schema":{"type":"object","maximum":9007199254740993}}]}`
	w := httptest.NewRecorder()
	reviewResponsesHandler(server.URL).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	request := string(<-forwarded)
	if !strings.Contains(request, `9007199254740993`) || !strings.Contains(request, `Tool execution failed:\nfailed`) {
		t.Fatalf("request: %s", request)
	}
}

func TestResponsesEmptyOutputAndText(t *testing.T) {
	for _, body := range []string{
		`{"id":"r","status":"completed","output":[]}`,
		`{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
		`{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		w := httptest.NewRecorder()
		reviewResponsesHandler(server.URL).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/m","messages":[{"role":"user","content":"hi"}]}`)))
		server.Close()
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "cannot be replayed") || !strings.Contains(w.Body.String(), "api_error") {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if got, want := strings.Contains(w.Body.String(), "provider reached the output token limit"), strings.Contains(body, `"status":"incomplete"`); got != want {
			t.Fatalf("unexpected token-limit detail: %s", w.Body)
		}
	}
}
