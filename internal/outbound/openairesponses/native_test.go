package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

func TestNativeBodySettings(t *testing.T) {
	temp, top := 0.4, 0.9
	limit := 300
	effort := "high"
	req := &canonical.CanonicalChatRequest{
		Stream: true, Params: canonical.CanonicalParams{Temperature: &temp, TopP: &top, MaxOutputTokens: &limit, ReasoningEffort: &effort},
		Responses: &canonical.CanonicalResponsesRequest{Settings: map[string]json.RawMessage{
			"reasoning": json.RawMessage(`{"summary":"detailed","vendor_extra":{"x":1}}`),
			"include":   json.RawMessage(`["reasoning.encrypted_content"]`),
		}},
		Messages: []canonical.CanonicalMessage{{Role: canonical.RoleAssistant, Content: []canonical.CanonicalContentPart{{Type: "native", Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.item", Data: json.RawMessage(`{"type":"reasoning","id":"x","encrypted_content":"opaque"}`)}}}}},
	}
	endpoint := config.Endpoint{Extensions: map[string]any{"omit_params": []any{"temperature", "max_output_tokens"}, "reasoning_effort_mapping": map[string]string{"high": "medium"}, "disable_encrypted_reasoning": true}}
	body, err := (&Adapter{}).nativeBody(req, "resolved", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "resolved" || got["store"] != false || got["stream"] != true || got["top_p"] != top {
		t.Fatalf("wrong body: %s", body)
	}
	if _, ok := got["temperature"]; ok {
		t.Fatalf("temperature was not omitted: %s", body)
	}
	if _, ok := got["max_output_tokens"]; ok {
		t.Fatalf("max_output_tokens was not omitted: %s", body)
	}
	if !reflect.DeepEqual(got["include"], []any{"reasoning.encrypted_content"}) {
		t.Fatalf("explicit include changed: %s", body)
	}
	if !reflect.DeepEqual(got["reasoning"], map[string]any{"effort": "medium", "summary": "detailed", "vendor_extra": map[string]any{"x": float64(1)}}) {
		t.Fatalf("reasoning lost: %s", body)
	}
	delete(req.Responses.Settings, "include")
	body, err = (&Adapter{}).nativeBody(req, "resolved", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	_ = json.Unmarshal(body, &got)
	if _, ok := got["include"]; ok {
		t.Fatalf("disabled include still sent: %s", body)
	}
	delete(endpoint.Extensions, "disable_encrypted_reasoning")
	body, err = (&Adapter{}).nativeBody(req, "resolved", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	_ = json.Unmarshal(body, &got)
	if !reflect.DeepEqual(got["include"], []any{"reasoning.encrypted_content"}) {
		t.Fatalf("automatic include missing: %s", body)
	}
}

func TestNativeBodyRejectsMissingItem(t *testing.T) {
	req := &canonical.CanonicalChatRequest{Responses: &canonical.CanonicalResponsesRequest{}, Messages: []canonical.CanonicalMessage{{Role: canonical.RoleUser}}}
	if _, err := (&Adapter{}).nativeBody(req, "model", config.Endpoint{}); err == nil {
		t.Fatal("expected error")
	}
}

type responseTestTransport func(*http.Request) (*http.Response, error)

func (transport responseTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type responseTestFill byte

func (fill responseTestFill) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = byte(fill)
	}
	return len(buffer), nil
}

type responseTestReadError struct{}

func (responseTestReadError) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func responseTestAdapter(status int, body io.Reader, headers http.Header) *Adapter {
	return NewAdapterWithPolicy(0, responseTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(body)}, nil
	}), httpretry.Policy{})
}

func TestNonStreamResponseBodyLimits(t *testing.T) {
	const response = `{"id":"r","object":"response","model":"m","status":"completed","output":[]}`
	for _, native := range []bool{false, true} {
		for _, size := range []int64{int64(len(response)), maxResponseBodyBytes, maxResponseBodyBytes + 1} {
			t.Run(fmt.Sprintf("native=%t/size=%d", native, size), func(t *testing.T) {
				request := &canonical.CanonicalChatRequest{}
				if native {
					request.Responses = &canonical.CanonicalResponsesRequest{}
				}
				body := io.MultiReader(strings.NewReader(response), io.LimitReader(responseTestFill(' '), size-int64(len(response))))
				result, err := responseTestAdapter(http.StatusOK, body, nil).ProcessChat(context.Background(), request, "m", config.Endpoint{URL: "http://provider.test"})
				if size <= maxResponseBodyBytes {
					if err != nil || result == nil {
						t.Fatalf("response = %v, error = %v", result, err)
					}
				} else {
					var gatewayError *canonical.GatewayError
					if result != nil || !errors.As(err, &gatewayError) || gatewayError.StatusCode != http.StatusBadGateway || gatewayError.ErrorKind != canonical.ErrorKindProviderError {
						t.Fatalf("response = %v, error = %v", result, err)
					}
				}
			})
		}
	}
}

func TestNonStreamErrorResponseBounds(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, test := range []struct {
			name    string
			body    io.Reader
			message string
		}{
			{"valid", strings.NewReader(`{"error":{"message":"rate limited"}}`), "rate limited"},
			{"malformed", strings.NewReader(`{`), "{"},
			{"large", io.LimitReader(responseTestFill('x'), maxErrorBodyBytes+1), "provider error response was incomplete or too large"},
			{"unreadable", io.MultiReader(strings.NewReader("partial"), responseTestReadError{}), "provider error response was incomplete or too large"},
		} {
			t.Run(fmt.Sprintf("native=%t/%s", native, test.name), func(t *testing.T) {
				request := &canonical.CanonicalChatRequest{}
				if native {
					request.Responses = &canonical.CanonicalResponsesRequest{}
				}
				adapter := responseTestAdapter(http.StatusTooManyRequests, test.body, http.Header{"Retry-After": []string{"17"}})
				_, err := adapter.ProcessChat(context.Background(), request, "m", config.Endpoint{URL: "http://provider.test"})
				var gatewayError *canonical.GatewayError
				if !errors.As(err, &gatewayError) || gatewayError.StatusCode != http.StatusTooManyRequests || gatewayError.RetryAfter != 17 || gatewayError.Message != test.message || len(gatewayError.Raw) > maxErrorBodyBytes {
					t.Fatalf("error = %+v", err)
				}
			})
		}
	}
}

type responseTestBlockingBody struct {
	closed chan struct{}
	once   sync.Once
}

func (body *responseTestBlockingBody) Read([]byte) (int, error) {
	<-body.closed
	return 0, io.ErrClosedPipe
}

func (body *responseTestBlockingBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

func TestNonStreamResponseReadFailureAndCancellation(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("native=%t/status=%d", native, status), func(t *testing.T) {
				request := &canonical.CanonicalChatRequest{}
				if native {
					request.Responses = &canonical.CanonicalResponsesRequest{}
				}
				body := &responseTestBlockingBody{closed: make(chan struct{})}
				adapter := NewAdapterWithPolicy(time.Second, responseTestTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				}), httpretry.Policy{})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				time.AfterFunc(10*time.Millisecond, cancel)
				started := time.Now()
				result, err := adapter.ProcessChat(ctx, request, "m", config.Endpoint{URL: "http://provider.test"})
				var gatewayError *canonical.GatewayError
				wantStatus := status
				if status == http.StatusOK {
					wantStatus = http.StatusBadGateway
				}
				if result != nil || !errors.As(err, &gatewayError) || gatewayError.StatusCode != wantStatus || time.Since(started) > 500*time.Millisecond {
					t.Fatalf("response = %v, error = %v", result, err)
				}
				select {
				case <-body.closed:
				default:
					t.Fatal("provider body was not closed")
				}
			})
		}
	}
}

func TestNativeIncompleteErrorField(t *testing.T) {
	for _, test := range []struct {
		name, field, want string
	}{
		{"missing", "", "done"},
		{"null", `,"error":null`, "done"},
		{"error", `,"error":{"code":"server_error","message":"failed"}`, "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := `data: {"type":"response.incomplete","response":{"id":"r","object":"response","status":"incomplete","output":[]` + test.field + `}}` + "\n\n"
			adapter := responseTestAdapter(http.StatusOK, strings.NewReader(payload), http.Header{"Content-Type": []string{"text/event-stream"}})
			request := &canonical.CanonicalChatRequest{Responses: &canonical.CanonicalResponsesRequest{}, Stream: true}
			events, err := adapter.ProcessChatStream(context.Background(), request, "m", config.Endpoint{URL: "http://provider.test"})
			if err != nil {
				t.Fatal(err)
			}
			var last string
			for event := range events {
				last = event.Type
			}
			if last != test.want {
				t.Fatalf("terminal = %q, want %q", last, test.want)
			}
		})
	}
}
