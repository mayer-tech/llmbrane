package core

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func limitPtr(value int) *int        { return &value }
func effortPtr(value string) *string { return &value }

func TestOutputTokenResolution(t *testing.T) {
	tests := []struct {
		name               string
		api                string
		client, configured *int
		effort             *string
		extensions         map[string]any
		want               *int
		fail               bool
	}{
		{name: "anthropic fallback", api: "anthropic-messages", want: limitPtr(16384)},
		{name: "anthropic configured", api: "anthropic-messages", configured: limitPtr(6000), want: limitPtr(6000)},
		{name: "client wins", api: "anthropic-messages", client: limitPtr(500), configured: limitPtr(6000), want: limitPtr(500)},
		{name: "explicit zero", api: "anthropic-messages", client: limitPtr(0), configured: limitPtr(6000), want: limitPtr(0)},
		{name: "negative", api: "anthropic-messages", client: limitPtr(-1), fail: true},
		{name: "other unset", api: "openai-chat-completions"},
		{name: "other configured", api: "bedrock-converse", configured: limitPtr(8000), want: limitPtr(8000)},
		{name: "other explicit zero", api: "openai-responses", client: limitPtr(0), configured: limitPtr(8000), want: limitPtr(0)},
		{name: "budget fallback raised", api: "anthropic-messages", effort: effortPtr("max"), extensions: map[string]any{"thinking_mode": "budget"}, want: limitPtr(36864)},
		{name: "budget configured raised", api: "anthropic-messages", configured: limitPtr(5000), effort: effortPtr("high"), extensions: map[string]any{"thinking_mode": "budget"}, want: limitPtr(12288)},
		{name: "budget configured higher", api: "anthropic-messages", configured: limitPtr(20000), effort: effortPtr("high"), extensions: map[string]any{"thinking_mode": "budget"}, want: limitPtr(20000)},
		{name: "mapped budget", api: "anthropic-messages", configured: limitPtr(5000), effort: effortPtr("max"), extensions: map[string]any{"thinking_mode": "budget", "reasoning_effort_mapping": map[string]string{"max": "low"}}, want: limitPtr(6144)},
		{name: "mapped none enables budget", api: "anthropic-messages", configured: limitPtr(5000), effort: effortPtr("none"), extensions: map[string]any{"thinking_mode": "budget", "reasoning_effort_mapping": map[string]string{"none": "low"}}, want: limitPtr(6144)},
		{name: "client not raised", api: "anthropic-messages", client: limitPtr(8193), effort: effortPtr("high"), extensions: map[string]any{"thinking_mode": "budget"}, want: limitPtr(8193)},
		{name: "client too small", api: "anthropic-messages", client: limitPtr(8192), effort: effortPtr("high"), extensions: map[string]any{"thinking_mode": "budget"}, fail: true},
		{name: "zero with thinking", api: "anthropic-messages", client: limitPtr(0), effort: effortPtr("high"), extensions: map[string]any{"thinking_mode": "budget"}, fail: true},
		{name: "zero with disabled thinking", api: "anthropic-messages", client: limitPtr(0), effort: effortPtr("none"), extensions: map[string]any{"thinking_mode": "budget"}, want: limitPtr(0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{MaxOutputTokens: tt.client, ReasoningEffort: tt.effort}}
			route := &ResolvedRoute{APIShape: tt.api, DefaultMaxOutputTokens: tt.configured, Endpoint: config.Endpoint{Extensions: tt.extensions}}
			err := applyOutputTokenDefault(req, route)
			if (err != nil) != tt.fail {
				t.Fatalf("error = %v, want error %v", err, tt.fail)
			}
			if tt.fail {
				if ge, ok := err.(*canonical.GatewayError); !ok || ge.StatusCode != http.StatusBadRequest {
					t.Fatalf("expected 400, got %v", err)
				}
				return
			}
			if (req.Params.MaxOutputTokens == nil) != (tt.want == nil) {
				t.Fatalf("limit = %v, want %v", req.Params.MaxOutputTokens, tt.want)
			}
			if tt.want != nil && *req.Params.MaxOutputTokens != *tt.want {
				t.Fatalf("limit = %d, want %d", *req.Params.MaxOutputTokens, *tt.want)
			}
			if tt.client == nil && tt.configured != nil && req.Params.MaxOutputTokens == tt.configured {
				t.Fatal("request shares config pointer")
			}
		})
	}
}

func TestOutputTokenResolutionConfigError(t *testing.T) {
	req := &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{ReasoningEffort: effortPtr("high")}}
	route := &ResolvedRoute{
		APIShape: "anthropic-messages",
		Endpoint: config.Endpoint{Extensions: map[string]any{"thinking_mode": "invalid"}},
	}
	err := applyOutputTokenDefault(req, route)
	if ge, ok := err.(*canonical.GatewayError); !ok || ge.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected configuration error 500, got %v", err)
	}
}

type outputLimitAdapter struct {
	calls int
	limit int
}

func (a *outputLimitAdapter) ProcessChat(_ context.Context, req *canonical.CanonicalChatRequest, _ string, _ config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	a.calls++
	if req.Params.MaxOutputTokens != nil {
		a.limit = *req.Params.MaxOutputTokens
	}
	return &canonical.CanonicalChatResponse{}, nil
}
func (a *outputLimitAdapter) ProcessChatStream(_ context.Context, req *canonical.CanonicalChatRequest, _ string, _ config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	a.calls++
	if req.Params.MaxOutputTokens != nil {
		a.limit = *req.Params.MaxOutputTokens
	}
	ch := make(chan *canonical.CanonicalChatStreamEvent)
	close(ch)
	return ch, nil
}

func TestOutputLimitResolvedBeforeReservation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tt := range []struct {
			name              string
			limit             *int
			client            *int
			extensions        map[string]any
			effort            string
			tpm, want, status int
		}{
			{"fallback rejected", nil, nil, nil, "", 10000, 0, 429},
			{"model admitted", limitPtr(1000), nil, nil, "", 10000, 1000, 0},
			{"client admitted", limitPtr(20000), limitPtr(1000), nil, "", 10000, 1000, 0},
			{"zero admitted", nil, limitPtr(0), nil, "", 10000, 0, 0},
			{"adjusted rejected", limitPtr(1000), nil, map[string]any{"thinking_mode": "budget"}, "high", 10000, 0, 429},
			{"invalid budget rejected", nil, limitPtr(100), map[string]any{"thinking_mode": "budget"}, "high", 10000, 0, 400},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tt.name), func(t *testing.T) {
				ep := config.Endpoint{API: "anthropic-messages", URL: "https://example.com", Extensions: tt.extensions, RateLimit: &config.RateLimit{TokensPerMinute: tt.tpm}, Models: map[string]config.ModelConf{"model": {Target: "model", DefaultMaxOutputTokens: tt.limit, DefaultReasoningEffort: tt.effort}}}
				g := NewGateway(&config.Config{Providers: map[string]config.Provider{"provider": {Endpoints: []config.Endpoint{ep}}}, Aliases: map[string]config.Alias{"alias": {Target: "provider/model"}}})
				a := &outputLimitAdapter{}
				g.RegisterOutboundAdapter(ep.API, a)
				route, err := g.ResolveRoute("alias")
				if err != nil || route.DefaultMaxOutputTokens != tt.limit {
					t.Fatalf("route = %+v, err = %v", route, err)
				}
				req := &canonical.CanonicalChatRequest{Model: "alias", Stream: stream, Params: canonical.CanonicalParams{MaxOutputTokens: tt.client}}
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
				if tt.status != 0 {
					ge, ok := err.(*canonical.GatewayError)
					if !ok || ge.StatusCode != tt.status || a.calls != 0 {
						t.Fatalf("err = %v, calls = %d", err, a.calls)
					}
				} else if err != nil || a.calls != 1 || a.limit != tt.want {
					t.Fatalf("err = %v, calls = %d, limit = %d", err, a.calls, a.limit)
				}
			})
		}
	}
}

func TestResponseReachedOutputLimit(t *testing.T) {
	if responseReachedOutputLimit(nil) || responseReachedOutputLimit(&canonical.CanonicalChatResponse{}) {
		t.Fatal("unexpected limit stop")
	}
	if !responseReachedOutputLimit(&canonical.CanonicalChatResponse{Stop: &canonical.CanonicalStop{Reason: "max_tokens"}}) {
		t.Fatal("missing stop reason")
	}
	if !responseReachedOutputLimit(&canonical.CanonicalChatResponse{Choices: []canonical.CanonicalChoice{{FinishReason: "length"}}}) {
		t.Fatal("missing choice finish reason")
	}
}
