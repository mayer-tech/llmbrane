package canonical

import (
	"context"
	"net/http"
	"strings"
)

type contextKey int

const (
	pathKey contextKey = iota
	conversationIDKey
	responseHeadersObserverKey
)

// WithPath returns a new context with the request path.
func WithPath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, pathKey, path)
}

// PathFromContext extracts the request path from the context.
func PathFromContext(ctx context.Context) (string, bool) {
	path, ok := ctx.Value(pathKey).(string)
	return path, ok
}

// WithConversationID associates a stable client-side conversation identifier
// with a request for local token estimation and debug log grouping.
func WithConversationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, conversationIDKey, id)
}

// ConversationIDFromHeaders prefers an explicit conversation ID over an
// interaction ID, which some clients change between conversation turns.
func ConversationIDFromHeaders(headers http.Header) string {
	for _, name := range []string{"X-Conversation-Id", "X-Interaction-Id"} {
		if id := strings.TrimSpace(headers.Get(name)); id != "" {
			return id
		}
	}
	return ""
}

// ConversationIDFromContext extracts the local conversation identifier.
func ConversationIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(conversationIDKey).(string)
	return id, ok && id != ""
}

// ResponseHeadersObserver receives headers from the final provider response.
// It allows gateway features such as local rate limiting to learn provider
// limits without coupling outbound adapters to gateway implementation details.
type ResponseHeadersObserver func(http.Header)

// WithResponseHeadersObserver registers an observer for provider response headers.
func WithResponseHeadersObserver(ctx context.Context, observer ResponseHeadersObserver) context.Context {
	return context.WithValue(ctx, responseHeadersObserverKey, observer)
}

// ObserveResponseHeaders invokes the observer registered on ctx, if any.
func ObserveResponseHeaders(ctx context.Context, headers http.Header) {
	if observer, ok := ctx.Value(responseHeadersObserverKey).(ResponseHeadersObserver); ok && observer != nil {
		observer(headers)
	}
}
