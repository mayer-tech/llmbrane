package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestOutputLimitTranslation(t *testing.T) {
	zero, small, valid := 0, 8192, 8193
	max, high := "max", "high"
	for _, tt := range []struct {
		name       string
		limit      *int
		effort     *string
		extensions map[string]any
		want       int
		fail       bool
	}{
		{"fallback", nil, nil, nil, 16384, false},
		{"zero", &zero, nil, nil, 0, false},
		{"manual max", nil, &max, map[string]any{"thinking_mode": "budget"}, 36864, false},
		{"invalid explicit", &small, &high, map[string]any{"thinking_mode": "budget"}, 0, true},
		{"valid explicit", &valid, &high, map[string]any{"thinking_mode": "budget"}, 8193, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{MaxOutputTokens: tt.limit, ReasoningEffort: tt.effort}}
			out, err := NewAdapter().translateRequest(req, "model", config.Endpoint{Extensions: tt.extensions})
			if (err != nil) != tt.fail {
				t.Fatalf("error = %v", err)
			}
			if tt.fail {
				return
			}
			wire, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(wire, &payload); err != nil {
				t.Fatal(err)
			}
			if string(payload["max_tokens"]) != fmt.Sprint(tt.want) {
				t.Fatalf("payload = %s", wire)
			}
		})
	}
}

func TestOutputLimitAndStopOverHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, zero := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/zero=%t", stream, zero), func(t *testing.T) {
				requests := make(chan map[string]json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode: %v", err)
					}
					requests <- payload
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
					} else {
						_, _ = w.Write([]byte(`{"id":"id","model":"model","content":[],"stop_reason":"max_tokens"}`))
					}
				}))
				defer server.Close()
				req := &canonical.CanonicalChatRequest{}
				want := "16384"
				if zero {
					value := 0
					req.Params.MaxOutputTokens = &value
					want = "0"
				}
				adapter := NewAdapter()
				endpoint := config.Endpoint{URL: server.URL}
				if stream {
					events, err := adapter.ProcessChatStream(context.Background(), req, "model", endpoint)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for event := range events {
						if event.Stop != nil && event.Stop.Reason == "length" {
							found = true
						}
						if event.Error != nil {
							t.Fatal(event.Error)
						}
					}
					if !found {
						t.Fatal("missing length stop")
					}
				} else {
					response, err := adapter.ProcessChat(context.Background(), req, "model", endpoint)
					if err != nil {
						t.Fatal(err)
					}
					if response.Stop == nil || response.Stop.Reason != "length" || response.Choices[0].FinishReason != "length" {
						t.Fatalf("response = %+v", response)
					}
				}
				payload := <-requests
				if string(payload["max_tokens"]) != want {
					t.Fatalf("payload = %#v", payload)
				}
			})
		}
	}
}
