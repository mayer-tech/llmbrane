package anthropic

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func streamEvent(data string) string { return "data: " + data + "\n\n" }

func collectAnthropicStream(t *testing.T, body string) []*canonical.CanonicalChatStreamEvent {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
	defer server.Close()
	ch, err := NewAdapter().ProcessChatStream(context.Background(), &canonical.CanonicalChatRequest{}, "model", config.Endpoint{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	var events []*canonical.CanonicalChatStreamEvent
	for event := range ch {
		events = append(events, event)
	}
	return events
}

func TestStreamCompletesOnlyAtMessageStop(t *testing.T) {
	message := streamEvent(`{"type":"message_start","message":{"id":"id"}}`)
	start := streamEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","extra":42}}`)
	delta := streamEvent(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig","extra":true}}`)
	closeBlock := streamEvent(`{"type":"content_block_stop","index":0}`)
	stopReason := streamEvent(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":17}}`)
	end := streamEvent(`{"type":"message_stop"}`)
	for _, tt := range []struct {
		name, body string
		wantError  bool
	}{
		{"normal", start + delta + closeBlock + stopReason + end, false},
		{"EOF after delta", start + delta + closeBlock + stopReason, true},
		{"DONE after delta", start + delta + closeBlock + stopReason + "data: [DONE]\n\n", true},
		{"open at stop", start + delta + stopReason + end, true},
		{"malformed after delta", stopReason + "data: {bad}\n\n" + end, true},
		{"EOF after block", start + delta + closeBlock, true},
		{"empty", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := collectAnthropicStream(t, message+tt.body)
			var dones, errors int
			for _, e := range events {
				if e.Type == "done" {
					dones++
					if e.Stop == nil || e.Stop.Reason != "tool_calls" || e.Usage == nil || e.Usage.OutputTokens != 17 {
						t.Fatalf("done = %+v", e)
					}
				}
				if e.Type == "error" {
					errors++
				}
			}
			if tt.wantError && (dones != 0 || errors != 1) || !tt.wantError && (dones != 1 || errors != 0) {
				t.Fatalf("events = %+v", events)
			}
			if !tt.wantError {
				if len(events) != 5 || string(events[1].Delta.Native.Data) != `{"type":"thinking","thinking":"","extra":42}` || string(events[2].Delta.Native.Data) != `{"type":"signature_delta","signature":"sig","extra":true}` {
					t.Fatalf("native events = %+v", events)
				}
			}
		})
	}
}

func TestStreamRejectsMalformedEvents(t *testing.T) {
	message := streamEvent(`{"type":"message_start","message":{"id":"id"}}`)
	start := streamEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	end := streamEvent(`{"type":"message_stop"}`)
	for _, body := range []string{
		"event: message_stop\ndata: {\"type\":\"message_delta\"}\n\n",
		streamEvent(`{"type":"message_start"}`),
		streamEvent(`{"type":"content_block_start","content_block":{"type":"text"}}`),
		start + start, start + streamEvent(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"a"}}`),
		start + streamEvent(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta"}}`),
		start + streamEvent(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"x"}}`),
		streamEvent(`{"type":"content_block_stop","index":0}`),
		streamEvent(`{"type":"message_delta"}`),
	} {
		events := collectAnthropicStream(t, message+body+end)
		if len(events) == 0 || events[len(events)-1].Type != "error" {
			t.Fatalf("%q: events = %+v", body, events)
		}
		for _, e := range events {
			if e.Type == "done" {
				t.Fatalf("unexpected done: %q", body)
			}
		}
	}
}

func TestStreamSSEFramingAndStopUsage(t *testing.T) {
	body := ": keepalive\n\nevent: ping\ndata: {\"type\":\"ping\"}\n\n" +
		streamEvent(`{"type":"message_start","message":{"id":"id"}}`) +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\ndata: \"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":8}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	events := collectAnthropicStream(t, body)
	if len(events) != 2 || events[1].Type != "done" || events[1].Stop.Reason != "length" || events[1].Usage.OutputTokens != 8 {
		t.Fatalf("events = %+v", events)
	}
}

func TestStreamCancellationUnblocksSendAndRead(t *testing.T) {
	for _, pendingSend := range []bool{false, true} {
		t.Run(fmt.Sprint(pendingSend), func(t *testing.T) {
			ready := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if pendingSend {
					fmt.Fprint(w, streamEvent(`{"type":"message_start","message":{"id":"id"}}`))
				} else {
					fmt.Fprint(w, ": ping\n\n")
				}
				w.(http.Flusher).Flush()
				close(ready)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			ch, err := NewAdapter().ProcessChatStream(ctx, &canonical.CanonicalChatRequest{}, "model", config.Endpoint{URL: server.URL})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			<-ready
			cancel()
			select {
			case _, ok := <-ch:
				if ok {
					for range ch {
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not stop on cancellation")
			}
		})
	}
}

func TestStreamRejectsOversizedEvent(t *testing.T) {
	events := collectAnthropicStream(t, "data: "+strings.Repeat("x", (4<<20)+1)+"\n\n")
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("events = %+v", events)
	}
}
