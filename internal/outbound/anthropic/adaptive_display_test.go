package anthropic

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestAdaptiveDisplayTranslation(t *testing.T) {
	tests := []struct {
		name       string
		params     map[string]any
		effort     string
		extensions map[string]any
		thinking   string
		output     string
		fail       bool
	}{
		{name: "absent"},
		{name: "adaptive", params: map[string]any{"llmbrane.anthropic_adaptive": true}, thinking: `{"type":"adaptive"}`},
		{name: "adaptive summarized", params: map[string]any{"llmbrane.anthropic_adaptive": true, "llmbrane.anthropic_thinking_display": "summarized"}, thinking: `{"type":"adaptive","display":"summarized"}`},
		{name: "adaptive omitted and effort", params: map[string]any{"llmbrane.anthropic_adaptive": true, "llmbrane.anthropic_thinking_display": "omitted"}, effort: "high", thinking: `{"type":"adaptive","display":"omitted"}`, output: `{"effort":"high"}`},
		{name: "effort with manual display", params: map[string]any{"llmbrane.anthropic_thinking_display": "summarized"}, effort: "low", extensions: map[string]any{"thinking_mode": "budget"}, thinking: `{"type":"enabled","budget_tokens":2048,"display":"summarized"}`},
		{name: "false adaptive marker", params: map[string]any{"llmbrane.anthropic_adaptive": false}},
		{name: "bad adaptive marker", params: map[string]any{"llmbrane.anthropic_adaptive": "true"}, fail: true},
		{name: "bad display type", params: map[string]any{"llmbrane.anthropic_adaptive": true, "llmbrane.anthropic_thinking_display": true}, fail: true},
		{name: "bad display value", params: map[string]any{"llmbrane.anthropic_adaptive": true, "llmbrane.anthropic_thinking_display": "updates"}, fail: true},
		{name: "null display", params: map[string]any{"llmbrane.anthropic_adaptive": true, "llmbrane.anthropic_thinking_display": nil}, fail: true},
		{name: "display without thinking", params: map[string]any{"llmbrane.anthropic_thinking_display": "omitted"}, fail: true},
		{name: "display with disabled thinking", params: map[string]any{"llmbrane.anthropic_thinking_display": "omitted"}, effort: "none", fail: true},
		{name: "adaptive in budget mode", params: map[string]any{"llmbrane.anthropic_adaptive": true}, extensions: map[string]any{"thinking_mode": "budget"}, fail: true},
		{name: "adaptive in budget mode with effort", params: map[string]any{"llmbrane.anthropic_adaptive": true}, effort: "low", extensions: map[string]any{"thinking_mode": "budget"}, fail: true},
		{name: "adaptive with none", params: map[string]any{"llmbrane.anthropic_adaptive": true}, effort: "none", fail: true},
		{name: "adaptive mapped to none", params: map[string]any{"llmbrane.anthropic_adaptive": true}, effort: "high", extensions: map[string]any{"reasoning_effort_mapping": map[string]string{"high": "none"}}, fail: true},
		{name: "mapped none to active", params: map[string]any{"llmbrane.anthropic_adaptive": true}, effort: "none", extensions: map[string]any{"reasoning_effort_mapping": map[string]string{"none": "low"}}, thinking: `{"type":"adaptive"}`, output: `{"effort":"low"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{ProviderSpecific: tt.params}}
			if tt.effort != "" {
				req.Params.ReasoningEffort = &tt.effort
			}
			out, err := NewAdapter().translateRequest(req, "model", config.Endpoint{Extensions: tt.extensions})
			if tt.fail {
				ge, ok := err.(*canonical.GatewayError)
				if !ok || ge.StatusCode != http.StatusBadRequest {
					t.Fatalf("expected 400, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"thinking": tt.thinking, "output_config": tt.output} {
				got, present := wire[key]
				if want == "" {
					if present {
						t.Fatalf("unexpected %s: %s", key, got)
					}
				} else if !present || string(got) != want {
					t.Fatalf("%s = %s, want %s", key, got, want)
				}
			}
		})
	}
}
