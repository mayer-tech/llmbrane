package common

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

// WithConversationID preserves the conversation ID from the request headers.
func WithConversationID(r *http.Request) context.Context {
	if id := canonical.ConversationIDFromHeaders(r.Header); id != "" {
		return canonical.WithConversationID(r.Context(), id)
	}
	return r.Context()
}

// WriteError writes an OpenAI-compatible error response.
func WriteError(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(canonical.OpenAIErrorResponse{
		Error: canonical.OpenAIErrorDetail{Message: message, Type: errType},
	})
}

// WriteGatewayError writes a gateway error in OpenAI format.
func WriteGatewayError(w http.ResponseWriter, err error) {
	if gwErr, ok := err.(*canonical.GatewayError); ok {
		w.Header().Set("Content-Type", "application/json")
		if gwErr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(gwErr.RetryAfter))
		}
		w.WriteHeader(gwErr.StatusCode)
		json.NewEncoder(w).Encode(gwErr.ToOpenAIFormat())
		return
	}
	WriteError(w, http.StatusInternalServerError, err.Error(), "internal_error")
}
