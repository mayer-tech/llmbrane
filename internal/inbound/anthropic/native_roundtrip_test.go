package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	outbound "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
)

const nativeBlocks = `[
	{"type":"thinking","thinking":"","signature":"sig-empty","extra":{"value":9007199254740993}},
	{"type":"redacted_thinking","data":"opaque-redacted","extra":"preserved"},
	{"type":"text","text":"Before tool"},
	{"type":"thinking","thinking":"consider carefully","signature":"sig-123"},
	{"type":"tool_use","id":"tool-1","name":"lookup","input":{"number":9007199254740993}}
]`

const nativeStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg-native","type":"message","role":"assistant","model":"provider-model","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","extra":{"value":9007199254740993}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-empty"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"opaque-redacted","extra":"preserved"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Before tool"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"thinking_delta","thinking":"consider "}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"thinking_delta","thinking":"carefully"}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"signature_delta","signature":"sig-123"}}

event: content_block_stop
data: {"type":"content_block_stop","index":3}

event: content_block_start
data: {"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"tool-1","name":"lookup","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"number\":9007199254740993}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":4}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":17}}

event: message_stop
data: {"type":"message_stop"}

`

func decodeJSON(t *testing.T, data []byte, dst any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		t.Fatalf("decode JSON %s: %v", data, err)
	}
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var a, b any
	decodeJSON(t, got, &a)
	decodeJSON(t, want, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("JSON mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// collectStream reconstructs the assistant content as a client would from SSE.
func collectStream(t *testing.T, body string) json.RawMessage {
	t.Helper()
	var blocks []map[string]any
	var types []string
	var active bool
	var input strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Index   *int   `json:"index"`
			Message struct {
				ID    string `json:"id"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock json.RawMessage `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		decodeJSON(t, []byte(strings.TrimPrefix(line, "data: ")), &event)
		types = append(types, event.Type)
		switch event.Type {
		case "message_start":
			if event.Message.ID != "msg-native" || event.Message.Usage.InputTokens != 12 {
				t.Fatalf("message start: %+v", event.Message)
			}
		case "content_block_start":
			if active || event.Index == nil || *event.Index != len(blocks) {
				t.Fatalf("invalid block start: %+v", event)
			}
			var block map[string]any
			decodeJSON(t, event.ContentBlock, &block)
			blocks = append(blocks, block)
			active = true
			input.Reset()
		case "content_block_delta":
			if !active || event.Index == nil || *event.Index != len(blocks)-1 {
				t.Fatalf("invalid block delta: %+v", event)
			}
			block := blocks[*event.Index]
			switch event.Delta.Type {
			case "text_delta":
				block["text"] = block["text"].(string) + event.Delta.Text
			case "thinking_delta":
				block["thinking"] = block["thinking"].(string) + event.Delta.Thinking
			case "signature_delta":
				previous, _ := block["signature"].(string)
				block["signature"] = previous + event.Delta.Signature
			case "input_json_delta":
				input.WriteString(event.Delta.PartialJSON)
			default:
				t.Fatalf("unexpected delta: %+v", event.Delta)
			}
		case "content_block_stop":
			if !active || event.Index == nil || *event.Index != len(blocks)-1 {
				t.Fatalf("invalid block stop: %+v", event)
			}
			if input.Len() > 0 {
				var value any
				decodeJSON(t, []byte(input.String()), &value)
				blocks[*event.Index]["input"] = value
			}
			active = false
		case "message_delta":
			if active || event.Delta.StopReason != "tool_use" || event.Usage.OutputTokens != 17 {
				t.Fatalf("message delta: %+v", event)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(types) < 2 || types[0] != "message_start" || types[len(types)-2] != "message_delta" || types[len(types)-1] != "message_stop" {
		t.Fatalf("invalid event order: %v", types)
	}
	data, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAnthropicNativeThinkingToolLoop(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			requests := make(chan json.RawMessage, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("provider request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- body
				var request struct {
					Messages []json.RawMessage `json:"messages"`
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Errorf("decode provider request: %v", err)
					return
				}
				if len(request.Messages) > 1 {
					fmt.Fprint(w, `{"id":"msg-final","type":"message","role":"assistant","model":"provider-model","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":2}}`)
				} else if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, nativeStream)
				} else {
					fmt.Fprintf(w, `{"id":"msg-native","type":"message","role":"assistant","model":"provider-model","content":%s,"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":17}}`, nativeBlocks)
				}
			}))
			defer server.Close()

			gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{
				"anthropic": {Endpoints: []config.Endpoint{{API: "anthropic-messages", URL: server.URL, Capabilities: []string{"streaming", "tools"}, Models: map[string]config.ModelConf{"model": {Target: "provider-model"}}}}},
			}})
			gateway.RegisterOutboundAdapter("anthropic-messages", outbound.NewAdapter())
			handler := NewHandler(gateway)
			first := fmt.Sprintf(`{"model":"anthropic/model","max_tokens":1024,"stream":%t,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`, stream)
			response := httptest.NewRecorder()
			handler.HandleMessages(response, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(first)))
			if response.Code != http.StatusOK {
				t.Fatalf("first response: %d %s", response.Code, response.Body.String())
			}
			<-requests
			var content json.RawMessage
			if stream {
				content = collectStream(t, response.Body.String())
			} else {
				var reply struct {
					ID         string          `json:"id"`
					Content    json.RawMessage `json:"content"`
					StopReason string          `json:"stop_reason"`
					Usage      struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				}
				decodeJSON(t, response.Body.Bytes(), &reply)
				if reply.ID != "msg-native" || reply.StopReason != "tool_use" || reply.Usage.InputTokens != 12 || reply.Usage.OutputTokens != 17 {
					t.Fatalf("unexpected response: %+v", reply)
				}
				content = reply.Content
			}
			assertJSONEqual(t, content, []byte(nativeBlocks))
			followup := fmt.Sprintf(`{"model":"anthropic/model","max_tokens":1024,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":%s},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"failed","is_error":true}]}]}`, content)
			response = httptest.NewRecorder()
			handler.HandleMessages(response, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(followup)))
			if response.Code != http.StatusOK {
				t.Fatalf("follow-up response: %d %s", response.Code, response.Body.String())
			}
			var forwarded struct {
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			decodeJSON(t, <-requests, &forwarded)
			if len(forwarded.Messages) != 3 {
				t.Fatalf("unexpected forwarded messages: %+v", forwarded.Messages)
			}
			assertJSONEqual(t, forwarded.Messages[1].Content, []byte(nativeBlocks))
			assertJSONEqual(t, forwarded.Messages[2].Content, []byte(`[{"type":"tool_result","tool_use_id":"tool-1","content":"failed","is_error":true}]`))
		})
	}
}
