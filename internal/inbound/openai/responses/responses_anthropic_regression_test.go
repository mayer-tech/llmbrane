package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestResponsesAnthropicPreStreamProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, message, kind string
		status                    int
	}{
		{"rate limit", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n", "slow down", "rate_limit", http.StatusTooManyRequests},
		{"overloaded", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"try later\"}}\n\n", "try later", "provider_error", http.StatusServiceUnavailable},
		{"malformed", "data: {invalid}\n\n", "failed to parse anthropic stream event", "provider_error", http.StatusBadGateway},
		{"empty", "", "anthropic stream ended before message_stop", "provider_error", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer backend.Close()
			w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": "hello", "stream": true})
			if w.Code != tc.status || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status %d, headers %v, body %s", w.Code, w.Header(), w.Body)
			}
			if w.Header().Get("Cache-Control") != "" || w.Header().Get("X-Accel-Buffering") != "" {
				t.Fatalf("unexpected SSE headers: %v", w.Header())
			}
			body := decodeObject(t, w.Body.Bytes())
			detail, ok := body["error"].(map[string]any)
			if !ok || detail["type"] != tc.kind || !strings.Contains(fmt.Sprint(detail["message"]), tc.message) {
				t.Fatalf("unexpected error: %s", w.Body)
			}
		})
	}
}

func TestResponsesAnthropicProviderErrorAfterFirstEvent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n")
	}))
	defer backend.Close()
	w := postResponses(anthropicResponsesHandler(backend.URL), map[string]any{"model": "p/m", "input": "hello", "stream": true})
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), "event: response.created\n") || !strings.Contains(w.Body.String(), "event: error\n") || !strings.Contains(w.Body.String(), "slow down") || strings.Contains(w.Body.String(), "event: response.completed\n") {
		t.Fatalf("status %d, headers %v, body %s", w.Code, w.Header(), w.Body)
	}
}

func TestResponsesAnthropicStreamErrorBoundary(t *testing.T) {
	providerError := &canonical.GatewayError{StatusCode: http.StatusTooManyRequests, ErrorKind: canonical.ErrorKindRateLimit, Message: "slow down", RetryAfter: 7}
	for _, tc := range []struct {
		name   string
		events []*canonical.CanonicalChatStreamEvent
		status int
		body   string
	}{
		{"provider error", []*canonical.CanonicalChatStreamEvent{nil, {Type: "error", Error: providerError}}, http.StatusTooManyRequests, "slow down"},
		{"missing error", []*canonical.CanonicalChatStreamEvent{{Type: "error"}}, http.StatusBadGateway, "provider stream failed"},
		{"invalid status", []*canonical.CanonicalChatStreamEvent{{Type: "error", Error: &canonical.GatewayError{Message: "invalid"}}}, http.StatusBadGateway, "invalid"},
		{"invalid first event", []*canonical.CanonicalChatStreamEvent{{Type: "usage"}}, http.StatusBadGateway, "provider stream has no message_start"},
		{"closed stream", nil, http.StatusBadGateway, "Provider stream ended before completion"},
		{"after first event", []*canonical.CanonicalChatStreamEvent{{Type: "message_start", ResponseID: "msg"}, {Type: "error", Error: providerError}}, http.StatusOK, "provider stream failed: slow down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan *canonical.CanonicalChatStreamEvent, len(tc.events))
			for _, event := range tc.events {
				ch <- event
			}
			close(ch)
			w := httptest.NewRecorder()
			streamAnthropicResponses(context.Background(), w, w, ch, "p/m")
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("status %d, body %s", w.Code, w.Body)
			}
			if tc.status == http.StatusOK {
				if w.Header().Get("Content-Type") != "text/event-stream" || w.Header().Get("Retry-After") != "" || !strings.Contains(w.Body.String(), "event: response.created\n") || !strings.Contains(w.Body.String(), "event: error\n") || strings.Contains(w.Body.String(), "event: response.completed\n") {
					t.Fatalf("headers %v, body %s", w.Header(), w.Body)
				}
			} else {
				if w.Header().Get("Content-Type") != "application/json" || strings.Contains(w.Body.String(), "event: ") {
					t.Fatalf("headers %v, body %s", w.Header(), w.Body)
				}
				if tc.name == "provider error" && (w.Header().Get("Retry-After") != "7" || decodeObject(t, w.Body.Bytes())["error"].(map[string]any)["type"] != "rate_limit") {
					t.Fatalf("headers %v, body %s", w.Header(), w.Body)
				}
			}
		})
	}
}

func TestResponsesAnthropicCancelledBeforeStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan *canonical.CanonicalChatStreamEvent)
	close(ch)
	w := httptest.NewRecorder()
	streamAnthropicResponses(ctx, w, w, ch, "p/m")
	if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
		t.Fatalf("cancelled request wrote headers %v, body %s", w.Header(), w.Body)
	}
}

func TestResponsesToolSchemaValidation(t *testing.T) {
	for _, raw := range []string{
		`null`,
		`{}`,
		`[{"type":"function","name":"f","parameters":42}]`,
		`[{"type":"function","name":"f","parameters":[]}]`,
		`[{"type":"function","name":"f"}] true`,
	} {
		t.Run(raw, func(t *testing.T) {
			fields := map[string]json.RawMessage{
				"model": json.RawMessage(`"p/m"`),
				"input": json.RawMessage(`"hello"`),
				"tools": json.RawMessage(raw),
			}
			if _, err := translateResponsesRequest(fields); err == nil {
				t.Fatalf("accepted tools %s", raw)
			}
		})
	}
}

func TestResponsesAnthropicToolSchemaNumberPrecision(t *testing.T) {
	const schema = `{"type":"object","properties":{"integer":{"const":9007199254740993},"nested":{"enum":[-9007199254740995,1.234567890123456789,1e400,{"minimum":0.000000000000000000123456789}]}}}`
	var expected map[string]any
	decoder := json.NewDecoder(strings.NewReader(schema))
	decoder.UseNumber()
	if err := decoder.Decode(&expected); err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{
		"model": json.RawMessage(`"p/m"`),
		"input": json.RawMessage(`"hello"`),
		"tools": json.RawMessage(`[{"type":"function","name":"f","parameters":` + schema + `}]`),
	}
	req, err := translateResponsesRequest(fields)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Tools[0].InputSchema, expected) {
		t.Fatalf("canonical schema = %#v, want %#v", req.Tools[0].InputSchema, expected)
	}
	if !bytes.Equal(req.Responses.Settings["tools"], fields["tools"]) {
		t.Fatal("native tools settings changed")
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			requests := make(chan []byte, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- body
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
				}
			}))
			defer backend.Close()
			body := fmt.Sprintf(`{"model":"p/m","input":"hello","stream":%t,"tools":[{"type":"function","name":"f","parameters":%s}]}`, stream, schema)
			w := httptest.NewRecorder()
			anthropicResponsesHandler(backend.URL).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(body)))
			if w.Code != http.StatusOK || (stream && !strings.Contains(w.Body.String(), "event: response.completed")) {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var upstream struct {
				Tools []struct {
					InputSchema map[string]any `json:"input_schema"`
				} `json:"tools"`
			}
			decoder := json.NewDecoder(bytes.NewReader(<-requests))
			decoder.UseNumber()
			if err := decoder.Decode(&upstream); err != nil {
				t.Fatal(err)
			}
			if len(upstream.Tools) != 1 || !reflect.DeepEqual(upstream.Tools[0].InputSchema, expected) {
				t.Fatalf("upstream schema = %#v, want %#v", upstream.Tools, expected)
			}
		})
	}
}
