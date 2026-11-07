package openairesponses

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestNativeBodySettings(t *testing.T) {
	temp, top := 0.4, 0.9
	limit := 300
	effort := "high"
	req := &canonical.CanonicalChatRequest{
		Stream: true, Params: canonical.CanonicalParams{Temperature: &temp, TopP: &top, MaxOutputTokens: &limit, ReasoningEffort: &effort},
		Responses: &canonical.CanonicalResponsesRequest{Settings: map[string]json.RawMessage{
			"reasoning": json.RawMessage(`{"summary":"detailed","vendor_extra":{"x":1}}`),
			"include":   json.RawMessage(`["reasoning.encrypted_content"]`),
		}},
		Messages: []canonical.CanonicalMessage{{Role: canonical.RoleAssistant, Content: []canonical.CanonicalContentPart{{Type: "native", Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.item", Data: json.RawMessage(`{"type":"reasoning","id":"x","encrypted_content":"opaque"}`)}}}}},
	}
	endpoint := config.Endpoint{Extensions: map[string]any{"omit_params": []any{"temperature", "max_output_tokens"}, "reasoning_effort_mapping": map[string]string{"high": "medium"}, "disable_encrypted_reasoning": true}}
	body, err := (&Adapter{}).nativeBody(req, "resolved", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "resolved" || got["store"] != false || got["stream"] != true || got["top_p"] != top {
		t.Fatalf("wrong body: %s", body)
	}
	if _, ok := got["temperature"]; ok {
		t.Fatalf("temperature was not omitted: %s", body)
	}
	if _, ok := got["max_output_tokens"]; ok {
		t.Fatalf("max_output_tokens was not omitted: %s", body)
	}
	if !reflect.DeepEqual(got["include"], []any{"reasoning.encrypted_content"}) {
		t.Fatalf("explicit include changed: %s", body)
	}
	if !reflect.DeepEqual(got["reasoning"], map[string]any{"effort": "medium", "summary": "detailed", "vendor_extra": map[string]any{"x": float64(1)}}) {
		t.Fatalf("reasoning lost: %s", body)
	}
	delete(req.Responses.Settings, "include")
	body, err = (&Adapter{}).nativeBody(req, "resolved", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	_ = json.Unmarshal(body, &got)
	if _, ok := got["include"]; ok {
		t.Fatalf("disabled include still sent: %s", body)
	}
	delete(endpoint.Extensions, "disable_encrypted_reasoning")
	body, err = (&Adapter{}).nativeBody(req, "resolved", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	_ = json.Unmarshal(body, &got)
	if !reflect.DeepEqual(got["include"], []any{"reasoning.encrypted_content"}) {
		t.Fatalf("automatic include missing: %s", body)
	}
}

func TestNativeBodyRejectsMissingItem(t *testing.T) {
	req := &canonical.CanonicalChatRequest{Responses: &canonical.CanonicalResponsesRequest{}, Messages: []canonical.CanonicalMessage{{Role: canonical.RoleUser}}}
	if _, err := (&Adapter{}).nativeBody(req, "model", config.Endpoint{}); err == nil {
		t.Fatal("expected error")
	}
}
