package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestTranslateRequestOmitSamplingParams(t *testing.T) {
	tests := []struct {
		name        string
		extensions  map[string]any
		temperature bool
		topP        bool
	}{
		{"default preserves both", nil, true, true},
		{"empty list preserves both", map[string]any{"omit_params": []string{}}, true, true},
		{"omit temperature", map[string]any{"omit_params": []string{"temperature"}}, false, true},
		{"omit top_p", map[string]any{"omit_params": []any{"top_p"}}, true, false},
		{"omit both", map[string]any{"omit_params": []any{"temperature", "top_p"}}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			temperature, topP := 0.0, 1.0
			effort := "high"
			maxTokens := 8192
			req := &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{
				Temperature: &temperature, TopP: &topP, ReasoningEffort: &effort, MaxOutputTokens: &maxTokens,
			}}
			out, err := NewAdapter().translateRequest(req, "claude-opus-5-5", config.Endpoint{Extensions: tt.extensions})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			_, hasTemperature := wire["temperature"]
			_, hasTopP := wire["top_p"]
			if hasTemperature != tt.temperature || hasTopP != tt.topP {
				t.Fatalf("unexpected sampling fields: %s", data)
			}
			if out.Thinking == nil || out.Thinking.Type != "adaptive" || out.OutputConfig == nil || out.OutputConfig.Effort != "high" || out.MaxTokens != maxTokens {
				t.Fatalf("unexpected request: %s", data)
			}
			if req.Params.Temperature != &temperature || req.Params.TopP != &topP || temperature != 0 || topP != 1 {
				t.Fatal("canonical request was modified")
			}
		})
	}
}

func TestProcessChatOmitsSamplingParams(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			received := make(chan map[string]json.RawMessage, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var wire map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
					t.Errorf("decode request: %v", err)
				}
				received <- wire
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
				} else {
					_, _ = w.Write([]byte(`{"id":"id","model":"model","content":[]}`))
				}
			}))
			defer server.Close()

			temperature, topP, effort := 0.1, 1.0, "high"
			req := &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{
				Temperature: &temperature, TopP: &topP, ReasoningEffort: &effort,
			}}
			endpoint := config.Endpoint{URL: server.URL, Extensions: map[string]any{"omit_params": []any{"temperature", "top_p"}}}
			adapter := NewAdapter()
			if stream {
				events, err := adapter.ProcessChatStream(context.Background(), req, "claude-opus-5-5", endpoint)
				if err != nil {
					t.Fatal(err)
				}
				for event := range events {
					if event.Type == "error" {
						t.Fatalf("stream error: %v", event.Error)
					}
				}
			} else if _, err := adapter.ProcessChat(context.Background(), req, "claude-opus-5-5", endpoint); err != nil {
				t.Fatal(err)
			}
			wire := <-received
			if _, ok := wire["temperature"]; ok {
				t.Fatalf("temperature was sent: %s", wire["temperature"])
			}
			if _, ok := wire["top_p"]; ok {
				t.Fatalf("top_p was sent: %s", wire["top_p"])
			}
			var thinking ThinkingConfig
			var output OutputConfig
			if err := json.Unmarshal(wire["thinking"], &thinking); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(wire["output_config"], &output); err != nil {
				t.Fatal(err)
			}
			if thinking.Type != "adaptive" || output.Effort != "high" || string(wire["max_tokens"]) != "16384" {
				t.Fatalf("unexpected request: %#v", wire)
			}
			if (string(wire["stream"]) == "true") != stream {
				t.Fatalf("unexpected stream field: %s", wire["stream"])
			}
		})
	}
}
