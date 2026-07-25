package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestProcessChat_EmptyChoices(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-123","model":"gpt-4","choices":[]}`))
	}))
	defer ts.Close()

	adapter := NewAdapter()
	req := &canonical.CanonicalChatRequest{
		Model: "gpt-4",
		Messages: []canonical.CanonicalMessage{
			{Role: "user", Content: []canonical.CanonicalContentPart{{Type: "text", Text: "Hello"}}},
		},
	}
	endpoint := config.Endpoint{URL: ts.URL}

	_, err := adapter.ProcessChat(context.Background(), req, "gpt-4", endpoint)
	if err == nil {
		t.Fatalf("expected error when provider returns empty choices, got nil")
	}

	gwErr, ok := err.(*canonical.GatewayError)
	if !ok {
		t.Fatalf("expected GatewayError, got %T: %v", err, err)
	}

	if gwErr.StatusCode != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d", gwErr.StatusCode)
	}
}

func TestProcessChat_ReasoningContentFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-123","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"Thinking deeply..."}}]}`))
	}))
	defer ts.Close()

	adapter := NewAdapter()
	req := &canonical.CanonicalChatRequest{
		Model: "gpt-4",
		Messages: []canonical.CanonicalMessage{
			{Role: "user", Content: []canonical.CanonicalContentPart{{Type: "text", Text: "Hello"}}},
		},
	}
	endpoint := config.Endpoint{URL: ts.URL}

	res, err := adapter.ProcessChat(context.Background(), req, "gpt-4", endpoint)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(res.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(res.Choices))
	}

	if len(res.Choices[0].Message.Content) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(res.Choices[0].Message.Content))
	}

	if res.Choices[0].Message.Content[0].Text != "Thinking deeply..." {
		t.Errorf("expected text 'Thinking deeply...', got '%s'", res.Choices[0].Message.Content[0].Text)
	}
}

func TestProcessChatStream_ReasoningContentFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Streamed thought\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer ts.Close()

	adapter := NewAdapter()
	req := &canonical.CanonicalChatRequest{
		Model: "gpt-4",
		Messages: []canonical.CanonicalMessage{
			{Role: "user", Content: []canonical.CanonicalContentPart{{Type: "text", Text: "Hello"}}},
		},
	}
	endpoint := config.Endpoint{URL: ts.URL}

	stream, err := adapter.ProcessChatStream(context.Background(), req, "gpt-4", endpoint)
	if err != nil {
		t.Fatalf("unexpected error starting stream: %v", err)
	}

	var events []*canonical.CanonicalChatStreamEvent
	for ev := range stream {
		events = append(events, ev)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	if events[0].Type != "content_delta" {
		t.Errorf("expected event type content_delta, got %s", events[0].Type)
	}

	if events[0].Delta.Text != "Streamed thought" {
		t.Errorf("expected delta text 'Streamed thought', got '%s'", events[0].Delta.Text)
	}
}
