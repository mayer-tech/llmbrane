package bedrock

import (
	"encoding/json"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestTranslateRequestSkipsForeignNativeReasoning(t *testing.T) {
	req := &canonical.CanonicalChatRequest{Messages: []canonical.CanonicalMessage{{
		Role: canonical.RoleAssistant,
		Content: []canonical.CanonicalContentPart{
			{Type: "reasoning", Reasoning: &canonical.CanonicalReasoning{Content: "foreign thinking"}, Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: "thinking", Data: json.RawMessage(`{"type":"thinking","signature":"foreign-secret"}`)}},
			{Type: "text", Text: "visible"},
		},
	}}}
	got := NewAdapter().translateRequest(req)
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 || got.Messages[0].Content[0].Text != "visible" || got.Messages[0].Content[0].ReasoningContent != nil {
		t.Fatalf("unexpected Bedrock content: %+v", got.Messages)
	}
}
