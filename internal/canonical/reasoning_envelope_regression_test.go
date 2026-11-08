package canonical

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestReasoningEnvelopePreservesNativeJSON(t *testing.T) {
	artifact := json.RawMessage(`{ "type" : "thinking", "thinking":"<x>&", "signature":"gwNative", "extra": {"n":9007199254740993,"escape":"\u003c"} }`)
	value, err := EncodeReasoningEnvelope("anthropic", artifact)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeReasoningEnvelope(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != string(artifact) {
		t.Fatalf("artifact changed: %s", got.Data)
	}
}

func TestReasoningEnvelopeRejectsInvalidUTF8(t *testing.T) {
	artifact := append([]byte(`{"type":"thinking","thinking":"`), 0xff)
	artifact = append(artifact, []byte(`","signature":"sig"}`)...)
	if _, err := EncodeReasoningEnvelope("anthropic", artifact); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	wire := append([]byte(`{"v":1,"origin":"anthropic-messages","artifact":`), artifact...)
	wire = append(wire, '}')
	if _, err := DecodeReasoningEnvelope("gw1." + base64.RawURLEncoding.EncodeToString(wire)); err == nil {
		t.Fatal("decoded invalid UTF-8")
	}
}
