package core

import (
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestResponsesRoutePolicyAndMarker(t *testing.T) {
	gateway := NewGateway(&config.Config{})
	req := &canonical.CanonicalChatRequest{Responses: &canonical.CanonicalResponsesRequest{}}
	for _, tc := range []struct {
		shape           string
		allowed, native bool
	}{
		{"openai-responses", true, true},
		{"anthropic-messages", true, false},
		{"openai-chat", false, false},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			route := &ResolvedRoute{APIShape: tc.shape}
			if got := gateway.checkPolicies(req, route) == nil; got != tc.allowed {
				t.Fatalf("allowed = %v", got)
			}
			out := requestForRoute(req, route)
			if (out.Responses != nil) != tc.native {
				t.Fatalf("Responses marker: %#v", out.Responses)
			}
			if req.Responses == nil {
				t.Fatal("original request mutated")
			}
		})
	}
}
