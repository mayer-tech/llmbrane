package anthropic_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	inboundAnthropic "github.com/mayer-tech/llmbrane/internal/inbound/anthropic"
	inboundOpenAI "github.com/mayer-tech/llmbrane/internal/inbound/openai"
	outbound "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
)

func TestNativeAndChatStreamLifecycle(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, event := range []string{
			`{"type":"message_start","message":{"id":"msg","usage":{"input_tokens":5,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig","extra":7}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hello"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call","name":"f","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer provider.Close()
	endpoint := config.Endpoint{API: "anthropic-messages", URL: provider.URL, Capabilities: []string{"streaming", "tools"}, Models: map[string]config.ModelConf{"model": {Target: "model"}}}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{endpoint}}}})
	gateway.RegisterOutboundAdapter(endpoint.API, outbound.NewAdapter())

	native := httptest.NewRecorder()
	inboundAnthropic.NewHandler(gateway).HandleMessages(native, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := native.Body.String()
	for _, required := range []string{`"type":"message_start"`, `"index":0`, `"index":1`, `"index":2`, `"signature":"sig"`, `"extra":7`, `"stop_reason":"tool_use"`, `"type":"message_stop"`} {
		if !strings.Contains(body, required) {
			t.Fatalf("missing %s in native stream: %s", required, body)
		}
	}
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("native stream error: %s", body)
	}

	chat := httptest.NewRecorder()
	inboundOpenAI.NewHandler(gateway).HandleChatCompletions(chat, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"p/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	foundTool := false
	for _, line := range strings.Split(chat.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimPrefix(line, "data: ") == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index int    `json:"index"`
						ID    string `json:"id"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, choice := range chunk.Choices {
			for _, tool := range choice.Delta.ToolCalls {
				if tool.Index != 0 {
					t.Fatalf("tool index is %d, want 0: %s", tool.Index, chat.Body.String())
				}
				if tool.ID == "call" {
					foundTool = true
				}
			}
		}
	}
	if !foundTool {
		t.Fatalf("missing tool call: %s", chat.Body.String())
	}
}
