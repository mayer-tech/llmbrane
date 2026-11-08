package anthropic

import (
	"net/http"
	"strings"
	"testing"
)

func TestStreamLifecycleAndTruncation(t *testing.T) {
	start := streamEvent(`{"type":"message_start","message":{"id":"m"}}`)
	block := streamEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	closeBlock := streamEvent(`{"type":"content_block_stop","index":0}`)
	stop := streamEvent(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
	end := streamEvent(`{"type":"message_stop"}`)
	cases := map[string]string{
		"no start":                  stop + end,
		"duplicate start":           start + start + stop + end,
		"no stop reason":            start + end,
		"duplicate stop reason":     start + stop + stop + end,
		"invalid tool start":        start + streamEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","input":{}}}`),
		"stop with open block":      start + block + stop + closeBlock + end,
		"content after stop":        start + stop + block + closeBlock + end,
		"unterminated message stop": start + stop + `data: {"type":"message_stop"}`,
		"unterminated block":        start + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			events := collectAnthropicStream(t, body)
			if len(events) == 0 || events[len(events)-1].Type != "error" {
				t.Fatalf("missing error: %+v", events)
			}
			for _, event := range events {
				if event.Type == "done" {
					t.Fatal("incomplete stream completed")
				}
			}
		})
	}
}

func TestStreamIndicesAndNativeMetadata(t *testing.T) {
	start := `{"type":"message_start","message":{"id":"m","extra":9,"usage":{"input_tokens":12,"output_tokens":0}}}`
	textDelta := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi","extra":9007199254740993}}`
	toolDelta := `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{}","extra":true}}`
	stopDelta := `{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"END","extra":42},"usage":{"output_tokens":7}}`
	body := streamEvent(start) +
		streamEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		streamEvent(textDelta) + streamEvent(`{"type":"content_block_stop","index":0}`) +
		streamEvent(`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"a","name":"f","input":{}}}`) +
		streamEvent(toolDelta) + streamEvent(`{"type":"content_block_stop","index":3}`) +
		streamEvent(`{"type":"content_block_start","index":5,"content_block":{"type":"tool_use","id":"b","name":"g","input":{}}}`) +
		streamEvent(`{"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"{}"}}`) +
		streamEvent(`{"type":"content_block_stop","index":5}`) +
		streamEvent(stopDelta) + streamEvent(`{"type":"message_stop"}`)
	events := collectAnthropicStream(t, body)
	if len(events) != 11 {
		t.Fatalf("unexpected events: %+v", events)
	}
	if string(events[0].Raw) != start {
		t.Fatalf("message raw: %s", events[0].Raw)
	}
	if string(events[2].Raw) != textDelta || string(events[2].Delta.Native.Data) != `{"type":"text_delta","text":"hi","extra":9007199254740993}` {
		t.Fatal("text delta metadata lost")
	}
	for _, check := range []struct{ event, content, tool int }{{4, 3, 0}, {5, 3, 0}, {7, 5, 1}, {8, 5, 1}} {
		e := events[check.event]
		if e.ContentIndex != check.content || e.ToolCall == nil || e.ToolCall.Index != check.tool {
			t.Fatalf("wrong index: %+v", e)
		}
	}
	if string(events[5].Raw) != toolDelta || string(events[5].Delta.Native.Data) != `{"type":"input_json_delta","partial_json":"{}","extra":true}` {
		t.Fatal("tool delta metadata lost")
	}
	done := events[10]
	if done.Type != "done" || done.Stop == nil || done.Stop.Reason != "stop_sequence" || done.Stop.Native == nil || string(done.Stop.Native.Data) != stopDelta || done.Usage == nil || done.Usage.InputTokens != 12 || done.Usage.OutputTokens != 7 || done.Usage.TotalTokens != 19 {
		t.Fatalf("stop metadata lost: %+v", done)
	}
	if string(done.Raw) != `{"type":"message_stop"}` {
		t.Fatalf("stop raw = %s", done.Raw)
	}
}

func TestStreamProviderErrorMapping(t *testing.T) {
	raw := `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	events := collectAnthropicStream(t, streamEvent(raw))
	if len(events) != 1 || events[0].Type != "error" || events[0].Error == nil {
		t.Fatalf("events: %+v", events)
	}
	err := events[0].Error
	if err.StatusCode != http.StatusTooManyRequests || err.Message != "slow down" || string(err.Raw) != raw || string(events[0].Raw) != raw {
		t.Fatalf("error: %+v", err)
	}
	if strings.Contains(err.Message, `"type"`) {
		t.Fatalf("raw JSON leaked in message: %s", err.Message)
	}
}
