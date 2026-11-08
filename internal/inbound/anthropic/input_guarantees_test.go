package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	outbound "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
)

func TestResponsesInputGuarantees(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, flag := range []string{"true", "false", "absent"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, flag), func(t *testing.T) {
				requests := make(chan json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					requests <- body
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\"}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"m\"}]}}\n\n")
					} else {
						fmt.Fprint(w, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
					}
				}))
				defer server.Close()
				strict, parallel := "", ""
				if flag != "absent" {
					strict = `,"strict":` + flag
					parallel = `,"disable_parallel_tool_use":` + flag
				}
				body := fmt.Sprintf(`{"model":"p/m","stream":%t,"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"custom","name":"f","input_schema":{"type":"object","maximum":9007199254740993}%s}],"tool_choice":{"type":"auto"%s}}`, stream, strict, parallel)
				w := httptest.NewRecorder()
				reviewResponsesHandler(server.URL).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				if w.Code != http.StatusOK || (stream && !strings.Contains(w.Body.String(), "event: message_stop")) {
					t.Fatalf("status: %d %s", w.Code, w.Body.String())
				}
				wire := <-requests
				var sent struct {
					Parallel *bool `json:"parallel_tool_calls"`
					Tools    []struct {
						Strict     *bool           `json:"strict"`
						Parameters json.RawMessage `json:"parameters"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(wire, &sent); err != nil {
					t.Fatal(err)
				}
				if len(sent.Tools) != 1 || sent.Tools[0].Strict == nil || *sent.Tools[0].Strict != (flag == "true") {
					t.Fatalf("tool strict: %s", wire)
				}
				if !strings.Contains(string(sent.Tools[0].Parameters), `9007199254740993`) {
					t.Fatalf("schema precision: %s", wire)
				}
				if flag == "absent" {
					if sent.Parallel != nil {
						t.Fatalf("unexpected parallel setting: %s", wire)
					}
				} else if sent.Parallel == nil || *sent.Parallel != (flag == "false") {
					t.Fatalf("parallel setting: %s", wire)
				}
			})
		}
	}
}

func TestResponsesRejectUnsafeProviderInput(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"id":"r","status":"completed","output":[]}`)
	}))
	defer server.Close()
	h := reviewResponsesHandler(server.URL)
	cases := []struct{ name, extra string }{
		{"format schema", `,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","maximum":9007199254740993}}}`},
		{"format unknown", `,"output_config":{"format":{"type":"future"}}`},
		{"format null", `,"output_config":{"format":null}`},
		{"output unknown", `,"output_config":{"future":true}`},
		{"thinking unknown", `,"thinking":{"type":"adaptive","future":true}`},
		{"parallel string", `,"tool_choice":{"type":"auto","disable_parallel_tool_use":"true"}`},
		{"parallel null", `,"tool_choice":{"type":"auto","disable_parallel_tool_use":null}`},
		{"parallel duplicate", `,"tool_choice":{"type":"auto","disable_parallel_tool_use":true,"disable_parallel_tool_use":false}`},
		{"choice unknown", `,"tool_choice":{"type":"auto","future":true}`},
		{"choice extra name", `,"tool_choice":{"type":"auto","name":"f"}`},
		{"strict string", `,"tools":[{"name":"f","input_schema":{},"strict":"true"}]`},
		{"strict null", `,"tools":[{"name":"f","input_schema":{},"strict":null}]`},
		{"strict duplicate", `,"tools":[{"name":"f","input_schema":{},"strict":true,"strict":false}]`},
		{"server tool", `,"tools":[{"type":"web_search_20250305","name":"f","input_schema":{}}]`},
		{"unknown type", `,"tools":[{"type":"future","name":"f","input_schema":{}}]`},
		{"null type", `,"tools":[{"type":null,"name":"f","input_schema":{}}]`},
		{"unknown tool field", `,"tools":[{"name":"f","input_schema":{},"future":true}]`},
		{"wrong field case", `,"tools":[{"name":"f","input_schema":{},"Strict":true}]`},
		{"unknown request field", `,"future":{"safety":true}`},
		{"duplicate model", `,"model":"p/m"`},
	}
	for _, stream := range []bool{false, true} {
		for _, tt := range cases {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tt.name), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"p/m","stream":%t,"messages":[{"role":"user","content":"hi"}]%s}`, stream, tt.extra)
				w := httptest.NewRecorder()
				h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request_error") {
					t.Fatalf("accepted: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
	for _, content := range []string{
		`[{"type":"text","text":"x","citations":{"enabled":true}}]`,
		`[{"type":"image","source":{"type":"url","url":"https://example.test","future":true}}]`,
		`[{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"x","future":true}]}]`,
	} {
		body := `{"model":"p/m","messages":[{"role":"user","content":` + content + `}]}`
		w := httptest.NewRecorder()
		h.HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted: %d %s", w.Code, w.Body.String())
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("forwarded %d invalid requests", requests.Load())
	}
}

func TestResponsesNamedToolAndArgumentPrecision(t *testing.T) {
	requests := make(chan json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		fmt.Fprint(w, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer server.Close()
	body := `{"model":"p/m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"f","input":{"n":9007199254740993}}]}],"tools":[{"name":"f","input_schema":{"type":"object"},"strict":true}],"tool_choice":{"type":"tool","name":"f","disable_parallel_tool_use":true}}`
	w := httptest.NewRecorder()
	reviewResponsesHandler(server.URL).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	var sent struct {
		Input []struct {
			Arguments string `json:"arguments"`
		} `json:"input"`
		Choice   map[string]string `json:"tool_choice"`
		Parallel *bool             `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(<-requests, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Input) != 1 || sent.Input[0].Arguments != `{"n":9007199254740993}` || sent.Choice["type"] != "function" || sent.Choice["name"] != "f" || sent.Parallel == nil || *sent.Parallel {
		t.Fatalf("unexpected request: %+v", sent)
	}
}

func TestNativeInputOptionsRemainUnchanged(t *testing.T) {
	requests := make(chan json.RawMessage, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body
		fmt.Fprint(w, `{"id":"r","type":"message","role":"assistant","model":"provider","content":[]}`)
	}))
	defer server.Close()
	gateway := core.NewGateway(&config.Config{Providers: map[string]config.Provider{"p": {Endpoints: []config.Endpoint{{API: "anthropic-messages", URL: server.URL, Models: map[string]config.ModelConf{"m": {Target: "provider"}}}}}}})
	gateway.RegisterOutboundAdapter("anthropic-messages", outbound.NewAdapter())
	for _, flag := range []string{"true", "false"} {
		body := `{"model":"p/m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object","maximum":9007199254740993},"strict":` + flag + `}],"tool_choice":{"type":"auto","disable_parallel_tool_use":` + flag + `},"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`
		w := httptest.NewRecorder()
		NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("native request rejected: %d %s", w.Code, w.Body.String())
		}
		wire := <-requests
		var sent map[string]json.RawMessage
		if err := json.Unmarshal(wire, &sent); err != nil {
			t.Fatal(err)
		}
		if _, ok := sent["output_config"]; ok {
			t.Fatalf("native behavior changed: %s", wire)
		}
		if strings.Contains(string(wire), `"strict"`) || strings.Contains(string(wire), `"disable_parallel_tool_use"`) {
			t.Fatalf("native behavior changed: %s", wire)
		}
		if !strings.Contains(string(wire), `9007199254740993`) {
			t.Fatalf("schema precision lost: %s", wire)
		}
	}
	// These values were previously ignored rather than decoded into typed fields.
	body := `{"model":"p/m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{},"strict":{"future":true}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":"future"},"output_config":{"format":42,"future":true}}`
	w := httptest.NewRecorder()
	NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("native request rejected: %d %s", w.Code, w.Body.String())
	}
	<-requests
}
