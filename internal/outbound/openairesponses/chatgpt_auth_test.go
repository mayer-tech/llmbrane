package openairesponses

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

type testTokenSource struct {
	token string
	err   error
}

func (s testTokenSource) Token(context.Context) (string, error) { return s.token, s.err }

func TestChatGPTAuthorizationPaths(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, fail := range []bool{false, true} {
				name := "canonical"
				if native {
					name = "native"
				}
				if stream {
					name += "/stream"
				} else {
					name += "/nonstream"
				}
				if fail {
					name += "/token-failure"
				}
				t.Run(name, func(t *testing.T) {
					calls := 0
					transport := responseTestTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						if r.Header.Get("Authorization") != "Bearer oauth-token" || r.Header.Get("X-Custom") != "value" {
							t.Error("incorrect request headers")
						}
						return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"test"}}`))}, nil
					})
					a := newAdapterWithTokenManagerFactory(0, transport, retry.Policy{}, func(path string) (tokenSource, error) {
						if path != "credentials.json" {
							t.Error("wrong credentials path")
						}
						if fail {
							return testTokenSource{err: errors.New("secret")}, nil
						}
						return testTokenSource{token: "oauth-token"}, nil
					})
					req := &canonical.CanonicalChatRequest{Stream: stream}
					if native {
						req.Responses = &canonical.CanonicalResponsesRequest{}
					}
					ep := config.Endpoint{URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "credentials.json"}, Headers: map[string]string{"X-Custom": "value"}}
					var err error
					if stream {
						_, err = a.ProcessChatStream(context.Background(), req, "model", ep)
					} else {
						_, err = a.ProcessChat(context.Background(), req, "model", ep)
					}
					if err == nil || strings.Contains(err.Error(), "secret") {
						t.Fatalf("unexpected error: %v", err)
					}
					if (calls == 0) != (fail || !stream) {
						t.Fatalf("unexpected request count: %d", calls)
					}
				})
			}
		}
	}
}

func TestChatGPTAuthorizationRestrictions(t *testing.T) {
	a := newAdapterWithTokenManagerFactory(0, nil, retry.Policy{}, func(string) (tokenSource, error) { return testTokenSource{token: "oauth-token"}, nil })
	for _, raw := range []string{"https://example.com/v1/responses", "http://api.openai.com/v1/responses", "https://api.openai.com/v1/responses?x=y"} {
		r, err := http.NewRequest(http.MethodPost, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.authorize(context.Background(), r, config.Endpoint{Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "creds"}}, "model"); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
