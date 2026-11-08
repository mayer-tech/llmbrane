package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
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

func serveMessagesWithBodyLimit(h *Handler, w http.ResponseWriter, r *http.Request) {
	h.HandleMessages(w, r)
}

type repeatedBodyByte byte

func (b repeatedBodyByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func bodyLimitHandler(t *testing.T, api string, fail bool) (*Handler, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"type":"permission_error","message":"provider denied"}}`)
			return
		}
		if api == "anthropic-messages" {
			_, _ = io.WriteString(w, `{"id":"id","type":"message","role":"assistant","model":"model","content":[]}`)
		} else {
			_, _ = io.WriteString(w, `{"id":"id","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
		}
	}))
	t.Cleanup(server.Close)

	endpoint := config.Endpoint{API: api, URL: server.URL, Models: map[string]config.ModelConf{"m": {Target: "model"}}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{endpoint}}}})
	if api == "anthropic-messages" {
		gateway.RegisterOutboundAdapter(api, outboundanthropic.NewAdapter())
	} else {
		gateway.RegisterOutboundAdapter(api, outboundresponses.NewAdapter())
	}
	return NewHandler(gateway), &calls
}

func assertMessagesError(t *testing.T, w *httptest.ResponseRecorder, status int, kind, message string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	var response ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Type != "error" || response.Error.Type != kind || !strings.Contains(response.Error.Message, message) {
		t.Fatalf("unexpected error response: %+v", response)
	}
}

func TestMessagesBodyLimitRejectsOversizedRequests(t *testing.T) {
	const prefix = `{"model":"p/m","messages":[{"role":"user","content":"`
	const suffix = `"}]}`
	const valid = `{"model":"p/m","messages":[{"role":"user","content":"hi"}]}`
	for _, api := range []string{"anthropic-messages", "openai-responses"} {
		t.Run(api, func(t *testing.T) {
			h, calls := bodyLimitHandler(t, api, false)
			for _, tt := range []struct {
				name   string
				body   func() io.Reader
				length int64
			}{
				{
					name: "unknown length large JSON string",
					body: func() io.Reader {
						return io.MultiReader(strings.NewReader(prefix), io.LimitReader(repeatedBodyByte('x'), maxMessagesBodyBytes), strings.NewReader(suffix))
					},
					length: -1,
				},
				{
					name: "known length trailing whitespace",
					body: func() io.Reader {
						return io.MultiReader(strings.NewReader(valid), io.LimitReader(repeatedBodyByte(' '), maxMessagesBodyBytes))
					},
					length: int64(len(valid) + maxMessagesBodyBytes),
				},
			} {
				t.Run(tt.name, func(t *testing.T) {
					request := httptest.NewRequest(http.MethodPost, "/v1/messages", tt.body())
					request.ContentLength = tt.length
					w := httptest.NewRecorder()
					serveMessagesWithBodyLimit(h, w, request)
					assertMessagesError(t, w, http.StatusRequestEntityTooLarge, "request_too_large", "40 MiB")
					if got := calls.Load(); got != 0 {
						t.Fatalf("forwarded %d oversized requests", got)
					}
				})
			}
		})
	}
}

func TestMessagesBodyLimitBoundary(t *testing.T) {
	for _, size := range []int64{maxMessagesBodyBytes - 1, maxMessagesBodyBytes, maxMessagesBodyBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", io.LimitReader(repeatedBodyByte(' '), size))
			w := httptest.NewRecorder()
			ok := limitMessagesBody(w, request)
			if ok != (size <= maxMessagesBodyBytes) {
				t.Fatalf("limitMessagesBody = %t for %d bytes", ok, size)
			}
			if !ok {
				assertMessagesError(t, w, http.StatusRequestEntityTooLarge, "request_too_large", "40 MiB")
				return
			}
			defer request.Body.Close()
			if got, err := io.Copy(io.Discard, request.Body); err != nil || got != size {
				t.Fatalf("replayed %d bytes, error %v; want %d", got, err, size)
			}
		})
	}
}

func TestMessagesBodyLimitPreservesHandling(t *testing.T) {
	const valid = `{"model":"p/m","messages":[{"role":"user","content":"hi"}]}`
	for _, api := range []string{"anthropic-messages", "openai-responses"} {
		t.Run(api, func(t *testing.T) {
			h, calls := bodyLimitHandler(t, api, false)
			for _, tt := range []struct {
				name, body, kind, message string
				status                    int
			}{
				{"valid", valid, "", "", http.StatusOK},
				{"invalid JSON", `{"model":`, "invalid_request_error", "Invalid JSON payload:", http.StatusBadRequest},
				{"translation error", `{"model":"p/m","thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, "invalid_request_error", "Failed to translate request:", http.StatusBadRequest},
			} {
				t.Run(tt.name, func(t *testing.T) {
					w := httptest.NewRecorder()
					serveMessagesWithBodyLimit(h, w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tt.body)))
					if tt.status == http.StatusOK {
						if w.Code != http.StatusOK {
							t.Fatalf("status = %d: %s", w.Code, w.Body.String())
						}
					} else {
						assertMessagesError(t, w, tt.status, tt.kind, tt.message)
					}
				})
			}
			w := httptest.NewRecorder()
			serveMessagesWithBodyLimit(h, w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(valid+` {}`)))
			if api == "openai-responses" {
				assertMessagesError(t, w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON payload: trailing data")
			} else if w.Code != http.StatusOK {
				t.Fatalf("native trailing data: %d %s", w.Code, w.Body.String())
			}
			wantCalls := int64(1)
			if api == "anthropic-messages" {
				wantCalls++
			}
			if got := calls.Load(); got != wantCalls {
				t.Fatalf("forwarded %d requests, want %d", got, wantCalls)
			}
			methodResponse := httptest.NewRecorder()
			serveMessagesWithBodyLimit(h, methodResponse, httptest.NewRequest(http.MethodGet, "/v1/messages", nil))
			assertMessagesError(t, methodResponse, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")

			failingHandler, failingCalls := bodyLimitHandler(t, api, true)
			providerResponse := httptest.NewRecorder()
			serveMessagesWithBodyLimit(failingHandler, providerResponse, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(valid)))
			assertMessagesError(t, providerResponse, http.StatusForbidden, "permission_error", "provider denied")
			if got := failingCalls.Load(); got != 1 {
				t.Fatalf("forwarded %d requests to failing provider, want 1", got)
			}
		})
	}
}

type failingMessagesBody struct{}

func (failingMessagesBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingMessagesBody) Close() error             { return nil }

type messagesBodyBytesAndError struct {
	data []byte
}

func (body *messagesBodyBytesAndError) Read(buffer []byte) (int, error) {
	count := copy(buffer, body.data)
	body.data = body.data[count:]
	if len(body.data) == 0 {
		return count, io.ErrUnexpectedEOF
	}
	return count, nil
}

func TestMessagesBodyLimitRejectsReadErrors(t *testing.T) {
	const valid = `{"model":"p/m","messages":[{"role":"user","content":"hi"}]}`
	for _, api := range []string{"anthropic-messages", "openai-responses"} {
		t.Run(api, func(t *testing.T) {
			handler, calls := bodyLimitHandler(t, api, false)
			for _, test := range []struct {
				name string
				body io.Reader
			}{
				{"before JSON", failingMessagesBody{}},
				{"during JSON", io.MultiReader(strings.NewReader(`{"model":`), failingMessagesBody{})},
				{"after JSON", io.MultiReader(strings.NewReader(valid), failingMessagesBody{})},
				{"bytes and error", &messagesBodyBytesAndError{data: []byte(valid)}},
			} {
				t.Run(test.name, func(t *testing.T) {
					request := httptest.NewRequest(http.MethodPost, "/v1/messages", test.body)
					response := httptest.NewRecorder()
					serveMessagesWithBodyLimit(handler, response, request)
					assertMessagesError(t, response, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
				})
			}
			if calls.Load() != 0 {
				t.Fatalf("forwarded %d requests with body read errors", calls.Load())
			}
		})
	}
}

func TestMessagesRejectsProviderErrorWithHTTP200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"provider failed"}}`)
	}))
	defer server.Close()
	endpoint := config.Endpoint{API: "anthropic-messages", URL: server.URL, Models: map[string]config.ModelConf{"m": {Target: "model"}}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{endpoint}}}})
	gateway.RegisterOutboundAdapter("anthropic-messages", outboundanthropic.NewAdapter())
	w := httptest.NewRecorder()
	NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/m","messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "provider failed") {
		t.Fatalf("response = %d %s", w.Code, w.Body)
	}
}
