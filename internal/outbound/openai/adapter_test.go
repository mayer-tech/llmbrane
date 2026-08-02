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

func TestProcessChat_ReasoningContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-123","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Answer","reasoning_content":"Thinking deeply..."}}]}`))
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

	if len(res.Choices[0].Message.Content) != 2 {
		t.Fatalf("expected 2 content parts (text and reasoning), got %d", len(res.Choices[0].Message.Content))
	}

	if res.Choices[0].Message.Content[0].Text != "Answer" {
		t.Errorf("expected text 'Answer', got '%s'", res.Choices[0].Message.Content[0].Text)
	}

	if res.Choices[0].Message.Content[1].Reasoning == nil || res.Choices[0].Message.Content[1].Reasoning.Content != "Thinking deeply..." {
		t.Errorf("expected reasoning 'Thinking deeply...', got '%v'", res.Choices[0].Message.Content[1].Reasoning)
	}
}

func TestProcessChatStream_ReasoningContent(t *testing.T) {
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

	if events[0].Delta.Type != "reasoning" || events[0].Delta.Reasoning == nil || events[0].Delta.Reasoning.Content != "Streamed thought" {
		t.Errorf("expected reasoning delta 'Streamed thought', got '%v'", events[0].Delta)
	}
}

func TestTranslateCanonicalRequest_ImageContent(t *testing.T) {
	adapter := NewAdapter()
	req := &canonical.CanonicalChatRequest{
		Model: "gpt-4o",
		Messages: []canonical.CanonicalMessage{
			{
				Role: canonical.RoleUser,
				Content: []canonical.CanonicalContentPart{
					{Type: "text", Text: "Look at this image:"},
					{Type: "image", Image: &canonical.CanonicalImage{URL: "data:image/png;base64,iVBORw0KGgo...", Detail: "high"}},
				},
			},
		},
	}

	endpoint := config.Endpoint{URL: "http://example.com"}
	outboundReq := adapter.translateCanonicalRequest(req, "gpt-4o", false, endpoint)

	if len(outboundReq.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(outboundReq.Messages))
	}

	contentParts, ok := outboundReq.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("expected message content to be []any, got %T", outboundReq.Messages[0].Content)
	}

	if len(contentParts) != 2 {
		t.Fatalf("expected 2 content parts in outbound message, got %d", len(contentParts))
	}
}
