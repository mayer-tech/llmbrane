package anthropic

import (
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

func TestResponsesNonStreamReplay(t *testing.T) {
	item := `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"one"},{"type":"summary_text","text":"two"}],"encrypted_content":"cipher","extra":{"n":9007199254740993}}`
	requests := make(chan json.RawMessage, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		fmt.Fprintf(w, `{"id":"r","model":"provider","status":"completed","output":[%s,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool","arguments":"{}"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}],"usage":{"input_tokens":3,"output_tokens":4}}`, item)
	}))
	defer server.Close()
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "openai-responses", URL: server.URL, Models: map[string]config.ModelConf{"m": {Target: "provider"}}}}}}})
	gateway.RegisterOutboundAdapter("openai-responses", outbound.NewAdapter())
	h := NewHandler(gateway)
	first := httptest.NewRecorder()
	h.HandleMessages(first, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/m","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","display":"summarized"}}`)))
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	initial := <-requests
	if !strings.Contains(string(initial), `"reasoning.encrypted_content"`) || !strings.Contains(string(initial), `"summary":"auto"`) {
		t.Fatalf("missing reasoning options: %s", initial)
	}
	var reply struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Content) != 4 || reply.StopReason != "tool_use" || reply.Content[0]["thinking"] != "one\ntwo" || reply.Content[2]["id"] != "call_1" {
		t.Fatalf("bad response: %s", first.Body.String())
	}
	envelope, _ := reply.Content[0]["signature"].(string)
	decoded, err := canonical.DecodeReasoningEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, decoded.Data, []byte(item))
	content, err := json.Marshal(reply.Content)
	if err != nil {
		t.Fatal(err)
	}
	followup := fmt.Sprintf(`{"model":"p/m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":%s},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}]}`, content)
	second := httptest.NewRecorder()
	h.HandleMessages(second, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(followup)))
	if second.Code != http.StatusOK {
		t.Fatalf("followup: %d %s", second.Code, second.Body.String())
	}
	var forwarded struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(<-requests, &forwarded); err != nil {
		t.Fatal(err)
	}
	if len(forwarded.Input) != 6 {
		t.Fatalf("wrong input: %s", forwarded.Input)
	}
	assertJSONEqual(t, forwarded.Input[1], []byte(item))
	if strings.Contains(string(forwarded.Input[1]), envelope) || !strings.Contains(string(forwarded.Input[3]), `"call_id":"call_1"`) {
		t.Fatalf("bad replay: %s", forwarded.Input)
	}
}

func TestRejectMalformedEnvelopeAndWrongRoute(t *testing.T) {
	h := &Handler{}
	for _, value := range []string{"gw2.bad", "gw1.invalid"} {
		var req MessagesRequest
		body := fmt.Sprintf(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":%q}]}]}`, value)
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		if _, err := h.translateRequest(&req); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	item := json.RawMessage(`{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"cipher"}`)
	envelope, err := canonical.EncodeReasoningEnvelope("openai", item)
	if err != nil {
		t.Fatal(err)
	}
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "anthropic-messages", Models: map[string]config.ModelConf{"m": {Target: "m"}}}}}}})
	body := fmt.Sprintf(`{"model":"p/m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":%q}]}]}`, envelope)
	w := httptest.NewRecorder()
	NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("wrong route accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestResponsesRejectMissingCiphertext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"r","model":"provider","status":"completed","output":[{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"visible"}]}]}`)
	}))
	defer server.Close()
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "openai-responses", URL: server.URL, Models: map[string]config.ModelConf{"m": {Target: "provider"}}}}}}})
	gateway.RegisterOutboundAdapter("openai-responses", outbound.NewAdapter())
	w := httptest.NewRecorder()
	NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"p/m","messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("missing ciphertext accepted: %d %s", w.Code, w.Body.String())
	}
}
