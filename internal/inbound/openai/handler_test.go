package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestTranslateStreamEvent_EmptyChoices(t *testing.T) {
	h := &Handler{}

	// A done/usage-only event with no choice-triggering fields
	event := &canonical.CanonicalChatStreamEvent{
		Type: "done",
		Usage: &canonical.CanonicalUsage{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	}

	chunk := h.translateStreamEvent(event, "test-model")

	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("failed to marshal chunk: %v", err)
	}

	jsonStr := string(data)
	if strings.Contains(jsonStr, `"choices":null`) {
		t.Errorf("expected choices to be empty array, got null: %s", jsonStr)
	}

	if !strings.Contains(jsonStr, `"choices":[]`) {
		t.Errorf("expected choices to be serialized as empty array [], got: %s", jsonStr)
	}

	if !strings.Contains(jsonStr, `"usage"`) {
		t.Errorf("expected usage to be present in json: %s", jsonStr)
	}
}

func TestTranslateResponse_EmptyChoices(t *testing.T) {
	h := &Handler{}

	res := &canonical.CanonicalChatResponse{
		ID:      "test-id",
		Model:   "test-model",
		Choices: nil, // empty choices
	}

	translated := h.translateResponse(res)

	data, err := json.Marshal(translated)
	if err != nil {
		t.Fatalf("failed to marshal response: %v", err)
	}

	jsonStr := string(data)
	if strings.Contains(jsonStr, `"choices":null`) {
		t.Errorf("expected choices to be empty array, got null: %s", jsonStr)
	}

	if !strings.Contains(jsonStr, `"choices":[]`) {
		t.Errorf("expected choices to be serialized as empty array [], got: %s", jsonStr)
	}
}
