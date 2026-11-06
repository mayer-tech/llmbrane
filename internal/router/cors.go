package router

import (
	"net/http"
	"strings"
)

// CORS allows browser clients to use the gateway from the configured origins.
// A nil list allows all origins; an empty list disables cross-origin access.
func CORS(allowedOrigins []string) Middleware {
	allowAll := allowedOrigins == nil
	origins := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if origin == "*" {
			allowAll = true
		}
		origins[origin] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Responses differ by origin and, for preflight, requested headers/method.
			w.Header().Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			method := r.Header.Get("Access-Control-Request-Method")
			preflight := r.Method == http.MethodOptions && origin != "" && method != ""
			if preflight {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")
			}

			_, allowed := origins[origin]
			if origin != "" && (allowAll || allowed) {
				if allowAll {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
				w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
			} else if preflight {
				w.WriteHeader(http.StatusForbidden)
				return
			}

			if preflight {
				if method != http.MethodGet && method != http.MethodHead && method != http.MethodPost {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST")
				if headers := strings.Join(r.Header.Values("Access-Control-Request-Headers"), ", "); headers != "" {
					w.Header().Set("Access-Control-Allow-Headers", headers)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
