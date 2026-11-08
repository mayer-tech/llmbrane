package openairesponses

import (
	"fmt"
	"strings"
	"testing"
)

func TestAnthropicStreamToolPrefixReconciliation(t *testing.T) {
	tests := []struct {
		name, initial string
		fragments     []string
		arguments     string
		done          bool
	}{
		{"repeated prefix", `{"x":`, []string{`{"`, `x":`, `1}`}, `{"x":1}`, false},
		{"suffix only", `{"x":`, []string{`1}`}, `{"x":1}`, true},
		{"ambiguous suffix", `{"a":`, []string{`{"a":`, `1}}`}, `{"a":{"a":1}}`, false},
		{"unicode boundary", `{"ä":`, []string{`{"ä":{"å":1}}`}, `{"ä":{"å":1}}`, false},
		{"no deltas", `{"x":`, nil, `{"x":1}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := sse(`{"type":"response.created","response":{"id":"r"}}`) +
				sse(fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"lookup","arguments":%q}}`, tt.initial))
			for _, fragment := range tt.fragments {
				source += sse(fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"f","delta":%q}`, fragment))
			}
			if tt.done {
				source += sse(fmt.Sprintf(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"f","arguments":%q}`, tt.arguments))
			}
			source += sse(fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"lookup","arguments":%q}}`, tt.arguments)) +
				sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","id":"f"}]}}`)
			var args strings.Builder
			for _, event := range streamEvents(t, source) {
				if event.Type == "error" {
					t.Fatal(event.Error)
				}
				if event.Type == "tool_call_delta" {
					args.WriteString(event.ToolCall.ArgumentsDelta)
				}
			}
			if args.String() != tt.arguments {
				t.Fatalf("arguments %q, want %q", args.String(), tt.arguments)
			}
		})
	}
}
