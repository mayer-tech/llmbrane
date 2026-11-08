package chatcompletions

import (
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
	outboundanthropic "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
	"github.com/mayer-tech/llmbrane/internal/outbound/bedrock"
	outboundopenai "github.com/mayer-tech/llmbrane/internal/outbound/openai"
	"github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

const chatFailureRequest = `{"model":"p/m","stream":true,"messages":[{"role":"user","content":"hello"}]}`

type chatFailureAdapter struct {
	events []*canonical.CanonicalChatStreamEvent
	err    error
	start  func(context.Context) <-chan *canonical.CanonicalChatStreamEvent
}

func (a *chatFailureAdapter) ProcessChat(context.Context, *canonical.CanonicalChatRequest, string, config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	return nil, errors.New("unexpected non-streaming request")
}

func (a *chatFailureAdapter) ProcessChatStream(ctx context.Context, _ *canonical.CanonicalChatRequest, _ string, _ config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	if a.err != nil {
		return nil, a.err
	}
	if a.start != nil {
		return a.start(ctx), nil
	}
	ch := make(chan *canonical.CanonicalChatStreamEvent, len(a.events))
	for _, event := range a.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

func chatFailureHandler(api, url string, adapter core.OutboundAdapter) *Handler {
	endpoint := config.Endpoint{API: api, URL: url, Models: map[string]config.ModelConf{"m": {Target: "model"}}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{endpoint}}}})
	gateway.RegisterOutboundAdapter(api, adapter)
	return NewHandler(gateway)
}

func chatFailureInvoke(h *Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.HandleChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatFailureRequest)))
	return w
}

func chatFailureStart(index int) *canonical.CanonicalChatStreamEvent {
	return &canonical.CanonicalChatStreamEvent{Type: "message_start", MessageIndex: index}
}

func chatFailureDelta(index int, text string) *canonical.CanonicalChatStreamEvent {
	return &canonical.CanonicalChatStreamEvent{Type: "content_delta", MessageIndex: index, Delta: &canonical.CanonicalContentPart{Type: "text", Text: text}}
}

func chatFailureStop(index int, reason string) *canonical.CanonicalChatStreamEvent {
	return &canonical.CanonicalChatStreamEvent{Type: "done", MessageIndex: index, Stop: &canonical.CanonicalStop{Reason: reason}}
}

func assertChatStreamFailure(t *testing.T, body, message, kind string) {
	t.Helper()
	if strings.Contains(body, "[DONE]") || strings.Contains(body, `"finish_reason":"`) {
		t.Fatalf("failure contained a success marker: %s", body)
	}
	frames := strings.Split(strings.TrimSpace(body), "\n\n")
	if len(frames) == 0 || !strings.HasPrefix(frames[len(frames)-1], "data: ") {
		t.Fatalf("missing error frame: %s", body)
	}
	var response canonical.OpenAIErrorResponse
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[len(frames)-1], "data: ")), &response); err != nil {
		t.Fatalf("invalid error frame: %v: %s", err, body)
	}
	if response.Error.Message != message || response.Error.Type != kind {
		t.Fatalf("unexpected error: %+v", response.Error)
	}
}

func TestChatStreamFailures(t *testing.T) {
	providerError := &canonical.GatewayError{Message: "provider failed", ErrorKind: canonical.ErrorKindRateLimit, StatusCode: http.StatusTooManyRequests}
	for _, tc := range []struct {
		name    string
		events  []*canonical.CanonicalChatStreamEvent
		message string
		kind    string
		partial bool
	}{
		{"error first", []*canonical.CanonicalChatStreamEvent{{Type: "error", Error: providerError}}, "provider failed", "rate_limit", false},
		{"error after partial", []*canonical.CanonicalChatStreamEvent{chatFailureStart(0), chatFailureDelta(0, "partial"), {Type: "error", Error: providerError}, chatFailureStop(0, "stop")}, "provider failed", "rate_limit", true},
		{"error with no type", []*canonical.CanonicalChatStreamEvent{chatFailureStart(0), {Error: providerError}}, "provider failed", "rate_limit", false},
		{"error without details", []*canonical.CanonicalChatStreamEvent{nil, {Type: "error"}}, "provider stream reported an unspecified error", "provider_error", false},
		{"error after stop", []*canonical.CanonicalChatStreamEvent{chatFailureStart(0), chatFailureStop(0, "stop"), {Type: "error", Error: providerError}}, "provider failed", "rate_limit", false},
		{"content after stop", []*canonical.CanonicalChatStreamEvent{chatFailureStart(0), chatFailureStop(0, "stop"), chatFailureDelta(0, "late")}, "provider stream ended before completion", "provider_error", false},
		{"empty stream", nil, "provider stream ended before completion", "provider_error", false},
		{"premature close", []*canonical.CanonicalChatStreamEvent{chatFailureStart(0), chatFailureDelta(0, "partial")}, "provider stream ended before completion", "provider_error", true},
		{"missing choice completion", []*canonical.CanonicalChatStreamEvent{chatFailureStart(0), chatFailureStart(1), chatFailureStop(0, "stop")}, "provider stream ended before completion", "provider_error", false},
		{"empty finish reason", []*canonical.CanonicalChatStreamEvent{chatFailureStart(0), chatFailureStop(0, "")}, "provider stream ended before completion", "provider_error", false},
		{"usage is not completion", []*canonical.CanonicalChatStreamEvent{chatFailureStart(0), {Type: "done", Usage: &canonical.CanonicalUsage{TotalTokens: 3}}}, "provider stream ended before completion", "provider_error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := chatFailureInvoke(chatFailureHandler("test", "", &chatFailureAdapter{events: tc.events}))
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" || !w.Flushed {
				t.Fatalf("unexpected stream response: %d %v", w.Code, w.Header())
			}
			assertChatStreamFailure(t, w.Body.String(), tc.message, tc.kind)
			if tc.partial && !strings.Contains(w.Body.String(), "partial") {
				t.Fatalf("partial content lost: %s", w.Body)
			}
		})
	}
}

func TestChatStreamCompleteChoicesAndOrder(t *testing.T) {
	events := []*canonical.CanonicalChatStreamEvent{
		chatFailureStart(0), chatFailureStart(1), chatFailureDelta(0, "first"),
		chatFailureStop(0, "stop"), chatFailureDelta(1, "second"), chatFailureStop(1, "length"),
		{Type: "usage", Usage: &canonical.CanonicalUsage{TotalTokens: 5}},
	}
	w := chatFailureInvoke(chatFailureHandler("test", "", &chatFailureAdapter{events: events}))
	body := w.Body.String()
	if w.Code != http.StatusOK || strings.Count(body, "data: [DONE]\n\n") != 1 || strings.Contains(body, `"error":`) {
		t.Fatalf("unexpected complete stream: %d %s", w.Code, body)
	}
	first := strings.Index(body, `"content":"first"`)
	stop := strings.Index(body, `"finish_reason":"stop"`)
	second := strings.Index(body, `"content":"second"`)
	length := strings.Index(body, `"finish_reason":"length"`)
	done := strings.Index(body, "[DONE]")
	if first < 0 || !(first < stop && stop < second && second < length && length < done) {
		t.Fatalf("stream order changed: %s", body)
	}
}

func TestChatNativeOpenAIStreams(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		failure    bool
	}{
		{"complete", "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
			"data: [DONE]\n\n", false},
		{"provider error after finish", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"error\":{\"message\":\"upstream failed\",\"type\":\"invalid_request_error\"}}\n\n", true},
		{"truncated before finish", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			adapter := outboundopenai.NewAdapterWithPolicy(time.Second, nil, retry.Policy{})
			w := chatFailureInvoke(chatFailureHandler("openai-chat", server.URL, adapter))
			if !strings.Contains(w.Body.String(), `"content":"hello"`) {
				t.Fatalf("missing content: %s", w.Body)
			}
			if tc.failure {
				message, kind := "provider stream ended before completion", "provider_error"
				if tc.name == "provider error after finish" {
					message, kind = "upstream failed", "invalid_request"
				}
				assertChatStreamFailure(t, w.Body.String(), message, kind)
			} else if strings.Count(w.Body.String(), `"finish_reason":"stop"`) != 1 || strings.Count(w.Body.String(), "data: [DONE]\n\n") != 1 {
				t.Fatalf("native stream changed: %s", w.Body)
			}
		})
	}
}

func TestChatAnthropicTruncatedAfterStopReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\"}}\n\n"+
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"+
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	}))
	defer server.Close()
	adapter := outboundanthropic.NewAdapterWithPolicy(time.Second, nil, retry.Policy{})
	w := chatFailureInvoke(chatFailureHandler("anthropic-messages", server.URL, adapter))
	if !strings.Contains(w.Body.String(), `"content":"hello"`) {
		t.Fatalf("missing partial content: %s", w.Body)
	}
	assertChatStreamFailure(t, w.Body.String(), "anthropic stream ended before message_stop", "provider_error")
}

func TestChatBedrockStopAndLateError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		error bool
	}{
		{"complete", false},
		{"exception after stop", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				encoder := eventstream.NewEncoder()
				send := func(kind, payload string) {
					_ = encoder.Encode(w, eventstream.Message{
						Headers: eventstream.Headers{{Name: ":event-type", Value: eventstream.StringValue(kind)}},
						Payload: []byte(payload),
					})
				}
				send("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hello"}}`)
				send("messageStop", `{"stopReason":"end_turn"}`)
				if tc.error {
					send("modelStreamErrorException", `{"message":"failed"}`)
				} else {
					send("metadata", `{"usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`)
				}
			}))
			defer server.Close()
			adapter := bedrock.NewAdapterWithPolicy(time.Second, nil, retry.Policy{})
			w := chatFailureInvoke(chatFailureHandler("bedrock-converse", server.URL, adapter))
			if !strings.Contains(w.Body.String(), `"content":"hello"`) {
				t.Fatalf("missing content: %s", w.Body)
			}
			if tc.error {
				assertChatStreamFailure(t, w.Body.String(), "modelStreamErrorException: failed", "provider_error")
			} else if strings.Count(w.Body.String(), `"finish_reason":"stop"`) != 1 || strings.Count(w.Body.String(), "data: [DONE]\n\n") != 1 {
				t.Fatalf("complete stream changed: %s", w.Body)
			}
		})
	}
}

func TestChatStreamInitialError(t *testing.T) {
	gwErr := &canonical.GatewayError{StatusCode: http.StatusTooManyRequests, ErrorKind: canonical.ErrorKindRateLimit, Message: "limited", RetryAfter: 7}
	w := chatFailureInvoke(chatFailureHandler("test", "", &chatFailureAdapter{err: gwErr}))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Retry-After") != "7" || w.Flushed {
		t.Fatalf("unexpected initial error: %d %v %s", w.Code, w.Header(), w.Body)
	}
}

type chatFailureWriter struct {
	header   http.Header
	body     strings.Builder
	writes   int
	flushes  int
	writeErr bool
	short    bool
	flushErr bool
	onWrite  func()
}

func (w *chatFailureWriter) Header() http.Header { return w.header }
func (w *chatFailureWriter) WriteHeader(int)     {}
func (w *chatFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.onWrite != nil {
		w.onWrite()
	}
	if w.writeErr {
		return 0, errors.New("write failed")
	}
	if w.short {
		return 0, nil
	}
	return w.body.Write(p)
}
func (w *chatFailureWriter) Flush() { w.flushes++ }
func (w *chatFailureWriter) FlushError() error {
	w.flushes++
	if w.flushErr {
		return errors.New("flush failed")
	}
	return nil
}

func waitChatFailure(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream cleanup")
	}
}

func TestChatStreamDownstreamFailuresCancelAndDrain(t *testing.T) {
	for _, mode := range []string{"write", "short write", "flush", "cancel on write"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan struct{})
			adapter := &chatFailureAdapter{start: func(providerCtx context.Context) <-chan *canonical.CanonicalChatStreamEvent {
				ch := make(chan *canonical.CanonicalChatStreamEvent)
				go func() {
					defer close(finished)
					defer close(ch)
					ch <- chatFailureStart(0)
					<-providerCtx.Done()
					// Simulate a provider or gateway wrapper with blocking sends.
					ch <- chatFailureStop(0, "stop")
				}()
				return ch
			}}
			w := &chatFailureWriter{header: make(http.Header), writeErr: mode == "write", short: mode == "short write", flushErr: mode == "flush"}
			if mode == "cancel on write" {
				w.onWrite = cancel
			}
			h := chatFailureHandler("test", "", adapter)
			h.HandleChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatFailureRequest)).WithContext(ctx))
			waitChatFailure(t, finished)
			if w.writes != 1 || strings.Contains(w.body.String(), "[DONE]") || strings.Contains(w.body.String(), `"finish_reason":"`) {
				t.Fatalf("wrote after downstream failure: writes=%d body=%s", w.writes, w.body.String())
			}
			if mode != "flush" && w.flushes != 0 {
				t.Fatalf("flushed after failure: %d", w.flushes)
			}
		})
	}
}

func TestChatStreamTerminalWriteFailureOmitsDone(t *testing.T) {
	for _, mode := range []string{"write", "flush", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &chatFailureWriter{header: make(http.Header)}
			w.onWrite = func() {
				if w.writes == 1 && mode == "cancel" {
					cancel()
				}
			}
			w.writeErr = mode == "write"
			w.flushErr = mode == "flush"
			h := chatFailureHandler("test", "", &chatFailureAdapter{events: []*canonical.CanonicalChatStreamEvent{chatFailureStop(0, "stop")}})
			h.HandleChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatFailureRequest)).WithContext(ctx))
			if w.writes != 1 || strings.Contains(w.body.String(), "[DONE]") {
				t.Fatalf("wrote DONE after terminal write failure: writes=%d body=%s", w.writes, w.body.String())
			}
		})
	}
}

func TestChatStreamCancellationWhileWaitingAfterStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSent := make(chan struct{})
	providerDone := make(chan struct{})
	adapter := &chatFailureAdapter{start: func(providerCtx context.Context) <-chan *canonical.CanonicalChatStreamEvent {
		ch := make(chan *canonical.CanonicalChatStreamEvent)
		go func() {
			defer close(providerDone)
			defer close(ch)
			ch <- chatFailureStart(0)
			ch <- chatFailureStop(0, "stop")
			close(stopSent)
			<-providerCtx.Done()
		}()
		return ch
	}}
	w := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		chatFailureHandler("test", "", adapter).HandleChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatFailureRequest)).WithContext(ctx))
	}()
	waitChatFailure(t, stopSent)
	cancel()
	waitChatFailure(t, handlerDone)
	waitChatFailure(t, providerDone)
	if strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), `"finish_reason":"`) || strings.Contains(w.Body.String(), `"error":`) {
		t.Fatalf("canceled stream emitted a terminal frame: %s", w.Body)
	}
}
