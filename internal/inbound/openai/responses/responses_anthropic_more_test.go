package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestResponsesAnthropicStreamErrorAfterArtifact(t *testing.T) {
	events := anthropicStreamEvents()[:5]
	events = append(events, &canonical.CanonicalChatStreamEvent{Type: "error", Error: &canonical.GatewayError{Message: "connection lost"}})
	result := mappedEvents(t, events)
	if result[len(result)-1]["type"] != "error" {
		t.Fatalf("events: %#v", result)
	}
	// A completed reasoning block is withheld if the stream fails before its stop reason.
	if len(result) != 2 || result[0]["type"] != "response.created" {
		t.Fatalf("buffered artifacts were delivered after error: %#v", result)
	}
}

func TestResponsesAnthropicStreamIncompleteAndEmptyThinking(t *testing.T) {
	events := []*canonical.CanonicalChatStreamEvent{
		{Type: "message_start", ResponseID: "m"},
		{Type: "content_start", Delta: nativePart("thinking", `{"type":"thinking","thinking":"","signature":"sig"}`)},
		{Type: "content_stop"},
		{Type: "done", Stop: &canonical.CanonicalStop{Reason: "length"}},
	}
	result := mappedEvents(t, events)
	last := result[len(result)-1]
	if last["type"] != "response.incomplete" {
		t.Fatalf("events: %#v", result)
	}
	var item map[string]any
	for _, event := range result {
		if strings.HasPrefix(event["type"].(string), "response.reasoning_summary_") {
			t.Fatalf("invented summary event: %#v", event)
		}
		if event["type"] == "response.output_item.done" {
			item = event["item"].(map[string]any)
		}
	}
	decoded, err := canonical.DecodeReasoningEnvelope(item["encrypted_content"].(string))
	if err != nil || decoded.Format != "thinking" || len(item["summary"].([]any)) != 0 {
		t.Fatalf("item %#v: %v", item, err)
	}
}

func TestResponsesNativeEnvelopeCompatibility(t *testing.T) {
	openaiArtifact := json.RawMessage(`{"type":"reasoning","id":"r","summary":[],"encrypted_content":"opaque","status":"completed"}`)
	openaiValue, err := canonical.EncodeReasoningEnvelope("openai", openaiArtifact)
	if err != nil {
		t.Fatal(err)
	}
	anthropicValue, err := canonical.EncodeReasoningEnvelope("anthropic", json.RawMessage(`{"type":"thinking","thinking":"x","signature":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{{openaiValue, true}, {anthropicValue, false}, {"gw3.invalid", false}, {"native-ciphertext", true}} {
		fields := map[string]json.RawMessage{"model": json.RawMessage(`"p/m"`)}
		fields["input"], _ = json.Marshal([]any{map[string]any{"type": "reasoning", "id": "r", "summary": []any{}, "encrypted_content": tc.value}})
		req, err := translateResponsesRequest(fields)
		if err != nil {
			t.Fatal(err)
		}
		err = prepareResponsesRoute(req, "openai-responses")
		if (err == nil) != tc.valid {
			t.Fatalf("%q: %v", tc.value, err)
		}
		if tc.value == openaiValue {
			var item map[string]any
			_ = json.Unmarshal(req.Messages[0].Content[0].Native.Data, &item)
			if item["encrypted_content"] != "opaque" || item["status"] != nil {
				t.Fatalf("native item: %#v", item)
			}
		}
	}
}
