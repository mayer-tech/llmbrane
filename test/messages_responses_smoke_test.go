//go:build manual

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type messagesResult struct {
	StopReason string            `json:"stop_reason"`
	Content    []json.RawMessage `json:"content"`
}

type messagesBlock struct {
	Type      string          `json:"type"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Text      string          `json:"text"`
}

func TestMessagesResponsesSmoke(t *testing.T) {
	model := os.Getenv("LLMBRANE_MANUAL_MODEL")
	if model == "" {
		t.Fatal("set LLMBRANE_MANUAL_MODEL to a model routed to an openai-responses endpoint")
	}
	baseURL := strings.TrimRight(os.Getenv("LLMBRANE_MANUAL_BASE_URL"), "/")
	if baseURL == "" {
		baseURL = "http://localhost:8888"
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	conversationID := fmt.Sprintf("manual-messages-responses-%d", time.Now().UnixNano())
	prompt := "Use the multiply tool to multiply the sum of 137 and 89 by the difference between 300 and 49. Report the tool's result as digits without separators in your final answer."
	tools := []any{map[string]any{
		"name": "multiply", "description": "Multiply two integers.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "integer"},
				"b": map[string]any{"type": "integer"},
			},
			"required": []string{"a", "b"},
		},
	}}
	request := func(messages []any) messagesResult {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"model": model, "messages": messages, "tools": tools,
			"tool_choice":   map[string]string{"type": "auto"},
			"thinking":      map[string]string{"type": "adaptive", "display": "summarized"},
			"output_config": map[string]string{"effort": "high"},
			"max_tokens":    16384,
		})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, baseURL+"/anthropic/v1/messages", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("X-Conversation-Id", conversationID)
		if key := os.Getenv("LLMBRANE_MANUAL_API_KEY"); key != "" {
			req.Header.Set("X-Api-Key", key)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
		}
		var result messagesResult
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("decode response: %v; body: %s", err, body)
		}
		return result
	}

	t.Logf("model: %s; conversation ID: %s", model, conversationID)
	messages := []any{map[string]any{"role": "user", "content": prompt}}
	first := request(messages)
	if first.StopReason != "tool_use" {
		t.Fatalf("expected tool_use stop reason, got %q", first.StopReason)
	}
	var calls []messagesBlock
	reasoningCount := 0
	for _, raw := range first.Content {
		var block messagesBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatalf("decode content block: %v", err)
		}
		t.Logf("first response block: %s", block.Type)
		switch block.Type {
		case "thinking":
			if !strings.HasPrefix(block.Signature, "gw1.") {
				t.Fatal("thinking block does not contain a gateway replay envelope; check the model route")
			}
			reasoningCount++
		case "tool_use":
			calls = append(calls, block)
		}
	}
	if reasoningCount == 0 {
		t.Fatal("no thinking block returned; the provider may not have produced reasoning, so its mapping could not be tested")
	}
	if len(calls) != 1 {
		t.Fatalf("expected one multiply tool call, got %d", len(calls))
	}
	call := calls[0]
	if call.Name != "multiply" || call.ID == "" {
		t.Fatalf("unexpected tool call: name=%q id=%q", call.Name, call.ID)
	}
	var args struct {
		A *int `json:"a"`
		B *int `json:"b"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil || args.A == nil || args.B == nil {
		t.Fatalf("invalid multiply input %s: %v", call.Input, err)
	}
	if !((*args.A == 226 && *args.B == 251) || (*args.A == 251 && *args.B == 226)) {
		t.Fatalf("unexpected multiply input: %s", call.Input)
	}
	product := *args.A * *args.B
	t.Logf("executing multiply(%d, %d) = %d", *args.A, *args.B, product)

	// Replay the complete assistant content, including the reasoning signature.
	messages = append(messages,
		map[string]any{"role": "assistant", "content": first.Content},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": call.ID, "content": fmt.Sprint(product),
		}}},
	)
	second := request(messages)
	if second.StopReason != "end_turn" {
		t.Fatalf("expected end_turn stop reason, got %q", second.StopReason)
	}
	var texts []string
	for _, raw := range second.Content {
		var block messagesBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatalf("decode final content block: %v", err)
		}
		if block.Type == "tool_use" {
			t.Fatalf("unexpected additional tool call: %s", raw)
		}
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	answer := strings.Join(texts, "\n")
	t.Logf("final answer: %s", answer)
	if !strings.Contains(answer, "56726") {
		t.Fatalf("final answer does not contain the tool result 56726: %q", answer)
	}
}
