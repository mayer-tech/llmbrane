package openai

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/inbound"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai/chatcompletions"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai/common"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai/responses"
)

// Handler exposes OpenAI-compatible routes.
type Handler struct {
	gateway   *core.Gateway
	chat      *chatcompletions.Handler
	responses *responses.Handler
}

// NewHandler creates an OpenAI inbound handler.
func NewHandler(gateway *core.Gateway) *Handler {
	return &Handler{gateway: gateway, chat: chatcompletions.NewHandler(gateway), responses: responses.NewHandler(gateway)}
}

// InferenceRoutes returns the OpenAI inference endpoints.
func (h *Handler) InferenceRoutes() []inbound.Route {
	return []inbound.Route{
		{Method: http.MethodPost, Path: "/v1/chat/completions", Handler: h.HandleChatCompletions},
		{Method: http.MethodPost, Path: "/v1/responses", Handler: h.HandleResponses},
	}
}

// ModelRoutes returns the OpenAI model endpoints.
func (h *Handler) ModelRoutes() []inbound.Route {
	return []inbound.Route{{Method: http.MethodGet, Path: "/v1/models", Handler: h.HandleListModels}}
}

// HandleChatCompletions processes a Chat Completions request.
func (h *Handler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	h.chat.HandleChatCompletions(w, r)
}

// HandleResponses processes a Responses request.
func (h *Handler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	h.responses.HandleResponses(w, r)
}

// HandleListModels lists configured models.
func (h *Handler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		common.WriteError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request")
		return
	}

	configuredModels := h.gateway.ConfiguredModels()
	now := time.Now().Unix()

	models := make([]Model, 0, len(configuredModels))
	for _, m := range configuredModels {
		models = append(models, Model{
			ID:      m,
			Object:  "model",
			Created: now,
			OwnedBy: "system",
		})
	}

	res := ModelList{
		Object: "list",
		Data:   models,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}
