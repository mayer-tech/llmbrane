package core

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestOmittedResponsesLimitDoesNotUndercountReservation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, omitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/omitted=%t", stream, omitted), func(t *testing.T) {
				ep := config.Endpoint{
					API: "openai-responses", URL: "https://example.com",
					Models:    map[string]config.ModelConf{"model": {Target: "model"}},
					RateLimit: &config.RateLimit{TokensPerMinute: 1000, OutputReservationTokens: 2000},
				}
				if omitted {
					ep.Extensions = map[string]any{"omit_params": []any{"max_output_tokens"}}
				}
				g := NewGateway(&config.Config{Providers: map[string]config.Provider{"provider": {Endpoints: []config.Endpoint{ep}}}})
				adapter := &outputLimitAdapter{}
				g.RegisterOutboundAdapter(ep.API, adapter)
				zero := 0
				req := &canonical.CanonicalChatRequest{Model: "provider/model", Stream: stream, Params: canonical.CanonicalParams{MaxOutputTokens: &zero}}
				var err error
				if stream {
					var events <-chan *canonical.CanonicalChatStreamEvent
					events, err = g.ProcessChatStream(context.Background(), req)
					if err == nil {
						for range events {
						}
					}
				} else {
					_, err = g.ProcessChat(context.Background(), req)
				}
				if omitted {
					ge, ok := err.(*canonical.GatewayError)
					if !ok || ge.StatusCode != http.StatusTooManyRequests || adapter.calls != 0 {
						t.Fatalf("error = %v, calls = %d", err, adapter.calls)
					}
				} else if err != nil || adapter.calls != 1 {
					t.Fatalf("error = %v, calls = %d", err, adapter.calls)
				}
			})
		}
	}
}
