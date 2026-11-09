package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
)

type discoveryTestToken struct{}

func (discoveryTestToken) Token(context.Context) (string, error) { return "test-token", nil }

type discoveryTransport func(*http.Request) (*http.Response, error)

func (f discoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchChatGPTModels(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		want       []string
		fail       bool
	}{
		{"catalog", `{"models":[{"slug":"gpt-one","visibility":"list"},{"slug":"gpt-one","visibility":"list"},{"slug":"hidden","visibility":"hide"},{"slug":"bad/name","visibility":"list"},{"slug":"gpt-two","visibility":"list"}]}`, 200, []string{"gpt-one", "gpt-two"}, false},
		{"empty", `{"models":[]}`, 200, nil, false},
		{"missing", `{"data":[]}`, 200, nil, true},
		{"null", `{"models":null}`, 200, nil, true},
		{"wrong type", `{"models":{}}`, 200, nil, true},
		{"malformed", `{`, 200, nil, true},
		{"status", `{}`, 403, nil, true},
		{"oversized", strings.Repeat("x", maxDiscoveredModelsResponse+1), 200, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: discoveryTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://api.openai.com/v1/models" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("incorrect discovery request")
				}
				return &http.Response{StatusCode: tt.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tt.body)), Request: r}, nil
			})}
			got, err := fetchChatGPTModels(context.Background(), client, discoveryTestToken{})
			if (err != nil) != tt.fail || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, error %v; want %v, failure %v", got, err, tt.want, tt.fail)
			}
		})
	}
}

func TestDiscoveryRejectsRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: discoveryTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://other.example/"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	_, err := fetchChatGPTModels(context.Background(), client, discoveryTestToken{})
	if err == nil || calls != 1 {
		t.Fatalf("redirect: error %v, calls %d", err, calls)
	}
}

func TestDiscoverChatGPTModelsPriorityAndFallback(t *testing.T) {
	auth := &config.EndpointAuth{Type: "chatgpt", AutoDiscoverModels: true}
	cfg := &config.Config{Providers: map[string]config.Provider{
		"p":     {Endpoints: []config.Endpoint{{Auth: auth}, {Models: map[string]config.ModelConf{"explicit": {Target: "custom"}}}}},
		"other": {Endpoints: []config.Endpoint{{Models: map[string]config.ModelConf{"shared": {}}}}},
		"fail":  {Endpoints: []config.Endpoint{{Auth: auth, URL: "fail", Models: map[string]config.ModelConf{"kept": {}}}}},
		"off":   {Endpoints: []config.Endpoint{{Auth: &config.EndpointAuth{Type: "chatgpt"}}}},
	}}
	calls := 0
	discoverChatGPTModelsWith(context.Background(), cfg, func(_ context.Context, ep config.Endpoint) ([]string, error) {
		calls++
		if ep.URL == "fail" {
			return nil, errors.New("unavailable")
		}
		return []string{"explicit", "shared", "new"}, nil
	})
	p := cfg.Providers["p"].Endpoints
	if calls != 2 || len(p[0].Models) != 2 || p[0].Models["shared"].Target != "shared" || p[0].Models["new"].Target != "new" {
		t.Fatalf("calls %d, discovered %#v", calls, p[0].Models)
	}
	if _, ok := p[0].Models["explicit"]; ok {
		t.Fatal("explicit model shadowed")
	}
	if _, ok := cfg.Providers["fail"].Endpoints[0].Models["kept"]; !ok {
		t.Fatal("configured model lost after failure")
	}
	gateway := core.NewGateway(cfg)
	route, err := gateway.ResolveRoute("p/explicit")
	if err != nil || route.Model != "custom" {
		t.Fatalf("explicit route: %#v %v", route, err)
	}
	route, err = gateway.ResolveRoute("p/shared")
	if err != nil || route.Model != "shared" {
		t.Fatalf("discovered route: %#v %v", route, err)
	}
	found := false
	for _, name := range gateway.ConfiguredModels() {
		if name == "p/new" {
			found = true
		}
	}
	if !found {
		t.Fatal("discovered model missing from listing")
	}
}
