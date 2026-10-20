package anthropic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
)

func TestHandleListModels(t *testing.T) {
	// Create a gateway with some configured aliases/models
	cfg := &config.Config{
		Aliases: map[string]config.Alias{
			"claude-3-5-sonnet-20240620": {Target: "anthropic/claude-3-5-sonnet-20240620"},
			"claude-3-opus-20240229":     {Target: "anthropic/claude-3-opus-20240229"},
		},
	}
	gateway := core.NewGateway(cfg)
	handler := NewHandler(gateway)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()

	handler.HandleListModels(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Result().StatusCode)
	}

	var res ModelList
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(res.Data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(res.Data))
	}

	// Gateway models map isn't ordered, so we just check presence
	modelsMap := make(map[string]bool)
	for _, m := range res.Data {
		if m.Type != "model" {
			t.Errorf("expected type model, got %s", m.Type)
		}
		if m.CreatedAt == "" {
			t.Errorf("expected created_at to not be empty")
		}
		modelsMap[m.ID] = true
	}

	if !modelsMap["claude-3-5-sonnet-20240620"] {
		t.Errorf("expected claude-3-5-sonnet-20240620 in models list")
	}
	if !modelsMap["claude-3-opus-20240229"] {
		t.Errorf("expected claude-3-opus-20240229 in models list")
	}

	if res.FirstID == "" || res.LastID == "" {
		t.Errorf("expected first_id and last_id to be populated")
	}
}

func TestTranslateRequest(t *testing.T) {
	h := &Handler{} // No gateway needed for translation tests

	reqBody := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	var req MessagesRequest
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("failed to unmarshal request: %v", err)
	}

	canonicalReq, err := h.translateRequest(&req)
	if err != nil {
		t.Fatalf("translateRequest failed: %v", err)
	}

	if canonicalReq.Model != "claude-3-5-sonnet-20240620" {
		t.Errorf("expected model claude-3-5-sonnet-20240620, got %s", canonicalReq.Model)
	}

	if len(canonicalReq.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(canonicalReq.Messages))
	}

	if canonicalReq.Messages[0].Role != "user" {
		t.Errorf("expected role user, got %s", canonicalReq.Messages[0].Role)
	}

	if len(canonicalReq.Messages[0].Content) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(canonicalReq.Messages[0].Content))
	}

	if canonicalReq.Messages[0].Content[0].Text != "Hello" {
		t.Errorf("expected text Hello, got %s", canonicalReq.Messages[0].Content[0].Text)
	}
}

func TestTranslateResponse(t *testing.T) {
	h := &Handler{}

	canonicalRes := &canonical.CanonicalChatResponse{
		ID:    "msg_123",
		Model: "claude-3-5-sonnet-20240620",
		Choices: []canonical.CanonicalChoice{
			{
				Index: 0,
				Message: canonical.CanonicalMessage{
					Role: "assistant",
					Content: []canonical.CanonicalContentPart{
						{Type: "text", Text: "Hi there!"},
					},
				},
			},
		},
		Stop: &canonical.CanonicalStop{
			Reason: "stop",
		},
		Usage: &canonical.CanonicalUsage{
			InputTokens:  10,
			OutputTokens: 5,
		},
	}

	res := h.translateResponse(canonicalRes)

	if res.ID != "msg_123" {
		t.Errorf("expected ID msg_123, got %s", res.ID)
	}
	if res.Role != "assistant" {
		t.Errorf("expected role assistant, got %s", res.Role)
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(res.Content))
	}
	if res.Content[0].Text != "Hi there!" {
		t.Errorf("expected text Hi there!, got %s", res.Content[0].Text)
	}
	if res.StopReason == nil || *res.StopReason != "end_turn" {
		t.Errorf("expected stop reason end_turn, got %v", res.StopReason)
	}
	if res.Usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", res.Usage.InputTokens)
	}
}
