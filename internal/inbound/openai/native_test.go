package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestNativeReasoningOnlyExposesReadableText(t *testing.T) {
	part := canonical.CanonicalContentPart{
		Type:      "reasoning",
		Reasoning: &canonical.CanonicalReasoning{Content: "readable"},
		Native:    &canonical.CanonicalProviderData{Provider: "anthropic", Format: "thinking", Data: json.RawMessage(`{"type":"thinking","thinking":"readable","signature":"private-signature"}`)},
	}
	h := &Handler{}
	response := &canonical.CanonicalChatResponse{Choices: []canonical.CanonicalChoice{{Message: canonical.CanonicalMessage{Content: []canonical.CanonicalContentPart{part}}}}}
	for _, value := range []any{h.translateResponse(response), h.translateStreamEvent(&canonical.CanonicalChatStreamEvent{Type: "content_delta", Delta: &part}, "model")} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"reasoning_content":"readable"`) || strings.Contains(string(data), "private-signature") {
			t.Fatalf("unexpected OpenAI response: %s", data)
		}
	}
	signature := &canonical.CanonicalContentPart{Type: "reasoning", Reasoning: &canonical.CanonicalReasoning{}, Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: "signature_delta", Data: json.RawMessage(`{"type":"signature_delta","signature":"private-signature"}`)}}
	chunk := h.translateStreamEvent(&canonical.CanonicalChatStreamEvent{Type: "content_delta", Delta: signature}, "model")
	if len(chunk.Choices) != 0 {
		t.Fatalf("signature-only event leaked: %+v", chunk)
	}
}
