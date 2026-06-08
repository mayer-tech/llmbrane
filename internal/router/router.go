package router

import (
	"net/http"
)

// New creates a new HTTP router using the standard library ServeMux.
func New() *http.ServeMux {
	return http.NewServeMux()
}
