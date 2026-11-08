package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/inbound"
	"github.com/mayer-tech/llmbrane/internal/inbound/anthropic"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai"
	outbound "github.com/mayer-tech/llmbrane/internal/outbound/openai"
)

func TestAggregatedModels(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"p": {Endpoints: []config.Endpoint{
				{API: "openai-responses", Models: map[string]config.ModelConf{"responses": {}}},
				{API: "anthropic-messages", Models: map[string]config.ModelConf{"messages": {}}},
				{API: "openai-chat-completions", Models: map[string]config.ModelConf{"chat": {}, "chat-direct": {}}},
				{API: "bedrock-converse", Models: map[string]config.ModelConf{"bedrock": {}}},
				{API: "unknown", Models: map[string]config.ModelConf{"unknown": {}}},
				{API: "missing", Models: map[string]config.ModelConf{"missing": {}}},
			}},
		},
		Aliases: map[string]config.Alias{
			"alias":  {Target: "p/responses"},
			"bad":    {Target: "p/not-found"},
			"chain":  {Target: "alias"},
			"p/chat": {Target: "p/messages"},
		},
	}
	gateway := core.NewGateway(cfg)
	for _, api := range []string{"openai-responses", "anthropic-messages", "openai-chat-completions", "bedrock-converse", "unknown"} {
		gateway.RegisterOutboundAdapter(api, outbound.NewAdapter())
	}
	oai := openai.NewHandler(gateway)
	anth := anthropic.NewHandler(gateway)
	inference := map[string][]inbound.Route{"/openai": oai.InferenceRoutes(), "/anthropic": anth.InferenceRoutes()}
	responses := []string{"../openai/v1/responses", "../openai/v1/chat/completions", "../anthropic/v1/messages"}
	messages := []string{"../anthropic/v1/messages", "../openai/v1/responses", "../openai/v1/chat/completions"}
	empty := []string{}
	want := []listedModel{
		{"alias", "alias", responses},
		{"bad", "bad", empty},
		{"chain", "chain", empty},
		{"p/bedrock", "p/bedrock", []string{"../anthropic/v1/messages", "../openai/v1/chat/completions"}},
		{"p/chat", "p/chat", messages},
		{"p/chat-direct", "p/chat-direct", []string{"../openai/v1/chat/completions", "../anthropic/v1/messages"}},
		{"p/messages", "p/messages", messages},
		{"p/missing", "p/missing", empty},
		{"p/responses", "p/responses", responses},
		{"p/unknown", "p/unknown", empty},
	}
	fetch := func(routes map[string][]inbound.Route) []listedModel {
		t.Helper()
		mux := http.NewServeMux()
		registerModelsRoute(mux, gateway, routes)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("response: %d %s", w.Code, w.Body.String())
		}
		var result struct {
			Data []listedModel `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Data
	}
	if got := fetch(inference); !reflect.DeepEqual(got, want) {
		t.Errorf("models = %#v, want %#v", got, want)
	}
	// References also resolve correctly when the gateway is behind a path prefix.
	base, _ := url.Parse("https://example.com/gateway/v1/models")
	for _, model := range fetch(inference) {
		for _, endpoint := range model.Endpoints {
			ref, err := url.Parse(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			resolved := base.ResolveReference(ref).Path
			if resolved != "/gateway/"+endpoint[3:] {
				t.Errorf("endpoint %q resolves to %q", endpoint, resolved)
			}
		}
	}
	// A known API shape without its adapter is not serviceable either.
	gateway = core.NewGateway(cfg)
	for _, model := range fetch(inference) {
		if len(model.Endpoints) != 0 {
			t.Errorf("endpoints without adapters for %s: %v", model.ID, model.Endpoints)
		}
	}
	for _, api := range []string{"openai-responses", "anthropic-messages", "openai-chat-completions", "bedrock-converse"} {
		gateway.RegisterOutboundAdapter(api, outbound.NewAdapter())
	}
	// A missing Responses route must never be advertised.
	inference["/openai"] = inference["/openai"][:1]
	for _, model := range fetch(inference) {
		for _, endpoint := range model.Endpoints {
			if endpoint == "../openai/v1/responses" {
				t.Errorf("unregistered endpoint for %s", model.ID)
			}
		}
	}
}

func TestAggregatedModelsEmpty(t *testing.T) {
	mux := http.NewServeMux()
	registerModelsRoute(mux, core.NewGateway(&config.Config{}), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Body.String() != "{\"data\":[]}\n" {
		t.Errorf("body = %q", w.Body.String())
	}
}
