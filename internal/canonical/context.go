package canonical

import "context"

type contextKey int

const (
	pathKey contextKey = iota
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
