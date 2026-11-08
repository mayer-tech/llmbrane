package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestAnthropicStreamTerminalAndCompatibility(t *testing.T) {
	created := sse(`{"type":"response.created","response":{"id":"r"}}`)
	added := sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m","role":"assistant"}}`)
	delta := sse(`{"type":"response.output_text.delta","output_index":0,"item_id":"m","content_index":0,"delta":"x"}`)
	done := sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"x"}]}}`)
	complete := sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"x"}]}]}}`)
	tests := []struct{ name, stream, last string }{
		{"minimal variants", created + added + delta + done + complete, "done"},
		{"missing terminal", created + added + delta + done, "error"},
		{"unterminated frame", created + added + delta + done + strings.TrimSuffix(complete, "\n\n"), "error"},
		{"wrong response id", created + added + delta + done + strings.Replace(complete, `"id":"r"`, `"id":"other"`, 1), "error"},
		{"conflicting output", created + added + delta + done + strings.Replace(complete, `"text":"x"`, `"text":"wrong"`, 1), "error"},
		{"missing delta", created + added + sse(`{"type":"response.output_text.delta","output_index":0,"item_id":"m"}`), "error"},
		{"refusal", created + added + sse(`{"type":"response.refusal.delta","output_index":0,"item_id":"m","delta":"no"}`), "error"},
		{"unknown additive event", created + sse(`{"type":"response.future_event"}`) + added + delta + done + complete, "done"},
		{"empty content done only", created + added + sse(`{"type":"response.content_part.done","output_index":0,"item_id":"m","content_index":0,"part":{"type":"output_text","text":""}}`) + sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":""}]}}`) + sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"message","id":"m"}]}}`), "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := streamEvents(t, tt.stream)
			if len(events) == 0 || events[len(events)-1].Type != tt.last {
				t.Fatalf("events: %+v", events)
			}
			for _, e := range events[:len(events)-1] {
				if e.Type == "done" || e.Type == "error" {
					t.Fatalf("premature terminal event: %+v", e)
				}
			}
		})
	}
}

func TestAnthropicStreamSummaryWithMissingDeltas(t *testing.T) {
	item := `{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}],"encrypted_content":"cipher"}`
	source := sse(`{"type":"response.created","response":{"id":"r"}}`) +
		sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs"}}`) +
		sse(`{"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"rs","summary_index":0}`) +
		sse(`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","summary_index":1,"delta":"second"}`) +
		sse(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`) +
		sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"reasoning","id":"rs"}]}}`)
	events := streamEvents(t, source)
	var thinking strings.Builder
	for _, event := range events {
		if event.Type == "error" {
			t.Fatal(event.Error)
		}
		if event.Delta != nil && event.Delta.Native != nil && event.Delta.Native.Format == "thinking_delta" {
			var d struct {
				Thinking string `json:"thinking"`
			}
			if err := json.Unmarshal(event.Delta.Native.Data, &d); err != nil {
				t.Fatal(err)
			}
			thinking.WriteString(d.Thinking)
		}
	}
	if thinking.String() != "first\nsecond" || events[len(events)-1].Type != "done" {
		t.Fatalf("thinking: %q, events: %+v", thinking.String(), events)
	}
}

func TestAnthropicNonStreamProviderErrors(t *testing.T) {
	cases := []struct {
		name, body string
		code       int
	}{
		{"invalid json", `{`, 502},
		{"missing output", `{"id":"r","status":"completed"}`, 502},
		{"missing id", `{"status":"completed","output":[]}`, 502},
		{"missing text", `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text"}]}]}`, 502},
		{"failed", `{"id":"r","status":"failed","output":[]}`, 502},
		{"incomplete max", `{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, 502},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tt.body) }))
			defer server.Close()
			_, err := NewAdapter().ProcessChat(context.Background(), anthropicRequest(), "m", config.Endpoint{URL: server.URL})
			if tt.code == 200 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			gw, ok := err.(*canonical.GatewayError)
			if !ok || gw.StatusCode != tt.code {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestResponsesAssistantMixedContentEncoding(t *testing.T) {
	req := anthropicRequest()
	req.Messages = []canonical.CanonicalMessage{{Role: canonical.RoleAssistant, Content: []canonical.CanonicalContentPart{
		{Type: "text", Text: ""}, {Type: "image", Image: &canonical.CanonicalImage{URL: "https://example.com/image.png"}},
	}}}
	out, err := (&Adapter{}).translateCanonicalRequest(req, "m", false, config.Endpoint{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"output_text","text":""`) {
		t.Fatalf("empty assistant text lost: %s", data)
	}
}

func TestAnthropicStreamDeferredToolNameAndPrefix(t *testing.T) {
	for _, delta := range []string{`{"x":1}`, `1}`} {
		source := sse(`{"type":"response.created","response":{"id":"r"}}`) +
			sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","arguments":"{\"x\":"}}`) +
			sse(fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"f","delta":%q}`, delta)) +
			sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"lookup","arguments":"{\"x\":1}"}}`) +
			sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","id":"f"}]}}`)
		events := streamEvents(t, source)
		var starts int
		var arguments string
		for _, event := range events {
			if event.Type == "error" {
				t.Fatal(event.Error)
			}
			if event.Type == "tool_call_start" {
				starts++
				if event.ToolCall.Name != "lookup" {
					t.Fatalf("name: %+v", event.ToolCall)
				}
			}
			if event.Type == "tool_call_delta" {
				arguments += event.ToolCall.ArgumentsDelta
			}
		}
		if starts != 1 || arguments != `{"x":1}` || events[len(events)-1].Type != "done" {
			t.Fatalf("events: %+v arguments: %q", events, arguments)
		}
	}
}

func TestAnthropicEncryptedReasoningCompatibilitySwitch(t *testing.T) {
	out, err := (&Adapter{}).translateCanonicalRequest(anthropicRequest(), "m", false, config.Endpoint{Extensions: map[string]any{"disable_encrypted_reasoning": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Include) != 0 {
		t.Fatalf("encrypted reasoning was not disabled: %+v", out.Include)
	}
}

func TestNativeChatToolResultWithoutCallIDUnchanged(t *testing.T) {
	req := &canonical.CanonicalChatRequest{Messages: []canonical.CanonicalMessage{{Role: canonical.RoleTool, Content: []canonical.CanonicalContentPart{{Type: "tool_result", ToolResult: &canonical.CanonicalToolResult{}}}}}}
	if _, err := (&Adapter{}).translateCanonicalRequest(req, "m", false, config.Endpoint{}); err != nil {
		t.Fatal(err)
	}
}
