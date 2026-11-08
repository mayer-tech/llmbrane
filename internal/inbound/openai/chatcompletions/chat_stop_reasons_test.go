package chatcompletions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	inboundanthropic "github.com/mayer-tech/llmbrane/internal/inbound/anthropic"
	"github.com/mayer-tech/llmbrane/internal/inbound/openai/responses"
	outboundanthropic "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
)

func TestChatCompletionFinishReasons(t *testing.T) {
	for _, tt := range []struct {
		reason string
		want   string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"stop", "stop"},
		{"length", "length"},
		{"tool_calls", "tool_calls"},
		{"content_filter", "content_filter"},
		{"function_call", "function_call"},
		{"future_reason", "future_reason"},
		{"", ""},
	} {
		t.Run(fmt.Sprintf("reason=%q", tt.reason), func(t *testing.T) {
			h := &Handler{}
			stop := &canonical.CanonicalStop{Reason: tt.reason, Native: &canonical.CanonicalProviderData{Provider: "anthropic", Format: "message_delta", Data: json.RawMessage(`{"extra":42}`)}}
			response := &canonical.CanonicalChatResponse{
				Choices: []canonical.CanonicalChoice{
					{Index: 3, FinishReason: tt.reason},
					{Index: 7, FinishReason: "length"},
				},
				Stop: stop,
			}
			got := h.translateResponse(response)
			if len(got.Choices) != 2 || got.Choices[0].Index != 3 || got.Choices[0].FinishReason != tt.want || got.Choices[1].Index != 7 || got.Choices[1].FinishReason != "length" {
				t.Fatalf("translated choices: %+v", got.Choices)
			}
			event := &canonical.CanonicalChatStreamEvent{Type: "done", MessageIndex: 3, Stop: stop}
			chunk := h.translateStreamEvent(event, "model")
			if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 3 || chunk.Choices[0].FinishReason == nil || *chunk.Choices[0].FinishReason != tt.want {
				t.Fatalf("translated stream: %+v", chunk.Choices)
			}
			if response.Choices[0].FinishReason != tt.reason || response.Stop.Reason != tt.reason || event.Stop.Reason != tt.reason || string(stop.Native.Data) != `{"extra":42}` {
				t.Fatalf("canonical response or event was modified: %+v %+v", response, event)
			}
		})
	}

	chunk := (&Handler{}).translateStreamEvent(&canonical.CanonicalChatStreamEvent{Type: "done"}, "model")
	if len(chunk.Choices) != 0 {
		t.Fatalf("missing stop produced a finish reason: %+v", chunk.Choices)
	}
}

func TestAnthropicStopReasonsThroughChatCompletions(t *testing.T) {
	for _, tt := range []struct {
		reason string
		want   string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"future_reason", "future_reason"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tt.reason, stream), func(t *testing.T) {
				provider := stopReasonProvider(tt.reason)
				defer provider.Close()
				gateway := stopReasonGateway(provider.URL)
				body := fmt.Sprintf(`{"model":"p/m","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)
				w := httptest.NewRecorder()
				NewHandler(gateway).HandleChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
				if w.Code != http.StatusOK {
					t.Fatalf("status %d: %s", w.Code, w.Body)
				}
				if !stream {
					var response ChatCompletionResponse
					if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if len(response.Choices) != 1 || response.Choices[0].FinishReason != tt.want || response.Choices[0].Message == nil {
						t.Fatalf("response: %s", w.Body)
					}
					message := response.Choices[0].Message
					if tt.reason == "tool_use" {
						if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call_1" || message.ToolCalls[0].Function.Arguments != `{"x":2}` {
							t.Fatalf("tool calls: %+v", message.ToolCalls)
						}
					} else if string(message.Content) != `"hello"` {
						t.Fatalf("content: %s", message.Content)
					}
					if response.Usage == nil || response.Usage.TotalTokens != 8 {
						t.Fatalf("usage: %+v", response.Usage)
					}
					return
				}

				var reasons []string
				var text, arguments strings.Builder
				var toolID string
				var usage *Usage
				done := 0
				for _, line := range strings.Split(w.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					data := strings.TrimPrefix(line, "data: ")
					if data == "[DONE]" {
						done++
						continue
					}
					var chunk ChatCompletionStreamResponse
					if err := json.Unmarshal([]byte(data), &chunk); err != nil {
						t.Fatalf("invalid chunk %q: %v", data, err)
					}
					if chunk.Usage != nil {
						usage = chunk.Usage
					}
					for _, choice := range chunk.Choices {
						if choice.FinishReason != nil {
							reasons = append(reasons, *choice.FinishReason)
						}
						if choice.Delta.Content != nil {
							text.WriteString(*choice.Delta.Content)
						}
						for _, call := range choice.Delta.ToolCalls {
							if call.ID != "" {
								toolID = call.ID
							}
							if call.Function != nil {
								arguments.WriteString(call.Function.Arguments)
							}
						}
					}
				}
				if !reflect.DeepEqual(reasons, []string{tt.want}) || done != 1 || usage == nil || usage.TotalTokens != 8 {
					t.Fatalf("stream: %s", w.Body)
				}
				if tt.reason == "tool_use" {
					if toolID != "call_1" || arguments.String() != `{"x":2}` {
						t.Fatalf("tool stream: %s", w.Body)
					}
				} else if text.String() != "hello" {
					t.Fatalf("text stream: %s", w.Body)
				}
			})
		}
	}
}

func TestAnthropicStopReasonsRemainIntactForMessagesAndResponses(t *testing.T) {
	for _, reason := range []string{"end_turn", "stop_sequence"} {
		for _, stream := range []bool{false, true} {
			for _, api := range []string{"messages", "responses"} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", api, reason, stream), func(t *testing.T) {
					provider := stopReasonProvider(reason)
					defer provider.Close()
					gateway := stopReasonGateway(provider.URL)
					mux := http.NewServeMux()
					mux.HandleFunc("POST /openai/v1/responses", responses.NewHandler(gateway).HandleResponses)
					inboundanthropic.NewHandler(gateway).RegisterRoutes(mux, "/anthropic")
					path := "/anthropic/v1/messages"
					body := fmt.Sprintf(`{"model":"p/m","messages":[{"role":"user","content":"hello"}],"max_tokens":100,"stream":%t}`, stream)
					if api == "responses" {
						path = "/openai/v1/responses"
						body = fmt.Sprintf(`{"model":"p/m","input":"hello","stream":%t}`, stream)
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
					if w.Code != http.StatusOK {
						t.Fatalf("status %d: %s", w.Code, w.Body)
					}
					if !stream {
						var result map[string]any
						if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
							t.Fatal(err)
						}
						if api == "messages" && result["stop_reason"] != reason {
							t.Fatalf("message stop reason: %s", w.Body)
						}
						if api == "responses" && result["status"] != "completed" {
							t.Fatalf("response status: %s", w.Body)
						}
						return
					}
					found := false
					for _, line := range strings.Split(w.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
							t.Fatal(err)
						}
						if event["type"] == "error" {
							t.Fatalf("stream error: %s", w.Body)
						}
						if api == "messages" && event["type"] == "message_delta" {
							delta, ok := event["delta"].(map[string]any)
							found = ok && delta["stop_reason"] == reason
						}
						if api == "responses" && event["type"] == "response.completed" {
							response, ok := event["response"].(map[string]any)
							found = ok && response["status"] == "completed"
						}
					}
					if !found {
						t.Fatalf("missing terminal event: %s", w.Body)
					}
				})
			}
		}
	}
}

func stopReasonGateway(url string) *core.Gateway {
	endpoint := config.Endpoint{API: "anthropic-messages", URL: url, Models: map[string]config.ModelConf{"m": {Target: "claude"}}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{endpoint}}}})
	gateway.RegisterOutboundAdapter(endpoint.API, outboundanthropic.NewAdapter())
	return gateway
}

func stopReasonProvider(reason string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		content := `[{"type":"text","text":"hello"}]`
		if reason == "tool_use" {
			content = `[{"type":"tool_use","id":"call_1","name":"f","input":{"x":2}}]`
		}
		stopSequence := "null"
		if reason == "stop_sequence" {
			stopSequence = `"END"`
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":%s,"stop_reason":%q,"stop_sequence":%s,"usage":{"input_tokens":5,"output_tokens":3}}`, content, reason, stopSequence)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n")
		if reason == "tool_use" {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"f\",\"input\":{}}}\n\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"x\\\":2}\"}}\n\n")
		} else {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		}
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q,\"stop_sequence\":%s},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n", reason, stopSequence)
	}))
}
