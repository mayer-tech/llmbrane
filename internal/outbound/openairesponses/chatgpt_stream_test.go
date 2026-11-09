package openairesponses

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

func TestChatGPTStreamTerminal(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, tc := range []struct{ name, body, want string }{
			{"completed", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n", "done"},
			{"incomplete", "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"incomplete\",\"output\":[]}}\n\n", "error"},
			{"truncated", "data: {\"type\":\"response.created\"}\n\n", "error"},
			{"failed", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"limit\"}}}\n\n", "error"},
		} {
			name := tc.name
			if native {
				name += "/native"
			} else {
				name += "/canonical"
			}
			t.Run(name, func(t *testing.T) {
				transport := responseTestTransport(func(r *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), `"stream":true`) {
						t.Error("upstream request is not streaming")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})
				a := newAdapterWithTokenManagerFactory(0, transport, retry.Policy{}, func(string) (tokenSource, error) { return testTokenSource{token: "test"}, nil })
				req := &canonical.CanonicalChatRequest{}
				if native {
					req.Responses = &canonical.CanonicalResponsesRequest{}
				}
				ep := config.Endpoint{URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "creds"}}
				ch, err := a.ProcessChatStream(context.Background(), req, "model", ep)
				if err != nil {
					t.Fatal(err)
				}
				last := ""
				for event := range ch {
					last = event.Type
				}
				if last != tc.want {
					t.Fatalf("terminal event = %q, want %q", last, tc.want)
				}
			})
		}
	}
}

func TestChatGPTStreamWithoutContentType(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, tc := range []struct {
			name, contentType, body, want string
			reject                        bool
		}{
			{"completed", "", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n", "done", false},
			{"empty", "", "", "error", false},
			{"json", "", `{"error":"not an event stream"}`, "error", false},
			{"incorrect type", "application/json", `{}`, "", true},
		} {
			name := tc.name + "/canonical"
			if native {
				name = tc.name + "/native"
			}
			t.Run(name, func(t *testing.T) {
				transport := responseTestTransport(func(r *http.Request) (*http.Response, error) {
					header := make(http.Header)
					if tc.contentType != "" {
						header.Set("Content-Type", tc.contentType)
					}
					return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})
				a := newAdapterWithTokenManagerFactory(0, transport, retry.Policy{}, func(string) (tokenSource, error) { return testTokenSource{token: "test"}, nil })
				req := &canonical.CanonicalChatRequest{}
				if native {
					req.Responses = &canonical.CanonicalResponsesRequest{}
				}
				ep := config.Endpoint{URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "creds"}}
				ch, err := a.ProcessChatStream(context.Background(), req, "model", ep)
				if tc.reject {
					if err == nil {
						t.Fatal("expected incorrect content type to be rejected")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				last := ""
				for event := range ch {
					last = event.Type
				}
				if last != tc.want {
					t.Fatalf("terminal event = %q, want %q", last, tc.want)
				}
			})
		}
	}
}
