package router_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	inboundAnthropic "github.com/mayer-tech/llmbrane/internal/inbound/anthropic"
	inboundOpenAI "github.com/mayer-tech/llmbrane/internal/inbound/openai"
	outboundOpenAI "github.com/mayer-tech/llmbrane/internal/outbound/openai"
	"github.com/mayer-tech/llmbrane/internal/router"
)

func TestCORSPreflight(t *testing.T) {
	for _, tc := range []struct {
		name       string
		origins    []string
		origin     string
		method     string
		path       string
		wantStatus int
		wantOrigin string
	}{
		{"default openai", nil, "https://app.example.com", "POST", "/openai/v1/chat/completions", 204, "*"},
		{"default anthropic", nil, "https://app.example.com", "POST", "/anthropic/v1/messages", 204, "*"},
		{"models", nil, "https://app.example.com", "GET", "/openai/v1/models", 204, "*"},
		{"head", nil, "https://app.example.com", "HEAD", "/anthropic/v1/models", 204, "*"},
		{"allowed origin", []string{"https://app.example.com"}, "https://app.example.com", "POST", "/openai/v1/chat/completions", 204, "https://app.example.com"},
		{"wildcard origin", []string{"*"}, "https://other.example.com", "POST", "/openai/v1/chat/completions", 204, "*"},
		{"disallowed origin", []string{"https://app.example.com"}, "https://other.example.com", "POST", "/openai/v1/chat/completions", 403, ""},
		{"disabled", []string{}, "https://app.example.com", "POST", "/openai/v1/chat/completions", 403, ""},
		{"disallowed method", nil, "https://app.example.com", "DELETE", "/openai/v1/chat/completions", 403, "*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			mux := http.NewServeMux()
			mux.HandleFunc("POST /openai/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { calls++ })
			mux.HandleFunc("POST /anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) { calls++ })
			mux.HandleFunc("GET /openai/v1/models", func(w http.ResponseWriter, r *http.Request) { calls++ })
			mux.HandleFunc("GET /anthropic/v1/models", func(w http.ResponseWriter, r *http.Request) { calls++ })
			handler := router.CORS(tc.origins)(mux)
			req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", tc.method)
			req.Header.Add("Access-Control-Request-Headers", "authorization, content-type")
			req.Header.Add("Access-Control-Request-Headers", "x-api-key, anthropic-version, x-custom-header")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			if recorder.Code != tc.wantStatus || recorder.Header().Get("Access-Control-Allow-Origin") != tc.wantOrigin {
				t.Fatalf("status=%d origin=%q", recorder.Code, recorder.Header().Get("Access-Control-Allow-Origin"))
			}
			if calls != 0 {
				t.Fatal("preflight reached an API handler")
			}
			vary := strings.Join(recorder.Header().Values("Vary"), ", ")
			for _, field := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
				if !strings.Contains(vary, field) {
					t.Errorf("missing Vary %s: %s", field, vary)
				}
			}
			if tc.wantStatus == http.StatusNoContent {
				if got := recorder.Header().Get("Access-Control-Allow-Headers"); got != "authorization, content-type, x-api-key, anthropic-version, x-custom-header" {
					t.Errorf("unexpected allowed headers: %q", got)
				}
				if got := recorder.Header().Get("Access-Control-Allow-Methods"); got != "GET, HEAD, POST" {
					t.Errorf("unexpected allowed methods: %q", got)
				}
				if recorder.Body.Len() != 0 {
					t.Errorf("preflight response has a body: %q", recorder.Body.String())
				}
			}
		})
	}
}

func TestCORSActualResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		origin     string
		wantStatus int
		wantOrigin string
	}{
		{"allowed", "GET", "/resource", "https://app.example.com", 429, "https://app.example.com"},
		{"disallowed", "GET", "/resource", "https://other.example.com", 429, ""},
		{"no origin", "GET", "/resource", "", 429, ""},
		{"not found", "GET", "/missing", "https://app.example.com", 404, "https://app.example.com"},
		{"method not allowed", "POST", "/resource", "https://app.example.com", 405, "https://app.example.com"},
		{"ordinary options", "OPTIONS", "/resource", "", 405, ""},
		{"panic", "GET", "/panic", "https://app.example.com", 500, "https://app.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /resource", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)
			})
			mux.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) { panic("test") })
			handler := router.Chain(mux, router.CORS([]string{"https://app.example.com"}), router.PanicRecovery)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tc.wantStatus || recorder.Header().Get("Access-Control-Allow-Origin") != tc.wantOrigin {
				t.Fatalf("status=%d origin=%q", recorder.Code, recorder.Header().Get("Access-Control-Allow-Origin"))
			}
			if got := recorder.Header().Get("Access-Control-Allow-Credentials"); got != "" {
				t.Errorf("credentials unexpectedly allowed: %q", got)
			}
			if tc.wantOrigin != "" && recorder.Header().Get("Access-Control-Expose-Headers") != "Retry-After" {
				t.Error("Retry-After is not exposed")
			}
			if !strings.Contains(strings.Join(recorder.Header().Values("Vary"), ", "), "Origin") {
				t.Error("missing Vary: Origin")
			}
		})
	}
}

func TestCORSInboundIntegration(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("invalid provider request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"response-1","model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`)
			return
		}
		// Verify that middleware preserves http.Flusher for both inbound protocols.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"response-1\",\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n"+
			"data: {\"id\":\"response-1\",\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer provider.Close()

	cfg := &config.Config{Providers: map[string]config.Provider{
		"test": {Endpoints: []config.Endpoint{{
			API: "openai-chat-completions", URL: provider.URL,
			Models: map[string]config.ModelConf{"test": {Target: "test"}},
		}}},
	}}
	gateway := core.NewGateway(cfg)
	gateway.RegisterOutboundAdapter("openai-chat-completions", outboundOpenAI.NewAdapterWithTimeout(time.Second))
	mux := router.New()
	inboundOpenAI.NewHandler(gateway).RegisterRoutes(mux, "/openai")
	inboundAnthropic.NewHandler(gateway).RegisterRoutes(mux, "/anthropic")
	handler := router.Chain(mux, router.CORS(nil), router.PanicRecovery, router.RequestPath, router.DebugLog)

	for _, tc := range []struct {
		name   string
		path   string
		body   string
		want   string
		stream bool
	}{
		{"openai stream", "/openai/v1/chat/completions", `{"model":"test/test","messages":[{"role":"user","content":"Hello"}],"stream":true}`, "data: [DONE]", true},
		{"anthropic stream", "/anthropic/v1/messages", `{"model":"test/test","max_tokens":100,"messages":[{"role":"user","content":"Hello"}],"stream":true}`, "event: ", true},
		{"openai", "/openai/v1/chat/completions", `{"model":"test/test","messages":[{"role":"user","content":"Hello"}]}`, "Hello", false},
		{"anthropic", "/anthropic/v1/messages", `{"model":"test/test","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`, "Hello", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preflight := httptest.NewRequest(http.MethodOptions, tc.path, nil)
			preflight.Header.Set("Origin", "https://app.example.com")
			preflight.Header.Set("Access-Control-Request-Method", "POST")
			preflight.Header.Set("Access-Control-Request-Headers", "authorization, content-type, x-api-key, anthropic-version")
			preflightResponse := httptest.NewRecorder()
			handler.ServeHTTP(preflightResponse, preflight)
			if preflightResponse.Code != http.StatusNoContent || preflightResponse.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatalf("unexpected preflight: %d %v", preflightResponse.Code, preflightResponse.Header())
			}

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Origin", "https://app.example.com")
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK || recorder.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
			if (tc.stream && !recorder.Flushed) || !strings.Contains(recorder.Body.String(), tc.want) {
				t.Fatalf("unexpected response: flushed=%t body=%s", recorder.Flushed, recorder.Body.String())
			}
		})
	}
	for _, path := range []string{"/openai/v1/models", "/anthropic/v1/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", "https://app.example.com")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK || recorder.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(recorder.Body.String(), "test/test") {
			t.Errorf("%s: status=%d headers=%v body=%s", path, recorder.Code, recorder.Header(), recorder.Body.String())
		}
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("provider calls=%d, want 4", got)
	}
}
