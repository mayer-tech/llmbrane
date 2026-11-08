package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
)

func TestRegisterRoutes(t *testing.T) {
	mux := http.NewServeMux()
	NewHandler(core.NewGateway(&config.Config{})).RegisterRoutes(mux, "/openai")

	for _, tc := range []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodPost, "/openai/v1/chat/completions", "invalid", http.StatusBadRequest},
		{http.MethodPost, "/openai/v1/responses", "invalid", http.StatusBadRequest},
		{http.MethodGet, "/openai/v1/models", "", http.StatusOK},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body)
			}
		})
	}
}

func TestHandleListModels(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"provider": {Endpoints: []config.Endpoint{{Models: map[string]config.ModelConf{
				"model": {Target: "upstream"},
			}}}},
		},
		Aliases: map[string]config.Alias{"alias": {Target: "provider/model"}},
	}
	h := NewHandler(core.NewGateway(cfg))
	w := httptest.NewRecorder()
	before := time.Now().Unix()
	h.HandleListModels(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	after := time.Now().Unix()

	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d, content type = %q", w.Code, w.Header().Get("Content-Type"))
	}
	var result ModelList
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "list" || len(result.Data) != 2 {
		t.Fatalf("unexpected model list: %+v", result)
	}
	seen := map[string]bool{}
	for _, model := range result.Data {
		if model.Object != "model" || model.OwnedBy != "system" || model.Created < before || model.Created > after {
			t.Errorf("unexpected model: %+v", model)
		}
		seen[model.ID] = true
	}
	if !seen["provider/model"] || !seen["alias"] {
		t.Errorf("unexpected model IDs: %v", seen)
	}
}

func TestHandleListModelsEmptyAndMethod(t *testing.T) {
	h := NewHandler(core.NewGateway(&config.Config{}))
	w := httptest.NewRecorder()
	h.HandleListModels(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if strings.TrimSpace(w.Body.String()) != `{"object":"list","data":[]}` {
		t.Errorf("unexpected empty response: %s", w.Body)
	}

	w = httptest.NewRecorder()
	h.HandleListModels(w, httptest.NewRequest(http.MethodPost, "/v1/models", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
