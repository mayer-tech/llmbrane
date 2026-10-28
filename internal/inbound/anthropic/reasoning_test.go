package anthropic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	outboundanthropic "github.com/mayer-tech/llmbrane/internal/outbound/anthropic"
)

func TestTranslateRequestReasoning(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
		fail bool
	}{
		{"absent", `{}`, "", false},
		{"adaptive without effort", `{"thinking":{"type":"adaptive"}}`, "", false},
		{"effort only", `{"output_config":{"effort":"high"}}`, "high", false},
		{"adaptive low", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`, "low", false},
		{"adaptive medium", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}}`, "medium", false},
		{"adaptive high", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, "high", false},
		{"adaptive xhigh", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`, "xhigh", false},
		{"adaptive with effort", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, "max", false},
		{"disabled", `{"thinking":{"type":"disabled"}}`, "none", false},
		{"disabled with budget", `{"thinking":{"type":"disabled","budget_tokens":0}}`, "", true},
		{"disabled with effort", `{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, "", true},
		{"manual with effort", `{"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"low"}}`, "", true},
		{"adaptive with budget", `{"thinking":{"type":"adaptive","budget_tokens":1024}}`, "", true},
		{"missing budget", `{"thinking":{"type":"enabled"}}`, "", true},
		{"negative budget", `{"thinking":{"type":"enabled","budget_tokens":-1}}`, "", true},
		{"zero budget", `{"thinking":{"type":"enabled","budget_tokens":0}}`, "none", false},
		{"minimal budget", `{"thinking":{"type":"enabled","budget_tokens":1024}}`, "minimal", false},
		{"low budget", `{"thinking":{"type":"enabled","budget_tokens":1025}}`, "low", false},
		{"low upper bound", `{"thinking":{"type":"enabled","budget_tokens":2048}}`, "low", false},
		{"medium budget", `{"thinking":{"type":"enabled","budget_tokens":2049}}`, "medium", false},
		{"medium upper bound", `{"thinking":{"type":"enabled","budget_tokens":4096}}`, "medium", false},
		{"high budget", `{"thinking":{"type":"enabled","budget_tokens":4097}}`, "high", false},
		{"high upper bound", `{"thinking":{"type":"enabled","budget_tokens":8192}}`, "high", false},
		{"xhigh budget", `{"thinking":{"type":"enabled","budget_tokens":8193}}`, "xhigh", false},
		{"xhigh upper bound", `{"thinking":{"type":"enabled","budget_tokens":16384}}`, "xhigh", false},
		{"max budget", `{"thinking":{"type":"enabled","budget_tokens":16385}}`, "max", false},
		{"invalid effort", `{"output_config":{"effort":"none"}}`, "", true},
		{"unknown thinking", `{"thinking":{"type":"other"}}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req MessagesRequest
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatal(err)
			}
			got, err := (&Handler{}).translateRequest(&req)
			if (err != nil) != tt.fail {
				t.Fatalf("error = %v, want error %v", err, tt.fail)
			}
			if tt.fail {
				return
			}
			if tt.want == "" {
				if got.Params.ReasoningEffort != nil {
					t.Fatalf("expected unset effort, got %q", *got.Params.ReasoningEffort)
				}
			} else if got.Params.ReasoningEffort == nil || *got.Params.ReasoningEffort != tt.want {
				t.Fatalf("effort = %v, want %q", got.Params.ReasoningEffort, tt.want)
			}
		})
	}
}

func TestHandleMessagesRejectsIncompatibleThinking(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`))
	w := httptest.NewRecorder()
	(&Handler{}).HandleMessages(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestAdaptiveReasoningDefaultsEndToEnd(t *testing.T) {
	tests := []struct {
		name  string
		model string
		input string
		want  string
	}{
		{"model default", "anthropic/model", `{"type":"adaptive"}`, "medium"},
		{"alias default", "alias", `{"type":"adaptive"}`, "high"},
		{"explicit disabled", "alias", `{"type":"disabled"}`, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var received struct {
				Thinking struct {
					Type string `json:"type"`
				} `json:"thinking"`
				OutputConfig *struct {
					Effort string `json:"effort"`
				} `json:"output_config"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Errorf("decode request: %v", err)
				}
				_, _ = w.Write([]byte(`{"id":"id","model":"model","content":[]}`))
			}))
			defer server.Close()
			cfg := &config.Config{
				Aliases: map[string]config.Alias{"alias": {Target: "anthropic/model", DefaultReasoningEffort: "high"}},
				Providers: map[string]config.Provider{"anthropic": {Endpoints: []config.Endpoint{{
					API: "anthropic-messages", URL: server.URL,
					Models: map[string]config.ModelConf{"model": {Target: "model", DefaultReasoningEffort: "medium"}},
				}}}},
			}
			gateway := core.NewGateway(cfg)
			gateway.RegisterOutboundAdapter("anthropic-messages", outboundanthropic.NewAdapter())
			body := `{"model":"` + tt.model + `","messages":[{"role":"user","content":"Hi"}],"thinking":` + tt.input + `}`
			w := httptest.NewRecorder()
			NewHandler(gateway).HandleMessages(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if tt.want == "none" {
				if received.Thinking.Type != "disabled" || received.OutputConfig != nil {
					t.Fatalf("unexpected request: %+v", received)
				}
			} else if received.Thinking.Type != "adaptive" || received.OutputConfig == nil || received.OutputConfig.Effort != tt.want {
				t.Fatalf("unexpected request: %+v", received)
			}
		})
	}
}
