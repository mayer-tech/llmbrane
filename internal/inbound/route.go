package inbound

import "net/http"

// Route describes an inbound endpoint relative to its protocol prefix.
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}
