package responses

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	responses "github.com/mayer-tech/llmbrane/internal/outbound/openairesponses"
)

func TestResponsesRouteErrors(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer backend.Close()
	handler := responsesGateway(t, backend.URL, nil)

	for _, tc := range []struct {
		name, model, kind, message string
		status                     int
	}{
		{"unsupported API", "unsupported", "invalid_request", "Responses requests require an openai-responses or anthropic-messages backend", http.StatusBadRequest},
		{"missing Anthropic adapter", "cross", "internal_error", "no outbound adapter registered for api shape: anthropic-messages", http.StatusInternalServerError},
	} {
		for _, stream := range []bool{false, true} {
			name := tc.name + "/non-streaming"
			if stream {
				name = tc.name + "/streaming"
			}
			t.Run(name, func(t *testing.T) {
				w := postResponses(handler, map[string]any{"model": tc.model, "input": "hello", "stream": stream})
				if w.Code != tc.status || w.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("status %d, headers %v: %s", w.Code, w.Header(), w.Body)
				}
				detail, ok := decodeObject(t, w.Body.Bytes())["error"].(map[string]any)
				if !ok || detail["type"] != tc.kind || detail["message"] != tc.message {
					t.Fatalf("unexpected error: %s", w.Body)
				}
				if calls.Load() != 0 {
					t.Fatalf("route error reached backend: %d calls", calls.Load())
				}
			})
		}
	}
}

func TestResponsesPolicyRejectsUnsupportedCapabilities(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, `{}`) }))
	defer backend.Close()
	cfg := &config.Config{
		Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "openai-responses", URL: backend.URL, Models: map[string]config.ModelConf{"m": {Target: "m"}}}}}},
		Policies:  map[string]any{"unsupported_features": "reject"},
	}
	gateway := core.NewGateway(cfg)
	gateway.RegisterOutboundAdapter("openai-responses", responses.NewAdapter())
	mux := http.NewServeMux()
	NewHandler(gateway).RegisterRoutes(mux, "/openai")
	w := postResponses(mux, map[string]any{"model": "p/m", "input": "hello", "stream": true})
	if w.Code != http.StatusBadRequest || calls.Load() != 0 || !strings.Contains(w.Body.String(), "streaming") {
		t.Fatalf("policy: %d %s, calls %d", w.Code, w.Body, calls.Load())
	}
}

func TestResponsesBackendResponseValidation(t *testing.T) {
	for _, body := range []string{
		`{"id":"r","object":"response","status":"failed","output":[],"error":{"message":"fail"}}`,
		`{"id":"r","object":"response","status":"completed"}`,
		`{"id":"r","object":"response","status":"completed","output":[{"type":"computer_call"}]}`,
		`{"id":"r","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"audio"}]}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer backend.Close()
			w := postResponses(responsesGateway(t, backend.URL, nil), map[string]any{"model": "alias", "input": "hello"})
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
		})
	}
}

func TestResponsesWrongStreamContentType(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	w := postResponses(responsesGateway(t, backend.URL, nil), map[string]any{"model": "alias", "input": "hello", "stream": true})
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "SSE") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func TestResponsesClientCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer backend.Close()
	gateway := httptest.NewServer(responsesGateway(t, backend.URL, nil))
	defer gateway.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/openai/v1/responses", strings.NewReader(`{"model":"alias","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("backend request was not cancelled")
	}
}

func TestResponsesLocalLimit(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer backend.Close()
	ep := config.Endpoint{API: "openai-responses", URL: backend.URL, Models: map[string]config.ModelConf{"m": {Target: "m"}}, RateLimit: &config.RateLimit{TokensPerMinute: 1000, OutputReservationTokens: 2000}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{ep}}}})
	gateway.RegisterOutboundAdapter("openai-responses", responses.NewAdapter())
	mux := http.NewServeMux()
	NewHandler(gateway).RegisterRoutes(mux, "/openai")
	w := postResponses(mux, map[string]any{"model": "p/m", "input": "hello"})
	if w.Code != http.StatusTooManyRequests || calls.Load() != 0 {
		t.Fatalf("local limit: %d %s, calls %d", w.Code, w.Body, calls.Load())
	}
}
