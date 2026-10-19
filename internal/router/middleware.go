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

// responseRecorder wraps http.ResponseWriter to capture status code and body.
type responseRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.status = code
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	rr.body.Write(b)
	return rr.ResponseWriter.Write(b)
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

		// Read and restore request body
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
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

		if len(reqBody) > 0 && json.Valid(reqBody) {
			entry.Body = json.RawMessage(reqBody)
		}

		respBody := rr.body.Bytes()
		if len(respBody) > 0 {
			if json.Valid(respBody) {
				entry.ResponseBody = json.RawMessage(respBody)
			} else {
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
