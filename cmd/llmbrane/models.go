package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/inbound"
)

// Endpoint order expresses the preferred inbound protocols for each outbound API.
var modelEndpointPriority = map[string][]string{
	"openai-responses":        {"openai/v1/responses", "openai/v1/chat/completions", "anthropic/v1/messages"},
	"anthropic-messages":      {"anthropic/v1/messages", "openai/v1/responses", "openai/v1/chat/completions"},
	"openai-chat-completions": {"openai/v1/chat/completions", "anthropic/v1/messages"},
	"bedrock-converse":        {"anthropic/v1/messages", "openai/v1/chat/completions"},
}

type listedModel struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Endpoints []string `json:"endpoints"`
}

func registerModelsRoute(mux *http.ServeMux, gateway *core.Gateway, inference map[string][]inbound.Route) {
	registered := make(map[string]bool)
	for prefix, routes := range inference {
		for _, route := range routes {
			if route.Method == http.MethodPost && route.Handler != nil {
				registered[strings.TrimPrefix(prefix+route.Path, "/")] = true
			}
		}
	}

	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		names := gateway.ConfiguredModels()
		sort.Strings(names)
		models := make([]listedModel, 0, len(names))
		for _, name := range names {
			model := listedModel{ID: name, Name: name, Endpoints: []string{}}
			if route, err := gateway.ResolveRoute(name); err == nil && gateway.HasOutboundAdapter(route.APIShape) {
				for _, endpoint := range modelEndpointPriority[route.APIShape] {
					if registered[endpoint] {
						model.Endpoints = append(model.Endpoints, endpoint)
					}
				}
			}
			models = append(models, model)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Data []listedModel `json:"data"`
		}{Data: models})
	})
}
