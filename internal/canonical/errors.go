package canonical

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ErrorKind defines the standard types of gateway errors.
type ErrorKind string

const (
	ErrorKindInvalidRequest     ErrorKind = "invalid_request"
	ErrorKindAuthError          ErrorKind = "authentication_error"
	ErrorKindRateLimit          ErrorKind = "rate_limit"
	ErrorKindModelNotFound      ErrorKind = "model_not_found"
	ErrorKindInternalError      ErrorKind = "internal_error"
	ErrorKindProviderError      ErrorKind = "provider_error"
	ErrorKindTimeout            ErrorKind = "timeout"
	ErrorKindPolicyViolation    ErrorKind = "policy_violation"
	ErrorKindUnsupportedFeature ErrorKind = "unsupported_feature"
)

// GatewayError represents a normalized error across all providers.
type GatewayError struct {
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
	StatusCode int       `json:"status_code"`
	ErrorKind  ErrorKind `json:"error_kind"`
	Message    string    `json:"message"`
	RetryAfter int       `json:"retry_after,omitempty"` // in seconds
	Raw        []byte    `json:"-"`
}

func (e *GatewayError) Error() string {
	msg := string(e.ErrorKind)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Provider != "" || e.Model != "" {
		msg += fmt.Sprintf(" (provider: %s, model: %s)", e.Provider, e.Model)
	}
	return msg
}

// MapHTTPStatusCode maps a standard HTTP status code to an ErrorKind.
func MapHTTPStatusCode(statusCode int) ErrorKind {
	switch statusCode {
	case http.StatusBadRequest:
		return ErrorKindInvalidRequest
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrorKindAuthError
	case http.StatusNotFound:
		return ErrorKindModelNotFound
	case http.StatusTooManyRequests:
		return ErrorKindRateLimit
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return ErrorKindTimeout
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return ErrorKindProviderError
	default:
		if statusCode >= 500 {
			return ErrorKindInternalError
		}
		return ErrorKindProviderError
	}
}

// NewGatewayError creates a new GatewayError with mapped ErrorKind from the status code.
func NewGatewayError(provider, model, message string, statusCode int, raw []byte) *GatewayError {
	return &GatewayError{
		Provider:   provider,
		Model:      model,
		StatusCode: statusCode,
		ErrorKind:  MapHTTPStatusCode(statusCode),
		Message:    message,
		Raw:        raw,
	}
}

// OpenAIErrorResponse represents the expected error format for OpenAI-compatible inbound endpoints.
type OpenAIErrorResponse struct {
	Error OpenAIErrorDetail `json:"error"`
}

type OpenAIErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param,omitempty"`
	Code    *string `json:"code,omitempty"`
}

// ToOpenAIFormat translates a GatewayError into the standard OpenAI JSON error format.
func (e *GatewayError) ToOpenAIFormat() OpenAIErrorResponse {
	return OpenAIErrorResponse{
		Error: OpenAIErrorDetail{
			Message: e.Message,
			Type:    string(e.ErrorKind),
		},
	}
}

// WriteOpenAIError writes the GatewayError to an http.ResponseWriter in OpenAI format.
func WriteOpenAIError(w http.ResponseWriter, err error) {
	var gerr *GatewayError
	if ge, ok := err.(*GatewayError); ok {
		gerr = ge
	} else {
		gerr = &GatewayError{
			StatusCode: http.StatusInternalServerError,
			ErrorKind:  ErrorKindInternalError,
			Message:    err.Error(),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if gerr.RetryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", gerr.RetryAfter))
	}
	w.WriteHeader(gerr.StatusCode)

	_ = json.NewEncoder(w).Encode(gerr.ToOpenAIFormat())
}
