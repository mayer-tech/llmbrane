package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go/eventstream"
	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	outanthropic "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
	"github.com/mayer-tech/llmbrane/internal/outbound/bedrock"
	"github.com/mayer-tech/llmbrane/internal/outbound/openai"
	"github.com/mayer-tech/llmbrane/internal/outbound/openairesponses"
	"github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

const streamingErrorRequest = `{"model":"p/m","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

type streamErrorAdapter struct {
	events []*canonical.CanonicalChatStreamEvent
	err    error
}

func (a *streamErrorAdapter) ProcessChat(context.Context, *canonical.CanonicalChatRequest, string, config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	return nil, errors.New("unexpected non-streaming request")
}

func (a *streamErrorAdapter) ProcessChatStream(context.Context, *canonical.CanonicalChatRequest, string, config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	if a.err != nil {
		return nil, a.err
	}
	stream := make(chan *canonical.CanonicalChatStreamEvent, len(a.events))
	for _, event := range a.events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

func streamingErrorHandler(api, url string, adapter core.OutboundAdapter) *Handler {
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{
		"p": {Endpoints: []config.Endpoint{{
			API: api, URL: url, Capabilities: []string{"streaming"},
			Models: map[string]config.ModelConf{"m": {Target: "provider-model"}},
		}}},
	}})
	gateway.RegisterOutboundAdapter(api, adapter)
	return NewHandler(gateway)
}

func invokeStreamingErrorHandler(h *Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(streamingErrorRequest)))
	return w
}

func assertHTTPStreamError(t *testing.T, w *httptest.ResponseRecorder, status int, errType, message, retryAfter string) {
	t.Helper()
	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != status || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected response: status=%d headers=%v body=%s", res.StatusCode, res.Header, w.Body.String())
	}
	if res.Header.Get("Cache-Control") != "" || res.Header.Get("Connection") != "" || res.Header.Get("Retry-After") != retryAfter || w.Flushed {
		t.Fatalf("unexpected error headers or flush: headers=%v flushed=%t", res.Header, w.Flushed)
	}
	var body ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON error: %v: %s", err, w.Body.String())
	}
	if body.Type != "error" || body.Error.Type != errType || (message != "" && body.Error.Message != message) {
		t.Fatalf("unexpected error body: %+v", body)
	}
}

func TestStreamErrorBeforeFirstSSEEvent(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		status     int
		errType    string
		eventType  string
		raw        string
		retryAfter int
	}{
		{"anthropic invalid request", "anthropic", 400, "invalid_request_error", "error", "", 0},
		{"openai authentication", "openai", 401, "authentication_error", "error", "", 0},
		{"bedrock permission without event type", "bedrock", 403, "permission_error", "", "", 0},
		{"responses not found", "openai-responses", 404, "not_found_error", "error", "", 0},
		{"request too large", "openai", 413, "request_too_large", "error", "", 0},
		{"rate limit", "anthropic", 429, "rate_limit_error", "error", "", 7},
		{"unknown client error", "openai", 422, "api_error", "error", "", 0},
		{"bedrock server error", "bedrock", 500, "api_error", "", "", 0},
		{"generic unavailable", "openai", 503, "api_error", "error", "", 0},
		{"anthropic overloaded event", "anthropic", 503, "overloaded_error", "error", `{"type":"error","error":{"type":"overloaded_error","message":"failure"}}`, 0},
		{"anthropic overloaded status", "anthropic", 529, "overloaded_error", "error", "", 0},
		{"redirect error", "openai", 302, "api_error", "error", "", 0},
		{"nonstandard status", "openai", 600, "api_error", "error", "", 0},
		{"unknown status", "unknown", 0, "api_error", "error", "", 0},
		{"success status on error", "unknown", 200, "api_error", "error", "", 0},
		{"invalid status", "unknown", 1000, "api_error", "error", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gwErr := &canonical.GatewayError{Provider: tc.provider, StatusCode: tc.status, Message: "failure", RetryAfter: tc.retryAfter, Raw: []byte(tc.raw)}
			adapter := &streamErrorAdapter{events: []*canonical.CanonicalChatStreamEvent{
				nil,
				{Type: "content_delta", Delta: &canonical.CanonicalContentPart{Type: "reasoning"}}, // No Anthropic frame is emitted.
				{Type: tc.eventType, Error: gwErr},
				{Type: "done"},
			}}
			w := invokeStreamingErrorHandler(streamingErrorHandler("test", "", adapter))
			status := tc.status
			if status < 300 || status > 999 {
				status = http.StatusBadGateway
			}
			retryAfter := ""
			if tc.retryAfter > 0 {
				retryAfter = fmt.Sprint(tc.retryAfter)
			}
			assertHTTPStreamError(t, w, status, tc.errType, "failure", retryAfter)
		})
	}
}

func TestStreamErrorAfterFirstSSEEvent(t *testing.T) {
	for _, tc := range []struct {
		name, provider, eventType, errType, raw string
		status                                  int
	}{
		{"bedrock", "bedrock", "", "permission_error", "", 403},
		{"anthropic", "anthropic", "error", "rate_limit_error", "", 429},
		{"anthropic overloaded", "anthropic", "error", "overloaded_error", `{"type":"error","error":{"type":"overloaded_error"}}`, 503},
		{"openai", "openai", "error", "api_error", "", 500},
		{"responses", "openai-responses", "error", "not_found_error", "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &streamErrorAdapter{events: []*canonical.CanonicalChatStreamEvent{
				{Type: "message_start", ResponseID: "msg"},
				{Type: tc.eventType, Error: &canonical.GatewayError{Provider: tc.provider, StatusCode: tc.status, Message: "failure", RetryAfter: 7, Raw: []byte(tc.raw)}},
				{Type: "done"},
			}}
			w := invokeStreamingErrorHandler(streamingErrorHandler("test", "", adapter))
			res := w.Result()
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("Retry-After") != "" || !w.Flushed {
				t.Fatalf("unexpected stream response: status=%d headers=%v", res.StatusCode, res.Header)
			}
			frames := strings.Split(strings.TrimSpace(w.Body.String()), "\n\n")
			if len(frames) != 2 || !strings.HasPrefix(frames[0], "event: message_start\n") || !strings.HasPrefix(frames[1], "event: error\ndata: ") {
				t.Fatalf("unexpected stream frames: %s", w.Body.String())
			}
			var event StreamEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[1], "event: error\ndata: ")), &event); err != nil {
				t.Fatal(err)
			}
			if event.Type != "error" || event.Error == nil || event.Error.Type != tc.errType || event.Error.Message != "failure" {
				t.Fatalf("unexpected error event: %+v", event)
			}
		})
	}
}

func TestStreamErrorMissingDetails(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%t", started), func(t *testing.T) {
			var events []*canonical.CanonicalChatStreamEvent
			if started {
				events = append(events, &canonical.CanonicalChatStreamEvent{Type: "message_start"})
			}
			events = append(events, &canonical.CanonicalChatStreamEvent{Type: "error"}, &canonical.CanonicalChatStreamEvent{Type: "done"})
			w := invokeStreamingErrorHandler(streamingErrorHandler("test", "", &streamErrorAdapter{events: events}))
			if !started {
				assertHTTPStreamError(t, w, http.StatusBadGateway, "api_error", "provider stream reported an unspecified error", "")
			} else if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"type":"api_error"`) || !strings.Contains(w.Body.String(), "event: error\n") || strings.Contains(w.Body.String(), "event: message_stop\n") {
				t.Fatalf("unexpected stream: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestStreamInitialGatewayError(t *testing.T) {
	gwErr := &canonical.GatewayError{StatusCode: http.StatusTooManyRequests, Message: "rate limited", RetryAfter: 11}
	w := invokeStreamingErrorHandler(streamingErrorHandler("test", "", &streamErrorAdapter{err: fmt.Errorf("wrapped: %w", gwErr)}))
	assertHTTPStreamError(t, w, http.StatusTooManyRequests, "rate_limit_error", "rate limited", "11")
}

func TestProviderStreamErrorsBeforeFirstSSEEvent(t *testing.T) {
	policy := retry.Policy{}
	var bedrockException bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&bedrockException, eventstream.Message{
		Headers: eventstream.Headers{{Name: ":event-type", Value: eventstream.StringValue("throttlingException")}},
		Payload: []byte(`{"message":"throttled"}`),
	}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, api, upstreamBody string
		adapter                 core.OutboundAdapter
		status                  int
		errType                 string
	}{
		{"anthropic permission", "anthropic-messages", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"permission_error\",\"message\":\"denied\"}}\n\n", outanthropic.NewAdapterWithPolicy(time.Second, nil, policy), 403, "permission_error"},
		{"anthropic overloaded", "anthropic-messages", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n", outanthropic.NewAdapterWithPolicy(time.Second, nil, policy), 503, "overloaded_error"},
		{"openai", "openai-chat", "data: {\"error\":{\"message\":\"denied\",\"type\":\"invalid_request_error\"}}\n\n", openai.NewAdapterWithPolicy(time.Second, nil, policy), 400, "invalid_request_error"},
		{"responses", "openai-responses", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"failed\"}}}\n\n", openairesponses.NewAdapterWithPolicy(time.Second, nil, policy), 502, "api_error"},
		{"bedrock exception", "bedrock-converse", bedrockException.String(), bedrock.NewAdapterWithPolicy(time.Second, nil, policy), 500, "api_error"},
		{"bedrock malformed eventstream", "bedrock-converse", string([]byte{0, 0, 0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}), bedrock.NewAdapterWithPolicy(time.Second, nil, policy), 500, "api_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.upstreamBody))
			}))
			defer server.Close()
			w := invokeStreamingErrorHandler(streamingErrorHandler(tc.api, server.URL, tc.adapter))
			assertHTTPStreamError(t, w, tc.status, tc.errType, "", "")
		})
	}
}

func TestProviderInitialHTTPErrorPreservesStatusAndRetryAfter(t *testing.T) {
	policy := retry.Policy{}
	for _, tc := range []struct {
		api     string
		adapter core.OutboundAdapter
	}{
		{"anthropic-messages", outanthropic.NewAdapterWithPolicy(time.Second, nil, policy)},
		{"openai-chat", openai.NewAdapterWithPolicy(time.Second, nil, policy)},
		{"openai-responses", openairesponses.NewAdapterWithPolicy(time.Second, nil, policy)},
		{"bedrock-converse", bedrock.NewAdapterWithPolicy(time.Second, nil, policy)},
	} {
		t.Run(tc.api, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "9")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			}))
			defer server.Close()
			w := invokeStreamingErrorHandler(streamingErrorHandler(tc.api, server.URL, tc.adapter))
			assertHTTPStreamError(t, w, http.StatusTooManyRequests, "rate_limit_error", "", "9")
		})
	}
}
