package openairesponses

import (
	"encoding/json"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestChatGPTSystemRoleTranslation(t *testing.T) {
	for _, chatgpt := range []bool{false, true} {
		ep := config.Endpoint{}
		want := "system"
		if chatgpt {
			ep.Auth = &config.EndpointAuth{Type: "chatgpt"}
			want = "developer"
		}
		req := &canonical.CanonicalChatRequest{Messages: []canonical.CanonicalMessage{{Role: canonical.RoleSystem, Content: []canonical.CanonicalContentPart{{Type: "text", Text: "rules"}}}}}
		out, err := (&Adapter{}).translateCanonicalRequest(req, "model", true, ep)
		if err != nil || len(out.Input) != 1 || out.Input[0].Role != want {
			t.Fatalf("chatgpt=%t: input=%+v error=%v", chatgpt, out.Input, err)
		}
	}
}

func TestChatGPTNativeSystemRoleTranslation(t *testing.T) {
	for _, chatgpt := range []bool{false, true} {
		ep := config.Endpoint{}
		want := "system"
		if chatgpt {
			ep.Auth = &config.EndpointAuth{Type: "chatgpt"}
			want = "developer"
		}
		raw := json.RawMessage(`{"type":"message","role":"system","content":"rules","extra":42}`)
		req := &canonical.CanonicalChatRequest{Responses: &canonical.CanonicalResponsesRequest{}, Messages: []canonical.CanonicalMessage{{Content: []canonical.CanonicalContentPart{{Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.item", Data: raw}}}}}}
		body, err := (&Adapter{}).nativeBody(req, "model", ep)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Input []map[string]json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		var role string
		if len(result.Input) != 1 || json.Unmarshal(result.Input[0]["role"], &role) != nil || role != want || string(result.Input[0]["extra"]) != "42" {
			t.Fatalf("chatgpt=%t: unexpected body %s", chatgpt, body)
		}
		if string(raw) != `{"type":"message","role":"system","content":"rules","extra":42}` {
			t.Fatal("original item was mutated")
		}
	}
}
