package router

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/debuglog"
)

// Middleware is a function that wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies multiple middlewares to a handler.
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// PanicRecovery middleware catches panics and returns a 500 status code.
func PanicRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic recovered: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Timeout middleware cancels the request context after a specified duration.
func Timeout(duration time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), duration)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestPath middleware injects the request's URL path into the context.
func RequestPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := canonical.WithPath(r.Context(), r.URL.Path)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Bound debug collection independently of the limits enforced by inbound handlers.
const (
	maxDebugRequestBytes  = 40 << 20
	maxDebugResponseBytes = 4 << 20
)

// replayBody retains the original body's Close behavior while replaying a prefix.
type replayBody struct {
	io.Reader
	io.Closer
}

// readError replays a consumed read error once, then allows reading to continue.
type readError struct {
	err error
}

func (r *readError) Read([]byte) (int, error) {
	if r.err == nil {
		return 0, io.EOF
	}
	err := r.err
	r.err = nil
	return 0, err
}

// responseRecorder wraps http.ResponseWriter to capture status code and a bounded body.
type responseRecorder struct {
	http.ResponseWriter
	status    int
	body      bytes.Buffer
	truncated bool
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.status = code
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	n, err := rr.ResponseWriter.Write(b)
	remaining := maxDebugResponseBytes - rr.body.Len()
	captured := min(n, remaining)
	_, _ = rr.body.Write(b[:captured])
	if n > captured {
		rr.truncated = true
	}
	return n, err
}

// Flush implements http.Flusher for SSE streaming support.
func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// DebugLog middleware logs inbound requests and responses to debug files.
func DebugLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !debuglog.Enabled() {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		conversationID := canonical.ConversationIDFromHeaders(r.Header)
		ctx := r.Context()
		if conversationID == "" {
			conversationID, _ = canonical.ConversationIDFromContext(ctx)
		} else {
			ctx = canonical.WithConversationID(ctx, conversationID)
		}
		r = r.WithContext(debuglog.WithRequest(ctx, conversationID))

		// Read at most the limit plus one byte to distinguish an exact-limit body
		// from a longer one. Replay everything read, followed by any read error and
		// the remaining original stream, so inbound limits see the complete body.
		var reqBody []byte
		var reqComplete bool
		if r.Body != nil {
			original := r.Body
			var err error
			reqBody, err = io.ReadAll(io.LimitReader(original, maxDebugRequestBytes+1))
			reqComplete = err == nil && len(reqBody) <= maxDebugRequestBytes
			readers := []io.Reader{bytes.NewReader(reqBody)}
			if err != nil {
				readers = append(readers, &readError{err: err})
			}
			readers = append(readers, original)
			r.Body = replayBody{Reader: io.MultiReader(readers...), Closer: original}
		}

		rr := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rr, r)

		entry := debuglog.NewEntry(r.Context(), "inbound")
		entry.Method = r.Method
		entry.Path = r.URL.Path
		entry.Headers = debuglog.RedactHeaders(r.Header)
		entry.Status = rr.status
		entry.ResponseHeaders = debuglog.RedactHeaders(rr.Header())
		entry.DurationMs = time.Since(start).Milliseconds()

		if reqComplete && len(reqBody) > 0 && json.Valid(reqBody) {
			entry.Body = json.RawMessage(reqBody)
		}

		respBody := rr.body.Bytes()
		if len(respBody) > 0 {
			if !rr.truncated && json.Valid(respBody) {
				entry.ResponseBody = json.RawMessage(respBody)
			} else {
				// For truncated streams, only inspect complete lines. A partial
				// data line can itself look like valid JSON but is not an event.
				if rr.truncated {
					if lastNewline := bytes.LastIndexByte(respBody, '\n'); lastNewline >= 0 {
						respBody = respBody[:lastNewline+1]
					} else {
						respBody = nil
					}
				}
				// SSE stream: extract individual data lines as stream events
				for line := range strings.Lines(string(respBody)) {
					line = strings.TrimSpace(line)
					if data, ok := strings.CutPrefix(line, "data: "); ok && data != "[DONE]" {
						if json.Valid([]byte(data)) {
							entry.StreamEvents = append(entry.StreamEvents, json.RawMessage(data))
						}
					}
				}
			}
		}

		debuglog.Write(entry)
	})
}
