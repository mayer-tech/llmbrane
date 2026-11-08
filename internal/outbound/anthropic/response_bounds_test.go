package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

type boundsTransport func(*http.Request) (*http.Response, error)

func (f boundsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type boundsBody struct {
	reader io.Reader
	read   int64
	closed bool
}

func (b *boundsBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *boundsBody) Close() error {
	b.closed = true
	return nil
}

type boundsFill byte

func (b boundsFill) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

type boundsReadError struct{ err error }

func (r boundsReadError) Read([]byte) (int, error) { return 0, r.err }

func boundsAdapter(status int, body *boundsBody, headers http.Header) *Adapter {
	return NewAdapterWithPolicy(0, boundsTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: headers, Body: body}, nil
	}), httpretry.Policy{})
}

func boundsRequest(t *testing.T, adapter *Adapter, stream bool) error {
	t.Helper()
	req := &canonical.CanonicalChatRequest{}
	endpoint := config.Endpoint{URL: "http://provider.test/messages"}
	if stream {
		_, err := adapter.ProcessChatStream(context.Background(), req, "test-model", endpoint)
		return err
	}
	_, err := adapter.ProcessChat(context.Background(), req, "test-model", endpoint)
	return err
}

func TestNonStreamResponseBodyBounds(t *testing.T) {
	const response = `{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[]}`
	if maxResponseBodyBytes != 40<<20 {
		t.Fatalf("response body limit = %d; want 40 MiB", maxResponseBodyBytes)
	}
	for _, tc := range []struct {
		name      string
		size      int64
		wantError bool
	}{
		{"small", int64(len(response)), false},
		{"exact limit", maxResponseBodyBytes, false},
		{"over limit", maxResponseBodyBytes + 1, true},
		{"much larger", 3 * maxResponseBodyBytes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &boundsBody{reader: io.MultiReader(strings.NewReader(response), io.LimitReader(boundsFill(' '), tc.size-int64(len(response))))}
			adapter := boundsAdapter(http.StatusOK, body, nil)
			got, err := adapter.ProcessChat(context.Background(), &canonical.CanonicalChatRequest{}, "test-model", config.Endpoint{URL: "http://provider.test/messages"})
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "response body exceeds") || got != nil {
					t.Fatalf("response = %v, error = %v; want size error", got, err)
				}
			} else if err != nil || got == nil || got.ID != "msg_1" || int64(len(got.Raw)) != tc.size {
				t.Fatalf("response = %v, error = %v", got, err)
			}
			wantRead := min(tc.size, int64(maxResponseBodyBytes+1))
			if body.read != wantRead || !body.closed {
				t.Fatalf("read = %d, closed = %v; want %d bytes and closed", body.read, body.closed, wantRead)
			}
		})
	}
}

func TestProviderErrorResponseBodyBounds(t *testing.T) {
	const jsonError = `{"type":"error","error":{"message":"slow down"}}`
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			prefix     string
			suffix     string
			fill       byte
			size       int64
			wantParsed bool
		}{
			{"small JSON", jsonError, "", ' ', int64(len(jsonError)), true},
			{"small plain text", "provider unavailable", "", ' ', int64(len("provider unavailable")), false},
			{"exact limit", jsonError, "", ' ', maxResponseBodyBytes, true},
			{"over limit", jsonError, "", ' ', maxResponseBodyBytes + 1, true},
			{"truncated JSON", `{"error":{"message":"`, `"}}`, 'x', maxResponseBodyBytes + 10, false},
			{"large plain text", "provider: ", "", 'x', 3 * maxResponseBodyBytes, false},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				padding := tc.size - int64(len(tc.prefix)) - int64(len(tc.suffix))
				body := &boundsBody{reader: io.MultiReader(strings.NewReader(tc.prefix), io.LimitReader(boundsFill(tc.fill), padding), strings.NewReader(tc.suffix))}
				headers := http.Header{"Retry-After": []string{"17"}}
				err := boundsRequest(t, boundsAdapter(http.StatusTooManyRequests, body, headers), stream)
				var got *canonical.GatewayError
				if !errors.As(err, &got) {
					t.Fatalf("error = %v; want GatewayError", err)
				}
				if got.Provider != "anthropic" || got.Model != "test-model" || got.StatusCode != http.StatusTooManyRequests || got.ErrorKind != canonical.ErrorKindRateLimit || got.RetryAfter != 17 {
					t.Fatalf("unexpected gateway error metadata: %+v", got)
				}
				wantRaw := min(tc.size, int64(maxResponseBodyBytes))
				if int64(len(got.Raw)) != wantRaw || !strings.HasPrefix(string(got.Raw[:len(tc.prefix)]), tc.prefix) {
					t.Fatalf("raw length = %d, want %d with prefix %q", len(got.Raw), wantRaw, tc.prefix)
				}
				if !stream && tc.size > maxResponseBodyBytes {
					if got.Message != "provider error response was incomplete or too large" {
						t.Fatalf("message = %q; want bounded generic error", got.Message)
					}
				} else if tc.wantParsed {
					if got.Message != "slow down" {
						t.Fatalf("message = %q; want parsed provider message", got.Message)
					}
				} else if len(got.Message) != len(got.Raw) || !strings.HasPrefix(got.Message, tc.prefix) {
					t.Fatalf("message length = %d; want raw body", len(got.Message))
				}
				wantRead := min(tc.size, int64(maxResponseBodyBytes+1))
				if body.read != wantRead || !body.closed {
					t.Fatalf("read = %d, closed = %v; want %d bytes and closed", body.read, body.closed, wantRead)
				}
			})
		}
	}
}

func TestResponseReadErrorHandling(t *testing.T) {
	readErr := errors.New("response read failed")
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		for _, stream := range []bool{false, true} {
			if status == http.StatusOK && stream {
				continue
			}
			t.Run(fmt.Sprintf("status=%d/stream=%t", status, stream), func(t *testing.T) {
				body := &boundsBody{reader: io.MultiReader(strings.NewReader("partial"), boundsReadError{readErr})}
				err := boundsRequest(t, boundsAdapter(status, body, nil), stream)
				if stream {
					var got *canonical.GatewayError
					if !errors.As(err, &got) || got.StatusCode != status || got.Message != "partial" || string(got.Raw) != "partial" {
						t.Fatalf("error = %v; want provider error with partial body", err)
					}
				} else {
					var got *canonical.GatewayError
					if !errors.As(err, &got) || (status == http.StatusOK && (got.StatusCode != http.StatusBadGateway || got.ErrorKind != canonical.ErrorKindProviderError)) || (status != http.StatusOK && (got.StatusCode != status || got.Message != "provider error response was incomplete or too large")) {
						t.Fatalf("error = %v; want classified provider error", err)
					}
				}
				if !body.closed {
					t.Fatal("response body was not closed")
				}
			})
		}
	}
}

func TestNonStreamResponseEnvelopeValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"provider error", `{"type":"error","error":{"message":"provider failed"}}`, "provider failed"},
		{"empty object", `{}`, "invalid provider message"},
		{"malformed", `{`, "invalid provider response"},
		{"missing type", `{"id":"m","role":"assistant","model":"model","content":[]}`, "invalid provider message"},
		{"wrong type", `{"id":"m","type":"other","role":"assistant","model":"model","content":[]}`, "invalid provider message"},
		{"missing role", `{"id":"m","type":"message","model":"model","content":[]}`, "invalid provider message"},
		{"missing id", `{"type":"message","role":"assistant","model":"model","content":[]}`, "invalid provider message"},
		{"missing model", `{"id":"m","type":"message","role":"assistant","content":[]}`, "invalid provider message"},
		{"missing content", `{"id":"m","type":"message","role":"assistant","model":"model"}`, "invalid provider message"},
		{"null content", `{"id":"m","type":"message","role":"assistant","model":"model","content":null}`, "invalid provider message"},
		{"non-array content", `{"id":"m","type":"message","role":"assistant","model":"model","content":{}}`, "invalid provider response"},
		{"empty content", `{"id":"m","type":"message","role":"assistant","model":"model","content":[]}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &boundsBody{reader: strings.NewReader(test.body)}
			err := boundsRequest(t, boundsAdapter(http.StatusOK, body, nil), false)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var gatewayError *canonical.GatewayError
			if !errors.As(err, &gatewayError) || gatewayError.StatusCode != http.StatusBadGateway || gatewayError.ErrorKind != canonical.ErrorKindProviderError || gatewayError.Message != test.want {
				t.Fatalf("error = %v; want 502 provider error %q", err, test.want)
			}
		})
	}
}

type blockingResponseBody struct {
	closed chan struct{}
	once   sync.Once
}

func (body *blockingResponseBody) Read([]byte) (int, error) {
	<-body.closed
	return 0, io.ErrClosedPipe
}

func (body *blockingResponseBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

func TestNonStreamResponseIdleTimeoutAndCancellation(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		for _, cancelRequest := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/cancel=%t", status, cancelRequest), func(t *testing.T) {
				body := &blockingResponseBody{closed: make(chan struct{})}
				adapter := NewAdapterWithPolicy(30*time.Millisecond, boundsTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"17"}}, Body: body}, nil
				}), httpretry.Policy{})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if cancelRequest {
					time.AfterFunc(5*time.Millisecond, cancel)
				}
				start := time.Now()
				_, err := adapter.ProcessChat(ctx, &canonical.CanonicalChatRequest{}, "model", config.Endpoint{URL: "http://provider.test"})
				var gatewayError *canonical.GatewayError
				if !errors.As(err, &gatewayError) || (status == http.StatusOK && gatewayError.StatusCode != http.StatusBadGateway) || (status != http.StatusOK && (gatewayError.StatusCode != status || gatewayError.RetryAfter != 17)) {
					t.Fatalf("error = %v", err)
				}
				if time.Since(start) > time.Second {
					t.Fatal("stalled response was not interrupted promptly")
				}
				select {
				case <-body.closed:
				default:
					t.Fatal("response body was not closed")
				}
			})
		}
	}
}

type slowResponseBody struct {
	reader io.Reader
}

func (body *slowResponseBody) Read(buffer []byte) (int, error) {
	time.Sleep(5 * time.Millisecond)
	if len(buffer) > 16 {
		buffer = buffer[:16]
	}
	return body.reader.Read(buffer)
}

func (body *slowResponseBody) Close() error { return nil }

func TestNonStreamResponseIdleTimeoutAllowsProgress(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
	}{
		{http.StatusOK, `{"id":"m","type":"message","role":"assistant","model":"model","content":[]}`},
		{http.StatusTooManyRequests, `{"type":"error","error":{"message":"rate limited"}}`},
	} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			adapter := NewAdapterWithPolicy(100*time.Millisecond, boundsTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: &slowResponseBody{reader: strings.NewReader(test.body)}}, nil
			}), httpretry.Policy{})
			err := boundsRequest(t, adapter, false)
			if test.status == http.StatusOK && err != nil {
				t.Fatal(err)
			}
			if test.status != http.StatusOK {
				var gatewayError *canonical.GatewayError
				if !errors.As(err, &gatewayError) || gatewayError.StatusCode != test.status || gatewayError.Message != "rate limited" {
					t.Fatalf("error = %v", err)
				}
			}
		})
	}
}

type boundsRepeatingReader struct {
	pattern string
	offset  int
}

func (r *boundsRepeatingReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		copied := copy(p[n:], r.pattern[r.offset:])
		n += copied
		r.offset = (r.offset + copied) % len(r.pattern)
	}
	return n, nil
}

func TestSuccessfulStreamIsNotSubjectToResponseBodyLimit(t *testing.T) {
	pattern := ":" + strings.Repeat(" ", 32<<10) + "\n"
	padding := int64(len(pattern)) * (int64(maxResponseBodyBytes)/int64(len(pattern)) + 1)
	const events = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	body := &boundsBody{reader: io.MultiReader(io.LimitReader(&boundsRepeatingReader{pattern: pattern}, padding), strings.NewReader(events))}
	adapter := boundsAdapter(http.StatusOK, body, nil)
	ch, err := adapter.ProcessChatStream(context.Background(), &canonical.CanonicalChatRequest{}, "test-model", config.Endpoint{URL: "http://provider.test/messages"})
	if err != nil {
		t.Fatal(err)
	}
	var done bool
	for event := range ch {
		if event.Type == "error" {
			t.Fatalf("stream error: %v", event.Error)
		}
		if event.Type == "done" {
			done = true
		}
	}
	if !done || body.read <= maxResponseBodyBytes || !body.closed {
		t.Fatalf("done = %v, read = %d, closed = %v", done, body.read, body.closed)
	}
}
