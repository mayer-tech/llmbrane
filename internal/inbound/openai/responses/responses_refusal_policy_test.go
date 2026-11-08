package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestResponsesAnthropicRefusalPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []canonical.CanonicalContentPart
	}{
		{"empty", nil},
		{"whitespace", []canonical.CanonicalContentPart{{Type: "text", Text: " "}}},
		{"multiple text blocks", []canonical.CanonicalContentPart{{Type: "text", Text: "a"}, {Type: "text", Text: "b"}}},
		{"reasoning and text", []canonical.CanonicalContentPart{{Type: "reasoning"}, {Type: "text", Text: "no"}}},
		{"tool and text", []canonical.CanonicalContentPart{{Type: "tool_call"}, {Type: "text", Text: "no"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &canonical.CanonicalChatResponse{ID: "m", Stop: &canonical.CanonicalStop{Reason: "refusal"}, Choices: []canonical.CanonicalChoice{{Message: canonical.CanonicalMessage{Content: tc.parts}}}}
			if _, err := responsesFromCanonical(response, "m"); err == nil {
				t.Fatal("ambiguous refusal accepted")
			}
		})
	}

	for _, reason := range []string{"future_reason", "pause_turn"} {
		if _, _, err := anthropicResponsesStatus(reason); err == nil {
			t.Fatalf("unsupported stop reason %q accepted", reason)
		}
	}
}

func TestResponsesAnthropicRefusalStreamPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []*canonical.CanonicalChatStreamEvent
	}{
		{"multiple blocks", []*canonical.CanonicalChatStreamEvent{
			{Type: "content_start", ContentIndex: 0, Delta: nativePart("text", `{"type":"text","text":"a"}`)},
			{Type: "content_stop", ContentIndex: 0},
			{Type: "content_start", ContentIndex: 1, Delta: nativePart("text", `{"type":"text","text":"b"}`)},
			{Type: "content_stop", ContentIndex: 1},
		}},
		{"thinking and text", []*canonical.CanonicalChatStreamEvent{
			{Type: "content_start", ContentIndex: 0, Delta: nativePart("redacted_thinking", `{"type":"redacted_thinking","data":"hidden"}`)},
			{Type: "content_stop", ContentIndex: 0},
			{Type: "content_start", ContentIndex: 1, Delta: nativePart("text", `{"type":"text","text":"no"}`)},
			{Type: "content_stop", ContentIndex: 1},
		}},
		{"whitespace", []*canonical.CanonicalChatStreamEvent{
			{Type: "content_start", ContentIndex: 0, Delta: nativePart("text", `{"type":"text","text":" "}`)},
			{Type: "content_stop", ContentIndex: 0},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []*canonical.CanonicalChatStreamEvent{{Type: "message_start", ResponseID: "m"}}
			events = append(events, tc.events...)
			events = append(events, &canonical.CanonicalChatStreamEvent{Type: "done", Stop: &canonical.CanonicalStop{Reason: "refusal"}})
			mapped := mappedEvents(t, events)
			if len(mapped) != 2 || mapped[1]["type"] != "error" {
				t.Fatalf("ambiguous output was delivered: %#v", mapped)
			}
		})
	}

	events := []*canonical.CanonicalChatStreamEvent{
		{Type: "message_start", ResponseID: "m"},
		{Type: "content_start", ContentIndex: 0, Delta: nativePart("text", `{"type":"text","text":"hello"}`)},
		{Type: "content_stop", ContentIndex: 0},
		{Type: "done", Stop: &canonical.CanonicalStop{Reason: "future_reason"}},
	}
	mapped := mappedEvents(t, events)
	if len(mapped) != 2 || mapped[1]["type"] != "error" {
		t.Fatalf("unsupported stop produced output: %#v", mapped)
	}
}

func TestResponsesAnthropicPendingEventsBoundAndDelivery(t *testing.T) {
	w := httptest.NewRecorder()
	s := newResponsesStream(w, w, "m")
	if _, err := s.accept(&canonical.CanonicalChatStreamEvent{Type: "message_start", ResponseID: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.accept(&canonical.CanonicalChatStreamEvent{Type: "content_start", ContentIndex: 0, Delta: nativePart("text", `{"type":"text","text":"hello"}`)}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), "response.output_item.added") {
		t.Fatal("output delivered before stop reason")
	}
	s.pendingBytes = maxResponsesStreamBytes
	if err := s.emit("response.output_text.delta", map[string]any{"delta": "x"}); err == nil {
		t.Fatal("pending event limit not enforced")
	}
	s.fail(fmt.Errorf("pending event limit"))
	lines := strings.Split(w.Body.String(), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			if event["type"] == "error" && event["sequence_number"] != float64(1) {
				t.Fatalf("error sequence: %#v", event)
			}
		}
	}
}

func TestResponsesAnthropicRefusalReplayEndToEnd(t *testing.T) {
	requests := make(chan map[string]any, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		_, _ = fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"no"}],"stop_reason":"refusal"}`)
	}))
	defer backend.Close()
	handler := anthropicResponsesHandler(backend.URL)
	first := postResponses(handler, map[string]any{"model": "p/m", "input": "question"})
	if first.Code != http.StatusOK {
		t.Fatalf("initial response: %d %s", first.Code, first.Body)
	}
	<-requests
	item := decodeObject(t, first.Body.Bytes())["output"].([]any)[0]
	input := []any{map[string]any{"role": "user", "content": "question"}, item, map[string]any{"role": "user", "content": "follow-up"}}
	second := postResponses(handler, map[string]any{"model": "p/m", "input": input})
	if second.Code != http.StatusOK {
		t.Fatalf("replay response: %d %s", second.Code, second.Body)
	}
	messages := (<-requests)["messages"].([]any)
	assistant := messages[1].(map[string]any)
	blocks := assistant["content"].([]any)
	block := blocks[0].(map[string]any)
	if assistant["role"] != "assistant" || block["type"] != "text" || block["text"] != "no" || messages[2].(map[string]any)["role"] != "user" {
		t.Fatalf("replay: %#v", messages)
	}
}
