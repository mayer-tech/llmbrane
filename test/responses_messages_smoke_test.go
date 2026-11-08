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

type responseItem struct {
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	Content          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responsesResult struct {
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
}

func TestResponsesMessagesSmoke(t *testing.T) {
	model := os.Getenv("LLMBRANE_MANUAL_MODEL")
	if model == "" {
		t.Fatal("set LLMBRANE_MANUAL_MODEL to a model routed to an anthropic-messages endpoint")
	}
	baseURL := strings.TrimRight(os.Getenv("LLMBRANE_MANUAL_BASE_URL"), "/")
	if baseURL == "" {
		baseURL = "http://localhost:8888"
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	conversationID := fmt.Sprintf("manual-responses-messages-%d", time.Now().UnixNano())
	prompt := "Use the multiply tool to multiply the sum of 137 and 89 by the difference between 300 and 49. Report the tool's result as digits without separators in your final answer."
	tools := []any{map[string]any{
		"type": "function", "name": "multiply", "description": "Multiply two integers.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "integer"},
				"b": map[string]any{"type": "integer"},
			},
			"required": []string{"a", "b"},
		},
	}}
	request := func(input any) responsesResult {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"model": model, "input": input, "tools": tools,
			"tool_choice": "auto", "reasoning": map[string]string{"effort": "high"},
			"max_output_tokens": 16384,
		})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, baseURL+"/openai/v1/responses", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-Id", conversationID)
		if key := os.Getenv("LLMBRANE_MANUAL_API_KEY"); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
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
		var result responsesResult
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("decode response: %v; body: %s", err, body)
		}
		if result.Status != "completed" {
			t.Fatalf("response status %q, expected completed: %s", result.Status, body)
		}
		return result
	}

	t.Logf("model: %s; conversation ID: %s", model, conversationID)
	first := request(prompt)
	var calls []responseItem
	reasoningCount := 0
	for _, raw := range first.Output {
		var item responseItem
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("decode output item: %v", err)
		}
		t.Logf("first response item: %s", item.Type)
		switch item.Type {
		case "reasoning":
			if !strings.HasPrefix(item.EncryptedContent, "gw1.") {
				t.Fatalf("reasoning item does not contain a gateway replay envelope; check the model route")
			}
			reasoningCount++
		case "function_call":
			calls = append(calls, item)
		}
	}
	if reasoningCount == 0 {
		t.Fatal("no reasoning item returned; the provider may not have produced thinking, so its mapping could not be tested")
	}
	if len(calls) != 1 {
		t.Fatalf("expected one multiply tool call, got %d", len(calls))
	}
	call := calls[0]
	if call.Name != "multiply" || call.CallID == "" {
		t.Fatalf("unexpected function call: name=%q call_id=%q", call.Name, call.CallID)
	}
	var args struct {
		A *int `json:"a"`
		B *int `json:"b"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || args.A == nil || args.B == nil {
		t.Fatalf("invalid multiply arguments %q: %v", call.Arguments, err)
	}
	if !((*args.A == 226 && *args.B == 251) || (*args.A == 251 && *args.B == 226)) {
		t.Fatalf("unexpected multiply arguments: %s", call.Arguments)
	}
	product := *args.A * *args.B
	t.Logf("executing multiply(%d, %d) = %d", *args.A, *args.B, product)

	// Replay all output items verbatim, including the provider's reasoning envelope.
	input := []any{map[string]any{"role": "user", "content": prompt}}
	for _, item := range first.Output {
		input = append(input, item)
	}
	input = append(input, map[string]any{
		"type": "function_call_output", "call_id": call.CallID, "output": fmt.Sprint(product),
	})
	second := request(input)
	var texts []string
	for _, raw := range second.Output {
		var item responseItem
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("decode final output item: %v", err)
		}
		if item.Type == "function_call" {
			t.Fatalf("unexpected additional function call: %s", raw)
		}
		if item.Type == "message" {
			for _, part := range item.Content {
				if part.Type == "output_text" {
					texts = append(texts, part.Text)
				}
			}
		}
	}
	answer := strings.Join(texts, "\n")
	t.Logf("final answer: %s", answer)
	if !strings.Contains(answer, "56726") {
		t.Fatalf("final answer does not contain the tool result 56726: %q", answer)
	}
}
