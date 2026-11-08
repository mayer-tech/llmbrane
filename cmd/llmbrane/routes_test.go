package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/inbound"
	"github.com/mayer-tech/llmbrane/internal/inbound/anthropic"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai/responses"
)

func TestInboundRoutes(t *testing.T) {
	gateway := core.NewGateway(&config.Config{})
	oai := openai.NewHandler(gateway)
	anth := anthropic.NewHandler(gateway)
	resp := responses.NewHandler(gateway)
	for _, group := range []struct {
		name   string
		routes []inbound.Route
		want   []struct{ method, path string }
	}{
		{"openai inference", oai.InferenceRoutes(), []struct{ method, path string }{{"POST", "/v1/chat/completions"}, {"POST", "/v1/responses"}}},
		{"openai models", oai.ModelRoutes(), []struct{ method, path string }{{"GET", "/v1/models"}}},
		{"anthropic inference", anth.InferenceRoutes(), []struct{ method, path string }{{"POST", "/v1/messages"}}},
		{"anthropic models", anth.ModelRoutes(), []struct{ method, path string }{{"GET", "/v1/models"}}},
		{"responses inference", resp.InferenceRoutes(), []struct{ method, path string }{{"POST", "/v1/responses"}}},
		{"responses models", resp.ModelRoutes(), nil},
	} {
		t.Run(group.name, func(t *testing.T) {
			if len(group.routes) != len(group.want) {
				t.Fatalf("got %d routes, want %d", len(group.routes), len(group.want))
			}
			for i, route := range group.routes {
				if route.Method != group.want[i].method || route.Path != group.want[i].path || route.Handler == nil {
					t.Errorf("unexpected route: %+v", route)
				}
			}
		})
	}

	mux := http.NewServeMux()
	registerRoutes(mux, "/openai", oai.InferenceRoutes(), oai.ModelRoutes())
	registerRoutes(mux, "/anthropic", anth.InferenceRoutes(), anth.ModelRoutes())
	registerModelsRoute(mux, gateway, map[string][]inbound.Route{
		"/openai":    oai.InferenceRoutes(),
		"/anthropic": anth.InferenceRoutes(),
	})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/openai/v1/chat/completions", "invalid", 400},
		{"POST", "/openai/v1/responses", "invalid", 400},
		{"POST", "/anthropic/v1/messages", "invalid", 400},
		{"GET", "/openai/v1/models", "", 200},
		{"GET", "/anthropic/v1/models", "", 200},
		{"HEAD", "/openai/v1/models", "", 405},
		{"GET", "/openai/v1/responses", "", 405},
		{"POST", "/anthropic/v1/models", "", 405},
		{"GET", "/v1/models", "", 200},
		{"HEAD", "/v1/models", "", 405},
		{"POST", "/v1/models", "", 405},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body)
			}
		})
	}

	standalone := http.NewServeMux()
	registerRoutes(standalone, "/openai", resp.InferenceRoutes(), resp.ModelRoutes())
	w := httptest.NewRecorder()
	standalone.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader("invalid")))
	if w.Code != http.StatusBadRequest {
		t.Errorf("standalone responses status = %d, want 400", w.Code)
	}
}
