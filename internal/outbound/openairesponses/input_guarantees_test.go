package openairesponses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestAnthropicToolGuaranteeTranslation(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		for _, strict := range []bool{true, false} {
			req := anthropicRequest()
			req.Params.ProviderSpecific["llmbrane.anthropic_disable_parallel_tool_use"] = disabled
			req.Tools = []canonical.CanonicalTool{{Type: "function", Name: "f", InputSchema: map[string]any{"maximum": json.Number("9007199254740993")}, Metadata: map[string]any{"llmbrane.anthropic_strict": strict}}}
			out, err := (&Adapter{}).translateCanonicalRequest(req, "m", false, config.Endpoint{})
			if err != nil {
				t.Fatal(err)
			}
			if out.ParallelToolCalls == nil || *out.ParallelToolCalls == disabled || len(out.Tools) != 1 || out.Tools[0].Strict == nil || *out.Tools[0].Strict != strict {
				t.Fatalf("wrong flags: %+v", out)
			}
			body, err := json.Marshal(out)
			if err != nil || !strings.Contains(string(body), "9007199254740993") {
				t.Fatalf("precision: %s %v", body, err)
			}
		}
	}
	req := anthropicRequest()
	req.Tools = []canonical.CanonicalTool{{Type: "function", Name: "f"}}
	out, err := (&Adapter{}).translateCanonicalRequest(req, "m", false, config.Endpoint{})
	if err != nil || out.ParallelToolCalls != nil || len(out.Tools) != 1 || out.Tools[0].Strict == nil || *out.Tools[0].Strict {
		t.Fatalf("wrong defaults: %+v %v", out, err)
	}
	req.Params.ProviderSpecific["llmbrane.anthropic_disable_parallel_tool_use"] = "true"
	if _, err := (&Adapter{}).translateCanonicalRequest(req, "m", false, config.Endpoint{}); err == nil {
		t.Fatal("accepted invalid parallel setting")
	}
	delete(req.Params.ProviderSpecific, "llmbrane.anthropic_disable_parallel_tool_use")
	req.Tools[0].Metadata = map[string]any{"llmbrane.anthropic_strict": "true"}
	if _, err := (&Adapter{}).translateCanonicalRequest(req, "m", false, config.Endpoint{}); err == nil {
		t.Fatal("accepted invalid strict setting")
	}
}

func TestNonAnthropicToolDefaultsUnchanged(t *testing.T) {
	req := &canonical.CanonicalChatRequest{Tools: []canonical.CanonicalTool{{Type: "function", Name: "f"}}}
	out, err := (&Adapter{}).translateCanonicalRequest(req, "m", false, config.Endpoint{})
	if err != nil || len(out.Tools) != 1 || out.Tools[0].Strict != nil || out.ParallelToolCalls != nil {
		t.Fatalf("unexpected change: %+v %v", out, err)
	}
}
