package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	outbound "github.com/mayer-tech/llmbrane/internal/outbound/openairesponses"
)

func TestResponsesStreamHiddenReasoningReplay(t *testing.T) {
	item := `{"type":"reasoning","id":"rs_hidden","summary":[],"encrypted_content":"cipher","extra":{"n":9007199254740993}}`
	requests := make(chan json.RawMessage, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &request)
		if !request.Stream {
			fmt.Fprint(w, `{"id":"r2","model":"provider","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_hidden"}}`,
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg"}}`,
			`{"type":"response.content_part.added","output_index":1,"item_id":"msg","content_index":0,"part":{"type":"output_text","text":""}}`,
			`{"type":"response.output_text.delta","output_index":1,"item_id":"msg","content_index":0,"delta":"hello"}`,
			`{"type":"response.content_part.done","output_index":1,"item_id":"msg","content_index":0,"part":{"type":"output_text","text":"hello"}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"reasoning","id":"rs_hidden"},{"type":"message","id":"msg"}],"usage":{"input_tokens":2,"output_tokens":3}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer server.Close()
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "openai-responses", URL: server.URL, Capabilities: []string{"streaming"}, Models: map[string]config.ModelConf{"m": {Target: "provider"}}}}}}})
	gateway.RegisterOutboundAdapter("openai-responses", outbound.NewAdapter())
	h := NewHandler(gateway)
	w := httptest.NewRecorder()
	h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("stream: %d %s", w.Code, w.Body.String())
	}
	<-requests
	var blocks []map[string]any
	active := -1
	var terminal string
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "data: ") {
			continue
		}
		var event struct {
			Type  string         `json:"type"`
			Index *int           `json:"index"`
			Block map[string]any `json:"content_block"`
			Delta map[string]any `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "error":
			t.Fatalf("stream error: %s", scanner.Text())
		case "content_block_start":
			if active != -1 || event.Index == nil || *event.Index != len(blocks) {
				t.Fatalf("bad block start: %s", scanner.Text())
			}
			active = *event.Index
			blocks = append(blocks, event.Block)
		case "content_block_delta":
			if event.Index == nil || *event.Index != active {
				t.Fatalf("bad delta: %s", scanner.Text())
			}
			switch event.Delta["type"] {
			case "signature_delta":
				blocks[active]["signature"] = event.Delta["signature"]
			case "thinking_delta":
				t.Fatal("hidden reasoning must not invent text")
			case "text_delta":
				blocks[active]["text"] = event.Delta["text"]
			}
		case "content_block_stop":
			if event.Index == nil || *event.Index != active {
				t.Fatalf("bad stop: %s", scanner.Text())
			}
			active = -1
		case "message_stop":
			terminal = event.Type
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if active != -1 || terminal != "message_stop" || len(blocks) != 2 || blocks[0]["thinking"] != "" || blocks[1]["text"] != "hello" {
		t.Fatalf("invalid stream: %s", w.Body.String())
	}
	envelope, _ := blocks[0]["signature"].(string)
	decoded, err := canonical.DecodeReasoningEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, decoded.Data, []byte(item))
	content, _ := json.Marshal(blocks)
	followup := fmt.Sprintf(`{"model":"p/m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":%s},{"role":"user","content":"next"}]}`, content)
	w = httptest.NewRecorder()
	h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(followup)))
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	var forwarded struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(<-requests, &forwarded); err != nil {
		t.Fatal(err)
	}
	if len(forwarded.Input) != 4 {
		t.Fatalf("bad input: %s", forwarded.Input)
	}
	assertJSONEqual(t, forwarded.Input[1], []byte(item))
}

func TestResponsesStreamErrorAfterPartialReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"+
			"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs\"}}\n\n"+
			"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs\",\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "openai-responses", URL: server.URL, Capabilities: []string{"streaming"}, Models: map[string]config.ModelConf{"m": {Target: "provider"}}}}}}})
	gateway.RegisterOutboundAdapter("openai-responses", outbound.NewAdapter())
	w := httptest.NewRecorder()
	NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	body := w.Body.String()
	if !strings.Contains(body, "event: error\n") || strings.Contains(body, "signature_delta") || strings.Contains(body, "event: message_stop\n") || strings.Contains(body, "event: content_block_stop\n") {
		t.Fatalf("unexpected interrupted stream: %s", body)
	}
}
