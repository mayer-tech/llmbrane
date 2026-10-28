package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestTranslateRequestDoesNotReplayForeignReasoning(t *testing.T) {
	req := &canonical.CanonicalChatRequest{Messages: []canonical.CanonicalMessage{{
		Role: canonical.RoleAssistant,
		Content: []canonical.CanonicalContentPart{
			{Type: "reasoning", Reasoning: &canonical.CanonicalReasoning{Content: "foreign"}, Native: &canonical.CanonicalProviderData{Provider: "other", Format: "thinking", Data: json.RawMessage(`{"type":"thinking","signature":"foreign-secret"}`)}},
			{Type: "text", Text: "visible"},
		},
	}}}
	result, err := NewAdapter().translateRequest(req, "model", config.Endpoint{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result.Messages[len(result.Messages)-1].Content)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `[{"type":"text","text":"visible"}]` {
		t.Fatalf("unexpected content: %s", data)
	}
}
