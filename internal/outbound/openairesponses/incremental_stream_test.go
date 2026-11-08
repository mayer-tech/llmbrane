package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

// Each gate keeps output_item.done off the wire until the test has observed
// the preceding deltas. This also tests serialization of interleaved items.
func TestAnthropicStreamLiveIncrementalItems(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reasoningDone := make(chan struct{})
	firstToolDone := make(chan struct{})
	secondToolDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher)
		write := func(events ...string) {
			for _, event := range events {
				_, _ = fmt.Fprint(w, sse(event))
			}
			flush.Flush()
		}
		wait := func(gate <-chan struct{}) bool {
			select {
			case <-gate:
				return true
			case <-r.Context().Done():
				return false
			}
		}
		write(
			`{"type":"response.created","response":{"id":"r"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs"}}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","summary_index":0,"delta":"First "}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","summary_index":0,"delta":"step."}`,
			`{"type":"response.reasoning_summary_part.added","output_index":0,"item_id":"rs","summary_index":1}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","summary_index":1,"delta":"Second"}`,
			`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"b","call_id":"call_b","name":"g"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"b","delta":"{\"b\":"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"a","call_id":"call_a","name":"f"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"a","delta":"{\"a\":"}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","summary_index":1,"delta":"!"}`,
		)
		if !wait(reasoningDone) {
			return
		}
		write(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"First step."},{"type":"summary_text","text":"Second!"}],"encrypted_content":"cipher"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"a","delta":"1}"}`)
		if !wait(firstToolDone) {
			return
		}
		write(`{"type":"response.function_call_arguments.done","output_index":1,"item_id":"a","arguments":"{\"a\":1}"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"a","call_id":"call_a","name":"f","arguments":"{\"a\":1}"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"b","delta":"2}"}`)
		if !wait(secondToolDone) {
			return
		}
		write(`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"b","call_id":"call_b","name":"g","arguments":"{\"b\":2}"}}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"reasoning","id":"rs"},{"type":"function_call","id":"a"},{"type":"function_call","id":"b"}]}}`)
	}))
	defer server.Close()
	defer cancel() // Unblock the handler before server.Close on a test failure.

	ch, err := NewAdapter().ProcessChatStream(ctx, anthropicRequest(), "model", config.Endpoint{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	next := func(kind string, index int) *canonical.CanonicalChatStreamEvent {
		t.Helper()
		select {
		case event, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before %s", kind)
			}
			if event.Type != kind || (index >= 0 && event.ContentIndex != index) {
				t.Fatalf("expected %s[%d], got %+v", kind, index, event)
			}
			return event
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s[%d] before output_item.done", kind, index)
			return nil
		}
	}
	thinking := func(want string) {
		t.Helper()
		e := next("content_delta", 0)
		if e.Delta == nil || e.Delta.Native == nil || e.Delta.Native.Format != "thinking_delta" {
			t.Fatalf("not a thinking delta: %+v", e)
		}
		var d struct {
			Thinking string `json:"thinking"`
		}
		if err := json.Unmarshal(e.Delta.Native.Data, &d); err != nil || d.Thinking != want {
			t.Fatalf("thinking delta %q, want %q: %v", d.Thinking, want, err)
		}
	}
	toolDelta := func(index int, want string) {
		t.Helper()
		e := next("tool_call_delta", index)
		if e.ToolCall == nil || e.ToolCall.Index != index || e.ToolCall.ArgumentsDelta != want {
			t.Fatalf("tool delta: %+v, want %q", e, want)
		}
	}
	next("message_start", -1)
	next("content_start", 0)
	thinking("First ")
	thinking("step.")
	thinking("\n")
	thinking("Second")
	thinking("!")
	close(reasoningDone)
	signature := next("content_delta", 0)
	if signature.Delta == nil || signature.Delta.Native == nil || signature.Delta.Native.Format != "signature_delta" {
		t.Fatalf("missing signature: %+v", signature)
	}
	var d struct {
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(signature.Delta.Native.Data, &d); err != nil {
		t.Fatal(err)
	}
	artifact, err := canonical.DecodeReasoningEnvelope(d.Signature)
	if err != nil || !strings.Contains(string(artifact.Data), `"encrypted_content":"cipher"`) {
		t.Fatalf("invalid signature envelope: %v", err)
	}
	next("content_stop", 0)
	startA := next("tool_call_start", 1)
	if startA.ToolCall == nil || startA.ToolCall.ID != "call_a" {
		t.Fatalf("wrong first tool: %+v", startA)
	}
	toolDelta(1, `{"a":`)
	toolDelta(1, `1}`)
	close(firstToolDone)
	next("content_stop", 1)
	startB := next("tool_call_start", 2)
	if startB.ToolCall == nil || startB.ToolCall.ID != "call_b" {
		t.Fatalf("wrong second tool: %+v", startB)
	}
	toolDelta(2, `{"b":`)
	toolDelta(2, `2}`)
	close(secondToolDone)
	next("content_stop", 2)
	end := next("done", -1)
	if end.Stop == nil || end.Stop.Reason != "tool_calls" {
		t.Fatalf("wrong stop: %+v", end)
	}
	if event, ok := <-ch; ok {
		t.Fatalf("event after completion: %+v", event)
	}
}

func TestAnthropicIncrementalCompletionValidation(t *testing.T) {
	created := sse(`{"type":"response.created","response":{"id":"r"}}`)
	reasoning := created + sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs"}}`) + sse(`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs","delta":"visible"}`)
	toolStart := created + sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"lookup"}}`)
	tool := toolStart + sse(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"f","delta":"{\"x\":1}"}`)
	reasoningDone := sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"visible"}],"encrypted_content":"cipher"}}`)
	toolDone := sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"lookup","arguments":"{\"x\":1}"}}`)
	reasoningTerminal := sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"reasoning","id":"rs"}]}}`)
	toolTerminal := sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","id":"f"}]}}`)

	tests := []struct {
		name, source, control, wantError, stopReason string
	}{
		{
			name:       "reasoning mismatch",
			source:     reasoning + sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"different"}],"encrypted_content":"cipher"}}`) + reasoningTerminal,
			control:    reasoning + reasoningDone + reasoningTerminal,
			wantError:  "reasoning summary does not match deltas",
			stopReason: "stop",
		},
		{
			name:       "arguments done mismatch",
			source:     tool + sse(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"f","arguments":"{\"x\":2}"}`) + toolDone + toolTerminal,
			control:    tool + sse(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"f","arguments":"{\"x\":1}"}`) + toolDone + toolTerminal,
			wantError:  "arguments completion does not match deltas",
			stopReason: "tool_calls",
		},
		{
			name:       "arguments item mismatch",
			source:     tool + sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"lookup","arguments":"{\"x\":2}"}}`) + toolTerminal,
			control:    tool + toolDone + toolTerminal,
			wantError:  "function call arguments do not match deltas",
			stopReason: "tool_calls",
		},
		{
			name:       "invalid arguments",
			source:     toolStart + sse(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"f","delta":"not json"}`) + sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"f","call_id":"c","name":"lookup","arguments":"not json"}}`) + toolTerminal,
			control:    tool + toolDone + toolTerminal,
			wantError:  "invalid completed function call",
			stopReason: "tool_calls",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("invalid", func(t *testing.T) {
				events := streamEvents(t, tt.source)
				if len(events) < 3 || events[len(events)-1].Type != "error" {
					t.Fatalf("missing error: %+v", events)
				}
				got := events[len(events)-1].Error
				if got == nil || got.Message != tt.wantError || got.StatusCode != http.StatusBadGateway || got.ErrorKind != canonical.ErrorKindProviderError {
					t.Fatalf("error = %+v, want provider error %q", got, tt.wantError)
				}
				for _, e := range events {
					if e.Type == "done" || (e.Delta != nil && e.Delta.Native != nil && e.Delta.Native.Format == "signature_delta") {
						t.Fatalf("invalid completion: %+v", e)
					}
				}
			})
			t.Run("valid", func(t *testing.T) {
				events := streamEvents(t, tt.control)
				if len(events) == 0 || events[len(events)-1].Type != "done" || events[len(events)-1].Stop == nil || events[len(events)-1].Stop.Reason != tt.stopReason {
					t.Fatalf("missing successful completion: %+v", events)
				}
				var thinking, arguments strings.Builder
				var signature string
				for _, e := range events[:len(events)-1] {
					if e.Type == "error" || e.Type == "done" {
						t.Fatalf("premature terminal event: %+v", e)
					}
					if e.Type == "tool_call_delta" {
						if e.ToolCall == nil {
							t.Fatal("missing tool call delta")
						}
						arguments.WriteString(e.ToolCall.ArgumentsDelta)
					}
					if e.Delta != nil && e.Delta.Native != nil {
						var d struct {
							Thinking  string `json:"thinking"`
							Signature string `json:"signature"`
						}
						if err := json.Unmarshal(e.Delta.Native.Data, &d); err != nil {
							t.Fatal(err)
						}
						thinking.WriteString(d.Thinking)
						signature += d.Signature
					}
				}
				if tt.stopReason == "stop" {
					artifact, err := canonical.DecodeReasoningEnvelope(signature)
					if thinking.String() != "visible" || err != nil || !strings.Contains(string(artifact.Data), `"encrypted_content":"cipher"`) {
						t.Fatalf("invalid reasoning control: thinking=%q signature=%q error=%v", thinking.String(), signature, err)
					}
				} else if arguments.String() != `{"x":1}` {
					t.Fatalf("arguments = %q, want %q", arguments.String(), `{"x":1}`)
				}
			})
		})
	}
}
