package openai

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/mayer-tech/llmbrane/internal/core"
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

// RegisterRoutes registers the OpenAI inbound routes with the given prefix.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("POST "+prefix+"/v1/chat/completions", h.HandleChatCompletions)
	mux.HandleFunc("POST "+prefix+"/v1/responses", h.HandleResponses)
	mux.HandleFunc("GET "+prefix+"/v1/models", h.HandleListModels)
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
