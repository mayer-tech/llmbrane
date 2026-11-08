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

func anthropicRequest() *canonical.CanonicalChatRequest {
	return &canonical.CanonicalChatRequest{Params: canonical.CanonicalParams{ProviderSpecific: map[string]any{"llmbrane.anthropic_inbound": true, "llmbrane.anthropic_adaptive": true}}}
}

func TestAnthropicResponsesOrderedReplay(t *testing.T) {
	req := anthropicRequest()
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","status":"completed","summary":[],"encrypted_content":"cipher","extra":{"number":9007199254740993}}`)
	req.Messages = []canonical.CanonicalMessage{{Role: canonical.RoleAssistant, Content: []canonical.CanonicalContentPart{
		{Type: "text", Text: "before"},
		{Type: "reasoning", Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.item", Data: raw}},
		{Type: "tool_call", ToolCall: &canonical.CanonicalToolCall{ID: "call_a", Name: "f", Arguments: json.RawMessage(`{}`)}},
		{Type: "text", Text: "after"},
	}}, {Role: canonical.RoleUser, Content: []canonical.CanonicalContentPart{
		{Type: "tool_result", ToolResult: &canonical.CanonicalToolResult{ToolCallID: "call_b", Content: []canonical.CanonicalContentPart{{Type: "text", Text: "B"}}}},
		{Type: "tool_result", ToolResult: &canonical.CanonicalToolResult{ToolCallID: "call_a", Content: []canonical.CanonicalContentPart{{Type: "text", Text: "A"}}}},
	}}}
	out, err := (&Adapter{}).translateCanonicalRequest(req, "model", true, config.Endpoint{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input     []map[string]json.RawMessage `json:"input"`
		Include   []string                     `json:"include"`
		Reasoning *Reasoning                   `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 6 || len(got.Include) != 1 || got.Include[0] != "reasoning.encrypted_content" || got.Reasoning == nil || got.Reasoning.Summary != "auto" {
		t.Fatalf("wrong request: %s", body)
	}
	for i, kind := range []string{"message", "reasoning", "function_call", "message", "function_call_output", "function_call_output"} {
		var actual string
		_ = json.Unmarshal(got.Input[i]["type"], &actual)
		if actual != kind {
			t.Fatalf("item %d: %s", i, body)
		}
	}
	if _, ok := got.Input[1]["status"]; ok {
		t.Fatalf("output status leaked: %s", body)
	}
	if string(got.Input[1]["extra"]) != `{"number":9007199254740993}` || string(got.Input[1]["encrypted_content"]) != `"cipher"` || string(got.Input[4]["call_id"]) != `"call_b"` {
		t.Fatalf("replay changed: %s", body)
	}
	for _, native := range []*canonical.CanonicalProviderData{nil, {Provider: "anthropic", Format: "thinking", Data: json.RawMessage(`{}`)}, {Provider: "openai", Format: "responses.item", Data: json.RawMessage(`{"type":"reasoning"}`)}} {
		req.Messages = []canonical.CanonicalMessage{{Role: canonical.RoleAssistant, Content: []canonical.CanonicalContentPart{{Type: "reasoning", Native: native}}}}
		if _, err := (&Adapter{}).translateCanonicalRequest(req, "model", false, config.Endpoint{}); err == nil {
			t.Fatalf("accepted incompatible reasoning: %+v", native)
		}
	}
	emptyOutput, err := json.Marshal(InputItem{Type: "function_call_output", CallID: "call_empty"})
	if err != nil || !strings.Contains(string(emptyOutput), `"output":""`) {
		t.Fatalf("empty output lost: %s %v", emptyOutput, err)
	}
}

func streamEvents(t *testing.T, source string) []*canonical.CanonicalChatStreamEvent {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, source)
	}))
	defer server.Close()
	ch, err := NewAdapter().ProcessChatStream(context.Background(), anthropicRequest(), "model", config.Endpoint{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	var events []*canonical.CanonicalChatStreamEvent
	for e := range ch {
		events = append(events, e)
	}
	return events
}

func sse(body string) string { return "data: " + body + "\n\n" }

func TestAnthropicStreamParallelToolsAndReasoning(t *testing.T) {
	item := `{"type":"reasoning","id":"rs","status":"completed","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}],"encrypted_content":"cipher","extra":1}`
	source := sse(`{"type":"response.created","response":{"id":"resp","status":"in_progress"}}`) +
		sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs"}}`) +
		sse(`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","summary_index":0,"delta":"first"}`) +
		sse(`{"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"rs","summary_index":1}`) +
		sse(`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","summary_index":1,"delta":"second"}`) +
		sse(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`) +
		sse(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f"}}`) +
		sse(`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"g"}}`) +
		sse(`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_b","delta":"{\"b\":"}`) +
		sse(`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_a","delta":"{}"}`) +
		sse(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{}"}}`) +
		sse(`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_b","delta":"2}"}`) +
		sse(`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"g","arguments":"{\"b\":2}"}}`) +
		sse(`{"type":"response.completed","response":{"id":"resp","status":"completed","output":[{"type":"reasoning","id":"rs"},{"type":"function_call","id":"fc_a"},{"type":"function_call","id":"fc_b"}]}}`)
	events := streamEvents(t, source)
	active := -1
	var starts []string
	var thinking, argsA, argsB string
	var envelope string
	for _, e := range events {
		switch e.Type {
		case "error":
			t.Fatalf("unexpected error: %v", e.Error)
		case "content_start", "tool_call_start":
			if active != -1 {
				t.Fatal("overlapping blocks")
			}
			active = e.ContentIndex
			if e.ToolCall != nil {
				starts = append(starts, e.ToolCall.ID)
			}
		case "content_delta":
			if e.ContentIndex != active {
				t.Fatal("wrong delta index")
			}
			var d map[string]string
			_ = json.Unmarshal(e.Delta.Native.Data, &d)
			thinking += d["thinking"]
			envelope += d["signature"]
		case "tool_call_delta":
			if e.ContentIndex != active {
				t.Fatal("wrong tool index")
			}
			if active == 1 {
				argsA += e.ToolCall.ArgumentsDelta
			} else if active == 2 {
				argsB += e.ToolCall.ArgumentsDelta
			}
		case "content_stop":
			if e.ContentIndex != active {
				t.Fatal("wrong stop")
			}
			active = -1
		}
	}
	if strings.Join(starts, ",") != "call_a,call_b" || thinking != "first\nsecond" || argsA != `{}` || argsB != `{"b":2}` || active != -1 {
		t.Fatalf("bad stream: starts %v thinking %q args %q %q", starts, thinking, argsA, argsB)
	}
	decoded, err := canonical.DecodeReasoningEnvelope(envelope)
	if err != nil || string(decoded.Data) != item {
		t.Fatalf("wrong envelope: %s %v", envelope, err)
	}
	last := events[len(events)-1]
	if last.Type != "done" || last.Stop.Reason != "tool_calls" {
		t.Fatalf("wrong ending: %+v", last)
	}
}

func TestAnthropicStreamInterruptions(t *testing.T) {
	created := sse(`{"type":"response.created","response":{"id":"r"}}`)
	start := sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs"}}`)
	partial := sse(`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","delta":"partial"}`)
	cases := []string{
		created + start + partial,
		created + start + partial + sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs","summary":[]}}`),
		created + start + partial + sse(`{"type":"response.failed","response":{"error":{"message":"failed"}}}`),
		created + start + partial + sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`),
		created + start + `data: {"type":"response.completed"}`,
	}
	for _, source := range cases {
		events := streamEvents(t, source)
		if len(events) == 0 || events[len(events)-1].Type != "error" {
			t.Fatalf("missing error: %+v", events)
		}
		for _, e := range events {
			if e.Type == "done" {
				t.Fatal("interrupted stream completed")
			}
			if e.Delta != nil && e.Delta.Native != nil && e.Delta.Native.Format == "signature_delta" {
				t.Fatal("partial reasoning signed")
			}
		}
	}
}

func TestAnthropicStreamCompletedReasoningBeforeInterruption(t *testing.T) {
	item := `{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"cipher"}`
	source := sse(`{"type":"response.created","response":{"id":"r"}}`) +
		sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs"}}`) +
		sse(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`) +
		sse(`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg"}}`) +
		sse(`{"type":"response.output_text.delta","output_index":1,"item_id":"msg","delta":"partial"}`)
	events := streamEvents(t, source)
	if len(events) == 0 || events[len(events)-1].Type != "error" {
		t.Fatalf("missing error: %+v", events)
	}
	var signature string
	for _, event := range events {
		if event.Type == "done" {
			t.Fatal("interrupted turn completed")
		}
		if event.Delta != nil && event.Delta.Native != nil && event.Delta.Native.Format == "signature_delta" {
			var d struct {
				Signature string `json:"signature"`
			}
			if err := json.Unmarshal(event.Delta.Native.Data, &d); err != nil {
				t.Fatal(err)
			}
			signature = d.Signature
		}
	}
	data, err := canonical.DecodeReasoningEnvelope(signature)
	if err != nil || string(data.Data) != item {
		t.Fatalf("completed artifact was lost: %v", err)
	}
}
