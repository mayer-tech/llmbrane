package canonical

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func envelopeJSON(value string) string {
	return "gw1." + base64.RawURLEncoding.EncodeToString([]byte(value))
}

func TestReasoningEnvelopeRoundTrip(t *testing.T) {
	tests := []struct{ provider, format, origin, artifact string }{
		{"anthropic", "thinking", "anthropic-messages", `{"type":"thinking","thinking":"","signature":"sig","extra":{"n":9007199254740993}}`},
		{"anthropic", "redacted_thinking", "anthropic-messages", `{"type":"redacted_thinking","data":"opaque","extra":[1,2]}`},
		{"openai", "responses.item", "openai-responses", `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thought","extra":true}],"encrypted_content":"cipher","extra":{"n":9007199254740993}}`},
		{"openai", "responses.item", "openai-responses", `{"type":"reasoning","id":"rs_2","summary":[],"encrypted_content":"cipher"}`},
	}
	for _, tt := range tests {
		t.Run(tt.format+tt.artifact, func(t *testing.T) {
			value, err := EncodeReasoningEnvelope(tt.provider, json.RawMessage(tt.artifact))
			if err != nil {
				t.Fatal(err)
			}
			if !IsReasoningEnvelope(value) || !strings.HasPrefix(value, "gw1.") {
				t.Fatalf("invalid prefix: %s", value)
			}
			wire, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "gw1."))
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(wire, &object); err != nil {
				t.Fatal(err)
			}
			if len(object) != 3 || string(object["v"]) != "1" || string(object["origin"]) != `"`+tt.origin+`"` {
				t.Fatalf("unexpected envelope: %s", wire)
			}
			got, err := DecodeReasoningEnvelope(value)
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider != tt.provider || got.Format != tt.format {
				t.Fatalf("unexpected metadata: %+v", got)
			}
			var a, b any
			decode := func(raw []byte, out *any) {
				d := json.NewDecoder(strings.NewReader(string(raw)))
				d.UseNumber()
				if err := d.Decode(out); err != nil {
					t.Fatal(err)
				}
			}
			decode(got.Data, &a)
			decode([]byte(tt.artifact), &b)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("artifact mismatch: %s", got.Data)
			}
		})
	}
}

func TestReasoningEnvelopeRejectsInvalidArtifacts(t *testing.T) {
	tests := []struct{ provider, artifact string }{
		{"unknown", `{"type":"thinking","thinking":"x","signature":"sig"}`},
		{"anthropic", `null`}, {"anthropic", `[]`}, {"anthropic", `{}`},
		{"anthropic", `{"type":"text","text":"x"}`},
		{"anthropic", `{"type":"thinking","signature":"sig"}`},
		{"anthropic", `{"type":"thinking","thinking":"x","signature":""}`},
		{"anthropic", `{"type":"thinking","thinking":null,"signature":"sig"}`},
		{"anthropic", `{"type":"redacted_thinking","data":null}`},
		{"anthropic", `{"type":"redacted_thinking","data":""}`},
		{"anthropic", `{"type":"thinking","type":"text","thinking":"x","signature":"sig"}`},
		{"anthropic", `{"type":"thinking","Type":"text","thinking":"x","signature":"sig"}`},
		{"anthropic", `{"type":"thinking","thinking":"x","signature":"sig","Signature":"gw1.bad"}`},
		{"anthropic", `{"type":"thinking","thinking":"x","signature":"sig","\u0073ignature":"gw1.bad"}`},
		{"anthropic", `{"type":"thinking","thinking":"x","signature":"sig","\u017fignature":"gw1.bad"}`},
		{"anthropic", `{"type":"thinking","thinking":"x","signature":"gw2.bad"}`},
		{"anthropic", `{"type":"redacted_thinking","data":"gw1.bad"}`},
		{"openai", `{"type":"reasoning","id":"x","summary":[],"encrypted_content":"gw1.bad"}`},
		{"openai", `{"type":"reasoning","id":"x","summary":[{"type":"summary_text","Type":"other","text":"x"}],"encrypted_content":"s"}`},
		{"openai", `{"type":"message","id":"x","summary":[],"encrypted_content":"s"}`},
		{"openai", `{"type":"reasoning","summary":[],"encrypted_content":"s"}`},
		{"openai", `{"type":"reasoning","id":"x","summary":[],"encrypted_content":""}`},
		{"openai", `{"type":"reasoning","id":"x","summary":null,"encrypted_content":"s"}`},
		{"openai", `{"type":"reasoning","id":"x","encrypted_content":"s"}`},
		{"openai", `{"type":"reasoning","id":"x","summary":[{"type":"summary_text","text":null}],"encrypted_content":"s"}`},
		{"openai", `{"type":"reasoning","id":"x","summary":[{"type":"other","text":"x"}],"encrypted_content":"s"}`},
	}
	for _, tt := range tests {
		t.Run(tt.provider+tt.artifact, func(t *testing.T) {
			if _, err := EncodeReasoningEnvelope(tt.provider, json.RawMessage(tt.artifact)); err == nil {
				t.Fatal("encoding accepted invalid artifact")
			}
			origin := "anthropic-messages"
			if tt.provider == "openai" {
				origin = "openai-responses"
			}
			if tt.provider != "unknown" {
				value := envelopeJSON(`{"v":1,"origin":"` + origin + `","artifact":` + tt.artifact + `}`)
				if _, err := DecodeReasoningEnvelope(value); err == nil {
					t.Fatal("decoding accepted invalid artifact")
				}
			}
		})
	}
}

func TestReasoningEnvelopeRejectsInvalidEncoding(t *testing.T) {
	for _, value := range []string{
		"", "native", "gw", "gw2.abc", "gw1.", "gw1.***", "gw1.YQ==", "gw1.Y\nQ",
		envelopeJSON(`null`), envelopeJSON(`{}`),
		envelopeJSON(`{"v":2,"origin":"anthropic-messages","artifact":{"type":"redacted_thinking","data":"x"}}`),
		envelopeJSON(`{"V":1,"origin":"anthropic-messages","artifact":{"type":"redacted_thinking","data":"x"}}`),
		envelopeJSON(`{"v":2,"v":1,"origin":"anthropic-messages","artifact":{"type":"redacted_thinking","data":"x"}}`),
		envelopeJSON(`{"v":1,"V":2,"origin":"anthropic-messages","artifact":{"type":"redacted_thinking","data":"x"}}`),
		envelopeJSON(`{"v":1,"origin":"unknown","artifact":{"type":"redacted_thinking","data":"x"}}`),
		envelopeJSON(`{"v":1,"origin":"anthropic-messages","artifact":{"type":"redacted_thinking","data":"x"},"other":1}`),
		"gw1." + strings.Repeat("A", base64.RawURLEncoding.EncodedLen(maxReasoningEnvelopeBytes)+1),
	} {
		if _, err := DecodeReasoningEnvelope(value); err == nil {
			t.Fatalf("accepted invalid envelope %q", value[:min(len(value), 100)])
		}
	}
	if _, err := EncodeReasoningEnvelope("anthropic", json.RawMessage(strings.Repeat("x", maxReasoningEnvelopeBytes+1))); err == nil {
		t.Fatal("accepted oversized artifact")
	}
	for _, value := range []string{"gw1.", "gw2.abc", "gw123.bad"} {
		if !IsReasoningEnvelope(value) {
			t.Errorf("reserved prefix not recognized: %q", value)
		}
	}
	for _, value := range []string{"", "signature", "xgw1.foo", "gw", "gw-invalid", "gwNativeSignature", "gw2", "gw.abc"} {
		if IsReasoningEnvelope(value) {
			t.Errorf("unexpected reserved prefix: %q", value)
		}
	}
}
