package openairesponses

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestAnthropicEmptyResponsesOutputMapping(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"completed empty", `{"id":"r","status":"completed","output":[]}`, "empty Responses output"},
		{"incomplete empty", `{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, "empty Responses output"},
		{"empty message", `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[]}]}`, "empty Responses message content"},
		{"empty text", `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}]}`, "empty Responses text"},
		{"incomplete empty text", `{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":""}]}]}`, "empty Responses text"},
		{"empty among text", `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"},{"type":"output_text","text":""}]}]}`, "empty Responses text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			_, err := NewAdapter().ProcessChat(context.Background(), anthropicRequest(), "m", config.Endpoint{URL: server.URL})
			gw, ok := err.(*canonical.GatewayError)
			if !ok || gw.StatusCode != http.StatusBadGateway || !strings.Contains(gw.Message, tc.want) {
				t.Fatalf("error: %v", err)
			}
			if got, want := strings.Contains(gw.Message, "provider reached the output token limit"), tc.name == "incomplete empty"; got != want {
				t.Fatalf("unexpected token-limit detail: %v", err)
			}
		})
	}
	for _, status := range []string{`"status":"completed"`, `"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"id":"r",%s,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":" "}]}]}`, status)
		}))
		response, err := NewAdapter().ProcessChat(context.Background(), anthropicRequest(), "m", config.Endpoint{URL: server.URL})
		server.Close()
		if err != nil || len(response.Choices[0].Message.Content) != 1 || response.Choices[0].Message.Content[0].Text != " " {
			t.Fatalf("whitespace output: %+v %v", response, err)
		}
	}
}

func TestChatResponsesEmptyOutputUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"r","status":"completed","output":[]}`)
	}))
	defer server.Close()
	response, err := NewAdapter().ProcessChat(context.Background(), &canonical.CanonicalChatRequest{}, "m", config.Endpoint{URL: server.URL})
	if err != nil || response == nil || len(response.Choices) != 1 {
		t.Fatalf("response: %+v, error: %v", response, err)
	}
}

func TestAnthropicStreamEmptyResponsesOutputMapping(t *testing.T) {
	created := sse(`{"type":"response.created","response":{"id":"r"}}`)
	added := sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m","role":"assistant"}}`)
	emptyDone := sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":""}]}}`)
	for _, tc := range []struct{ name, source, want string }{
		{"completed empty", created + sse(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`), "empty Responses output"},
		{"incomplete empty", created + sse(`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`), "empty Responses output"},
		{"empty item", created + added + emptyDone, "empty Responses text"},
		{"empty message", created + added + sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m","role":"assistant","content":[]}}`), "empty Responses message content"},
		{"empty part stopped", created + added + sse(`{"type":"response.content_part.added","output_index":0,"item_id":"m","content_index":0,"part":{"type":"output_text","text":""}}`) + sse(`{"type":"response.content_part.done","output_index":0,"item_id":"m","content_index":0,"part":{"type":"output_text","text":""}}`) + emptyDone, "empty Responses text"},
		{"empty text done without part stop", created + added + sse(`{"type":"response.output_text.done","output_index":0,"item_id":"m","content_index":0,"text":""}`) + emptyDone, "empty Responses text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := streamEvents(t, tc.source)
			if len(events) == 0 || events[len(events)-1].Type != "error" {
				t.Fatalf("events: %+v", events)
			}
			last := events[len(events)-1].Error
			if last == nil || last.StatusCode != http.StatusBadGateway || !strings.Contains(last.Message, tc.want) {
				t.Fatalf("error: %+v", last)
			}
			for _, e := range events {
				if e.Type == "done" || e.Type == "content_stop" {
					t.Fatalf("unreplayable block completed: %+v", events)
				}
			}
		})
	}
}

func TestAnthropicStreamWhitespaceAndIncomplete(t *testing.T) {
	created := sse(`{"type":"response.created","response":{"id":"r"}}`)
	added := sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m","role":"assistant"}}`)
	part := sse(`{"type":"response.content_part.added","output_index":0,"item_id":"m","content_index":0,"part":{"type":"output_text","text":""}}`) +
		sse(`{"type":"response.output_text.delta","output_index":0,"item_id":"m","content_index":0,"delta":" "}`) +
		sse(`{"type":"response.content_part.done","output_index":0,"item_id":"m","content_index":0,"part":{"type":"output_text","text":" "}}`)
	for _, tc := range []struct{ status, event, reason, itemStatus, details string }{
		{"completed", "response.completed", "stop", "completed", ""},
		{"incomplete", "response.incomplete", "length", "incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`},
	} {
		t.Run(tc.status, func(t *testing.T) {
			item := fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m","role":"assistant","status":%q,"content":[{"type":"output_text","text":" "}]}}`, tc.itemStatus)
			terminal := fmt.Sprintf(`{"type":%q,"response":{"id":"r","status":%q%s,"output":[{"type":"message","id":"m"}]}}`, tc.event, tc.status, tc.details)
			events := streamEvents(t, created+added+part+sse(item)+sse(terminal))
			last := events[len(events)-1]
			if last.Type != "done" || last.Stop == nil || last.Stop.Reason != tc.reason {
				t.Fatalf("events: %+v", events)
			}
			var text string
			var stops int
			for _, event := range events {
				if event.Type == "content_delta" && event.Delta != nil {
					text += event.Delta.Text
				}
				if event.Type == "content_stop" {
					stops++
				}
			}
			if text != " " || stops != 1 {
				t.Fatalf("text %q, stops %d", text, stops)
			}
		})
	}
}
