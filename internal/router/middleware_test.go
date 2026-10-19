package router_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/core"
	"github.com/mayer-tech/llmbrane/internal/debuglog"
	inbound "github.com/mayer-tech/llmbrane/internal/inbound/openai"
	outbound "github.com/mayer-tech/llmbrane/internal/outbound/openai"
	"github.com/mayer-tech/llmbrane/internal/router"
)

func readLogEntries(t *testing.T, path string) []debuglog.Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result []debuglog.Entry
	for _, block := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
		_, body, ok := strings.Cut(block, "\n")
		if !ok {
			t.Fatalf("missing entry body in %s", path)
		}
		var entry debuglog.Entry
		if err := json.Unmarshal([]byte(body), &entry); err != nil {
			t.Fatal(err)
		}
		result = append(result, entry)
	}
	return result
}

func TestDebugLogCorrelatesAllLayers(t *testing.T) {
	for _, test := range []struct {
		name         string
		conversation string
		interaction  string
		stream       bool
		retry        bool
	}{
		{name: "explicit conversation takes precedence", conversation: "conversation-1", interaction: "interaction-1"},
		{name: "interaction fallback", interaction: "interaction-1"},
		{name: "generated request groups"},
		{name: "streaming", conversation: "stream-1", stream: true},
		{name: "streaming without ID", stream: true},
		{name: "retry", conversation: "retry-1", retry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := t.TempDir()
			if err := debuglog.Init(d, 24*time.Hour); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := debuglog.Init("", 0); err != nil {
					t.Error(err)
				}
			})

			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if test.retry && call == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"error":{"message":"try again"}}`)
					return
				}
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"id\":\"response-1\",\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"response-1","model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`)
			}))
			defer provider.Close()

			cfg := &config.Config{
				Providers: map[string]config.Provider{
					"test": {Endpoints: []config.Endpoint{{
						API:    "openai-chat-completions",
						URL:    provider.URL,
						Models: map[string]config.ModelConf{"test": {Target: "test"}},
					}}},
				},
			}
			gateway := core.NewGateway(cfg)
			gateway.RegisterOutboundAdapter("openai-chat-completions", outbound.NewAdapterWithTransport(time.Second, debuglog.NewTransport(nil)))
			mux := http.NewServeMux()
			inbound.NewHandler(gateway).RegisterRoutes(mux, "/openai")
			handler := router.DebugLog(mux)
			for range 2 {
				body := fmt.Sprintf(`{"model":"test/test","messages":[{"role":"user","content":"Hello"}],"stream":%t}`, test.stream)
				req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("X-Conversation-Id", test.conversation)
				req.Header.Set("X-Interaction-Id", test.interaction)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, req)
				if recorder.Code != http.StatusOK {
					t.Fatalf("unexpected response %d: %s", recorder.Code, recorder.Body.String())
				}
			}

			// Stream bodies may be closed just after the canonical channel closes.
			wantOutbound := 2
			if test.retry {
				wantOutbound++
			}
			deadline := time.Now().Add(2 * time.Second)
			var files []string
			for {
				var err error
				files, err = filepath.Glob(filepath.Join(d, "*.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				outboundEntries := 0
				for _, file := range files {
					if strings.HasSuffix(file, "_outbound.jsonl") {
						data, err := os.ReadFile(file)
						if err != nil {
							t.Fatal(err)
						}
						// The trailing blank line is written after the JSON body.
						outboundEntries += strings.Count(string(data), "\n\n")
					}
				}
				if outboundEntries == wantOutbound {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("expected %d outbound entries, got %d", wantOutbound, outboundEntries)
				}
				time.Sleep(time.Millisecond)
			}

			conversationID := test.conversation
			if conversationID == "" {
				conversationID = test.interaction
			}
			wantFiles := 3
			if conversationID == "" {
				wantFiles = 6
			}
			if len(files) != wantFiles {
				t.Fatalf("expected %d files, got %v", wantFiles, files)
			}
			counts := make(map[string]map[string]int)
			prefixes := make(map[string]string)
			for _, path := range files {
				for _, entry := range readLogEntries(t, path) {
					if entry.RequestID == "" || entry.ConversationID != conversationID {
						t.Fatalf("unexpected correlation: %+v", entry)
					}
					if counts[entry.RequestID] == nil {
						counts[entry.RequestID] = make(map[string]int)
					}
					counts[entry.RequestID][entry.Direction]++
					prefix := strings.TrimSuffix(filepath.Base(path), "_"+entry.Direction+".jsonl")
					if existing := prefixes[entry.RequestID]; existing != "" && existing != prefix {
						t.Fatalf("request %s spans different filename prefixes", entry.RequestID)
					}
					prefixes[entry.RequestID] = prefix
					id := conversationID
					if id == "" {
						id = entry.RequestID
					}
					if !strings.HasSuffix(prefix, "_"+id) {
						t.Fatalf("ID not before direction in %s", path)
					}
					if test.stream && len(entry.StreamEvents) == 0 {
						t.Fatalf("missing stream events in %s", path)
					}
				}
			}
			if len(counts) != 2 {
				t.Fatalf("expected two request IDs, got %v", counts)
			}
			outboundCount := 0
			for id, directions := range counts {
				if directions["inbound"] != 1 || directions["canonical"] != 1 || directions["outbound"] < 1 {
					t.Fatalf("request %s missing correlated layers: %v", id, directions)
				}
				outboundCount += directions["outbound"]
			}
			if outboundCount != wantOutbound {
				t.Fatalf("expected %d outbound entries, got %d", wantOutbound, outboundCount)
			}
		})
	}
}
