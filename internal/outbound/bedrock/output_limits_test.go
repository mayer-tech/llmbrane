package bedrock

import (
	"encoding/json"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestExplicitZeroOutputLimitIsNotDropped(t *testing.T) {
	zero := 0
	out := NewAdapter().translateRequest(&canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{MaxOutputTokens: &zero}})
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		InferenceConfig map[string]json.RawMessage `json:"inferenceConfig"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire.InferenceConfig["maxTokens"]) != "0" {
		t.Fatalf("payload = %s", data)
	}
}
