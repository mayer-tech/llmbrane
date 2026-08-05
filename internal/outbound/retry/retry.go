package retry

import (
	"context"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/logger"
)

// Policy controls retries for provider-side rate limiting.
type Policy struct {
	MaxRetries int
	BaseDelay  time.Duration
	Jitter     time.Duration
	MaxDelay   time.Duration
}

// DefaultPolicy retries an initial HTTP 429 response at most twice.
var DefaultPolicy = Policy{
	MaxRetries: 2,
	BaseDelay:  500 * time.Millisecond,
	Jitter:     250 * time.Millisecond,
	MaxDelay:   30 * time.Second,
}

// Do executes an HTTP request and transparently retries initial HTTP 429
// responses. Streaming responses are safe because retries happen before the
// successful response body is exposed to the caller.
func Do(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	return DefaultPolicy.Do(ctx, client, req)
}

// Do executes an HTTP request according to the policy.
func (p Policy) Do(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	currentReq := req

	for attempt := 0; ; attempt++ {
		resp, err := client.Do(currentReq)
		if resp != nil {
			canonical.ObserveResponseHeaders(ctx, resp.Header)
		}
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || attempt >= p.MaxRetries {
			return resp, err
		}

		nextReq, err := cloneRequest(req, ctx)
		if err != nil {
			return resp, nil
		}

		delay, ok := RetryDelay(resp.Header, time.Now())
		if !ok {
			delay = exponentialDelay(p.BaseDelay, attempt)
		}
		delay += randomJitter(p.Jitter)
		if p.MaxDelay > 0 && delay > p.MaxDelay {
			return resp, nil
		}

		// Preserve the provider's 429 response when the request deadline is too
		// close for a useful retry. This lets the inbound adapter return the
		// provider's Retry-After value instead of an opaque timeout.
		if deadline, ok := ctx.Deadline(); ok && time.Now().Add(delay).After(deadline) {
			return resp, nil
		}

		drainAndClose(resp.Body)
		logger.Warn.Printf("Provider returned HTTP 429 for %s; retrying in %s (attempt %d/%d)", req.URL.Redacted(), delay, attempt+1, p.MaxRetries)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}

		currentReq = nextReq
	}
}

// RetryDelay returns the delay requested by standard Retry-After or common
// provider rate-limit reset headers. Retry-After takes precedence.
func RetryDelay(headers http.Header, now time.Time) (time.Duration, bool) {
	if value := headers.Get("Retry-After"); value != "" {
		if delay, ok := parseRetryAfter(value, now); ok {
			return delay, true
		}
	}

	if value := headers.Get("Retry-After-Ms"); value != "" {
		if milliseconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && milliseconds >= 0 {
			return time.Duration(milliseconds * float64(time.Millisecond)), true
		}
	}

	resetHeaders := []string{
		"X-RateLimit-Reset-Tokens",
		"X-RateLimit-Reset-Requests",
		"X-RateLimit-Reset",
		"RateLimit-Reset",
		"Anthropic-RateLimit-Requests-Reset",
		"Anthropic-RateLimit-Tokens-Reset",
		"Anthropic-RateLimit-Input-Tokens-Reset",
		"Anthropic-RateLimit-Output-Tokens-Reset",
	}

	var longest time.Duration
	found := false
	for _, name := range resetHeaders {
		if value := headers.Get(name); value != "" {
			if delay, ok := parseReset(value, now); ok {
				if !found || delay > longest {
					longest = delay
				}
				found = true
			}
		}
	}
	return longest, found
}

// ApplyHeaders adds provider retry timing to a normalized rate-limit error.
func ApplyHeaders(err *canonical.GatewayError, headers http.Header) *canonical.GatewayError {
	if err == nil || err.StatusCode != http.StatusTooManyRequests {
		return err
	}
	if delay, ok := RetryDelay(headers, time.Now()); ok && delay > 0 {
		err.RetryAfter = int(math.Ceil(delay.Seconds()))
	}
	return err
}

func cloneRequest(req *http.Request, ctx context.Context) (*http.Request, error) {
	clone := req.Clone(ctx)
	if req.Body == nil {
		return clone, nil
	}
	if req.GetBody == nil {
		return nil, http.ErrBodyNotAllowed
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	clone.Body = body
	return clone, nil
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second)), true
	}
	if at, err := http.ParseTime(value); err == nil {
		return nonNegative(at.Sub(now)), true
	}
	return 0, false
}

func parseReset(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if delay, err := time.ParseDuration(value); err == nil {
		return nonNegative(delay), true
	}
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return nonNegative(at.Sub(now)), true
	}
	if numeric, err := strconv.ParseFloat(value, 64); err == nil && numeric >= 0 {
		// Large reset values are Unix timestamps; small values are durations in
		// seconds, as used by several OpenAI-compatible providers.
		if numeric > float64(now.Unix()-60) {
			at := time.Unix(0, int64(numeric*float64(time.Second)))
			return nonNegative(at.Sub(now)), true
		}
		return time.Duration(numeric * float64(time.Second)), true
	}
	return 0, false
}

func exponentialDelay(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	return base * time.Duration(1<<min(attempt, 10))
}

func randomJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(maximum) + 1))
}

func nonNegative(delay time.Duration) time.Duration {
	if delay < 0 {
		return 0
	}
	return delay
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, body, 64*1024)
	_ = body.Close()
}
