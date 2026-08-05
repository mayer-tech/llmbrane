package retry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestDoRetries429AndReplaysBody(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		if string(body) != `{"message":"hello"}` {
			t.Errorf("unexpected request body: %s", body)
		}

		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	policy := Policy{MaxRetries: 2}
	resp, err := policy.Do(context.Background(), server.Client(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestDoStopsAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("body"))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	policy := Policy{MaxRetries: 2}
	resp, err := policy.Do(context.Background(), server.Client(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", resp.StatusCode)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRetryDelayPrefersRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	headers := http.Header{
		"Retry-After":              []string{"2"},
		"X-Ratelimit-Reset-Tokens": []string{"10s"},
	}

	delay, ok := RetryDelay(headers, now)
	if !ok {
		t.Fatal("expected retry delay")
	}
	if delay != 2*time.Second {
		t.Fatalf("expected 2s delay, got %s", delay)
	}
}

func TestRetryDelayUsesLongestReset(t *testing.T) {
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	headers := http.Header{
		"X-Ratelimit-Reset-Tokens":   []string{"3s"},
		"X-Ratelimit-Reset-Requests": []string{"1s"},
	}

	delay, ok := RetryDelay(headers, now)
	if !ok {
		t.Fatal("expected retry delay")
	}
	if delay != 3*time.Second {
		t.Fatalf("expected 3s delay, got %s", delay)
	}
}

func TestDoReturns429WhenDelayExceedsDeadline(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("body"))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := DefaultPolicy.Do(ctx, server.Client(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", resp.StatusCode)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 attempt, got %d", attempts.Load())
	}
}

func TestDoReturns429WhenDelayExceedsPolicyMaximum(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("body"))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := DefaultPolicy.Do(context.Background(), server.Client(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", resp.StatusCode)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 attempt, got %d", attempts.Load())
	}
}

func TestApplyHeadersSetsRoundedRetryAfter(t *testing.T) {
	gatewayErr := canonical.NewGatewayError("provider", "model", "rate limited", http.StatusTooManyRequests, nil)
	headers := http.Header{"Retry-After-Ms": []string{"1500"}}

	ApplyHeaders(gatewayErr, headers)

	if gatewayErr.RetryAfter != 2 {
		t.Fatalf("expected RetryAfter 2, got %d", gatewayErr.RetryAfter)
	}
}

func TestDoObservesProviderHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit-Tokens", "200000")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	observed := make(chan http.Header, 1)
	ctx := canonical.WithResponseHeadersObserver(context.Background(), func(headers http.Header) {
		observed <- headers.Clone()
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("body"))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	resp, err := Do(ctx, server.Client(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	select {
	case headers := <-observed:
		if headers.Get("X-RateLimit-Limit-Tokens") != "200000" {
			t.Fatalf("unexpected observed headers: %v", headers)
		}
	default:
		t.Fatal("expected provider response headers to be observed")
	}
}
