package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestTranslateRequestReasoning(t *testing.T) {
	tests := []struct {
		name       string
		effort     string
		extensions map[string]any
		thinking   *ThinkingConfig
		output     *OutputConfig
		fail       bool
	}{
		{"unset", "", nil, nil, nil, false},
		{"none", "none", nil, &ThinkingConfig{Type: "disabled"}, nil, false},
		{"minimal", "minimal", nil, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "low"}, false},
		{"low", "low", nil, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "low"}, false},
		{"medium", "medium", nil, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "medium"}, false},
		{"high", "high", nil, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "high"}, false},
		{"xhigh", "xhigh", nil, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "xhigh"}, false},
		{"max", "max", nil, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "max"}, false},
		{"legacy", "high", map[string]any{"thinking_mode": "budget"}, &ThinkingConfig{Type: "enabled", BudgetTokens: 8192}, nil, false},
		{"legacy none", "none", map[string]any{"thinking_mode": "budget"}, &ThinkingConfig{Type: "disabled"}, nil, false},
		{"legacy minimal", "minimal", map[string]any{"thinking_mode": "budget"}, &ThinkingConfig{Type: "enabled", BudgetTokens: 1024}, nil, false},
		{"legacy max", "max", map[string]any{"thinking_mode": "budget"}, &ThinkingConfig{Type: "enabled", BudgetTokens: 32768}, nil, false},
		{"mapping", "xhigh", map[string]any{"reasoning_effort_mapping": map[string]any{"xhigh": "high"}}, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "high"}, false},
		{"mapping minimal", "minimal", map[string]any{"reasoning_effort_mapping": map[string]any{"minimal": "high"}}, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "high"}, false},
		{"mapping none", "none", map[string]any{"reasoning_effort_mapping": map[string]string{"none": "low"}}, &ThinkingConfig{Type: "adaptive"}, &OutputConfig{Effort: "low"}, false},
		{"invalid effort", "bad", nil, nil, nil, true},
		{"invalid mode", "high", map[string]any{"thinking_mode": "bad"}, nil, nil, true},
		{"invalid mapped value", "high", map[string]any{"reasoning_effort_mapping": map[string]any{"high": "bad"}}, nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &canonical.CanonicalChatRequest{}
			if tt.effort != "" {
				req.Params.ReasoningEffort = &tt.effort
			}
			got, err := NewAdapter().translateRequest(req, "model", config.Endpoint{Extensions: tt.extensions})
			if (err != nil) != tt.fail {
				t.Fatalf("error = %v, want error %v", err, tt.fail)
			}
			if !tt.fail && (!reflect.DeepEqual(got.Thinking, tt.thinking) || !reflect.DeepEqual(got.OutputConfig, tt.output)) {
				t.Fatalf("thinking = %#v, output = %#v; want %#v, %#v", got.Thinking, got.OutputConfig, tt.thinking, tt.output)
			}
			if !tt.fail {
				data, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]json.RawMessage
				if err := json.Unmarshal(data, &wire); err != nil {
					t.Fatal(err)
				}
				_, hasThinking := wire["thinking"]
				_, hasOutput := wire["output_config"]
				if hasThinking != (tt.thinking != nil) || hasOutput != (tt.output != nil) {
					t.Fatalf("unexpected wire fields: %s", data)
				}
				if tt.thinking != nil {
					var thinking map[string]json.RawMessage
					if err := json.Unmarshal(wire["thinking"], &thinking); err != nil {
						t.Fatal(err)
					}
					_, hasBudget := thinking["budget_tokens"]
					if hasBudget != (tt.thinking.Type == "enabled") {
						t.Fatalf("unexpected thinking fields: %s", wire["thinking"])
					}
				}
			}
		})
	}
}

func TestProcessChatReasoningPayload(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			var received MessagesRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
				} else {
					_, _ = w.Write([]byte(`{"id":"id","model":"model","content":[]}`))
				}
			}))
			defer server.Close()
			effort := "medium"
			req := &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{ReasoningEffort: &effort}}
			adapter := NewAdapter()
			endpoint := config.Endpoint{URL: server.URL}
			if stream {
				events, err := adapter.ProcessChatStream(context.Background(), req, "model", endpoint)
				if err != nil {
					t.Fatal(err)
				}
				for event := range events {
					if event.Type == "error" {
						t.Fatalf("stream error: %v", event.Error)
					}
				}
			} else if _, err := adapter.ProcessChat(context.Background(), req, "model", endpoint); err != nil {
				t.Fatal(err)
			}
			if received.Thinking == nil || received.Thinking.Type != "adaptive" || received.OutputConfig == nil || received.OutputConfig.Effort != "medium" || received.Stream != stream {
				t.Fatalf("unexpected request: %+v", received)
			}
		})
	}
}

func TestProviderRejectsUnsupportedThinking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"thinking disabled is unsupported"}}`))
	}))
	defer server.Close()
	effort := "none"
	req := &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{ReasoningEffort: &effort}}
	endpoint := config.Endpoint{URL: server.URL}
	adapter := NewAdapter()
	for _, stream := range []bool{false, true} {
		var err error
		if stream {
			_, err = adapter.ProcessChatStream(context.Background(), req, "model", endpoint)
		} else {
			_, err = adapter.ProcessChat(context.Background(), req, "model", endpoint)
		}
		gatewayErr, ok := err.(*canonical.GatewayError)
		if !ok || gatewayErr.StatusCode != http.StatusBadRequest || gatewayErr.Message != "thinking disabled is unsupported" {
			t.Fatalf("stream = %v, error = %v", stream, err)
		}
	}
}
