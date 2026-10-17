package core

import (
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestDetermineRequiredCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		req      *canonical.CanonicalChatRequest
		expected []string
	}{
		{
			name: "Basic Request",
			req: &canonical.CanonicalChatRequest{
				Messages: []canonical.CanonicalMessage{
					{Role: canonical.RoleUser, Content: []canonical.CanonicalContentPart{{Type: "text", Text: "Hello"}}},
				},
			},
			expected: nil,
		},
		{
			name: "Tools and Stream",
			req: &canonical.CanonicalChatRequest{
				Stream: true,
				Tools:  []canonical.CanonicalTool{{Type: "function", Name: "my_tool"}},
			},
			expected: []string{"tools", "streaming"},
		},
		{
			name: "Vision and Audio",
			req: &canonical.CanonicalChatRequest{
				Messages: []canonical.CanonicalMessage{
					{
						Role: canonical.RoleUser,
						Content: []canonical.CanonicalContentPart{
							{Type: "image", Image: &canonical.CanonicalImage{URL: "https://example.com/image.png"}},
							{Type: "audio", Audio: &canonical.CanonicalAudio{Data: []byte("audio")}},
						},
					},
				},
			},
			expected: []string{"vision", "audio_input"},
		},
		{
			name: "JSON Schema Response Format",
			req: &canonical.CanonicalChatRequest{
				ResponseFormat: &canonical.CanonicalResponseFormat{Type: "json_schema"},
			},
			expected: []string{"json_schema"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := determineRequiredCapabilities(tt.req)
			if len(caps) != len(tt.expected) {
				t.Errorf("Expected %d caps, got %d", len(tt.expected), len(caps))
			}
			expectedMap := make(map[string]bool)
			for _, e := range tt.expected {
				expectedMap[e] = true
			}
			for _, c := range caps {
				if !expectedMap[c] {
					t.Errorf("Unexpected cap: %s", c)
				}
			}
		})
	}
}

func TestCheckPolicies(t *testing.T) {
	cfg := &config.Config{
		Policies: map[string]any{
			"unknown_fields":       "reject",
			"unsupported_features": "reject",
		},
	}

	g := NewGateway(cfg)

	t.Run("Reject Unknown Fields", func(t *testing.T) {
		req := &canonical.CanonicalChatRequest{
			Extensions: map[string]any{"unknown": "value"},
		}
		route := &ResolvedRoute{}
		err := g.checkPolicies(req, route)
		if err == nil {
			t.Fatal("Expected error due to unknown fields")
		}
		gerr, ok := err.(*canonical.GatewayError)
		if !ok || gerr.ErrorKind != canonical.ErrorKindPolicyViolation {
			t.Errorf("Expected PolicyViolation error, got: %v", err)
		}
	})

	t.Run("Reject Unsupported Features", func(t *testing.T) {
		req := &canonical.CanonicalChatRequest{
			Stream: true,
		}
		route := &ResolvedRoute{
			Capabilities: []string{"vision"}, // Streaming is missing
		}
		err := g.checkPolicies(req, route)
		if err == nil {
			t.Fatal("Expected error due to unsupported features")
		}
		gerr, ok := err.(*canonical.GatewayError)
		if !ok || gerr.ErrorKind != canonical.ErrorKindUnsupportedFeature {
			t.Errorf("Expected UnsupportedFeature error, got: %v", err)
		}
	})

	t.Run("Allow Supported Features", func(t *testing.T) {
		req := &canonical.CanonicalChatRequest{
			Stream: true,
		}
		route := &ResolvedRoute{
			Capabilities: []string{"streaming", "vision"},
		}
		err := g.checkPolicies(req, route)
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
	})
}

func TestApplyReasoningEffortDefault(t *testing.T) {
	route := &ResolvedRoute{DefaultReasoningEffort: "medium"}

	t.Run("Inject When Missing", func(t *testing.T) {
		req := &canonical.CanonicalChatRequest{}
		applyReasoningEffortDefault(req, route)
		if req.Params.ReasoningEffort == nil || *req.Params.ReasoningEffort != "medium" {
			t.Fatalf("expected injected effort 'medium', got %v", req.Params.ReasoningEffort)
		}
	})

	t.Run("Explicit Client Value Wins", func(t *testing.T) {
		none := "none"
		req := &canonical.CanonicalChatRequest{}
		req.Params.ReasoningEffort = &none
		applyReasoningEffortDefault(req, route)
		if req.Params.ReasoningEffort == nil || *req.Params.ReasoningEffort != "none" {
			t.Fatalf("expected client effort 'none' to be kept, got %v", req.Params.ReasoningEffort)
		}
	})

	t.Run("No Default Configured", func(t *testing.T) {
		req := &canonical.CanonicalChatRequest{}
		applyReasoningEffortDefault(req, &ResolvedRoute{})
		if req.Params.ReasoningEffort != nil {
			t.Fatalf("expected nil effort, got %v", *req.Params.ReasoningEffort)
		}
	})
}

func TestResolveRouteDefaultReasoningEffort(t *testing.T) {
	cfg := &config.Config{
		Aliases: map[string]string{"opus": "anthropic/claude-opus-4-7"},
		Providers: map[string]config.Provider{
			"anthropic": {
				Endpoints: []config.Endpoint{
					{
						API: "anthropic-messages",
						URL: "https://api.anthropic.com/v1/messages",
						Models: map[string]config.ModelConf{
							"claude-opus-4-7": {Target: "claude-opus-4-7", DefaultReasoningEffort: "medium"},
							"claude-haiku-4-5": {Target: "claude-haiku-4-5"},
						},
					},
				},
			},
		},
	}

	g := NewGateway(cfg)

	route, err := g.ResolveRoute("opus")
	if err != nil {
		t.Fatalf("ResolveRoute failed: %v", err)
	}
	if route.DefaultReasoningEffort != "medium" {
		t.Errorf("expected alias to inherit default 'medium', got %q", route.DefaultReasoningEffort)
	}

	route, err = g.ResolveRoute("anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("ResolveRoute failed: %v", err)
	}
	if route.DefaultReasoningEffort != "" {
		t.Errorf("expected empty default, got %q", route.DefaultReasoningEffort)
	}
}
