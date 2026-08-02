package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestTranslateStreamEvent_EmptyChoices(t *testing.T) {
	h := &Handler{}

	// A done/usage-only event with no choice-triggering fields
	event := &canonical.CanonicalChatStreamEvent{
		Type: "done",
		Usage: &canonical.CanonicalUsage{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	}

	chunk := h.translateStreamEvent(event, "test-model")

	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("failed to marshal chunk: %v", err)
	}

	jsonStr := string(data)
	if strings.Contains(jsonStr, `"choices":null`) {
		t.Errorf("expected choices to be empty array, got null: %s", jsonStr)
	}

	if !strings.Contains(jsonStr, `"choices":[]`) {
		t.Errorf("expected choices to be serialized as empty array [], got: %s", jsonStr)
	}

	if !strings.Contains(jsonStr, `"usage"`) {
		t.Errorf("expected usage to be present in json: %s", jsonStr)
	}
}

func TestTranslateResponse_EmptyChoices(t *testing.T) {
	h := &Handler{}

	res := &canonical.CanonicalChatResponse{
		ID:      "test-id",
		Model:   "test-model",
		Choices: nil, // empty choices
	}

	translated := h.translateResponse(res)

	data, err := json.Marshal(translated)
	if err != nil {
		t.Fatalf("failed to marshal response: %v", err)
	}

	jsonStr := string(data)
	if strings.Contains(jsonStr, `"choices":null`) {
		t.Errorf("expected choices to be empty array, got null: %s", jsonStr)
	}

	if !strings.Contains(jsonStr, `"choices":[]`) {
		t.Errorf("expected choices to be serialized as empty array [], got: %s", jsonStr)
	}
}

func TestTranslateStreamEvent_ReasoningContent(t *testing.T) {
	h := &Handler{}

	event := &canonical.CanonicalChatStreamEvent{
		Type:         "content_delta",
		MessageIndex: 0,
		Delta: &canonical.CanonicalContentPart{
			Type: "reasoning",
			Reasoning: &canonical.CanonicalReasoning{
				Content: "Thinking about math...",
			},
		},
	}

	chunk := h.translateStreamEvent(event, "test-model")
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("failed to marshal chunk: %v", err)
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"reasoning_content":"Thinking about math..."`) {
		t.Errorf("expected reasoning_content in json, got: %s", jsonStr)
	}

	if strings.Contains(jsonStr, `"content":`) {
		t.Errorf("expected content field to be omitted for reasoning delta, got: %s", jsonStr)
	}
}

func TestTranslateStreamEvent_EmptyDeltaFiltered(t *testing.T) {
	h := &Handler{}

	// Content delta event with empty text/delta (e.g. keep-alive or flush event)
	event := &canonical.CanonicalChatStreamEvent{
		Type:         "content_delta",
		MessageIndex: 0,
		Delta: &canonical.CanonicalContentPart{
			Type: "text",
			Text: "",
		},
	}

	chunk := h.translateStreamEvent(event, "test-model")
	if len(chunk.Choices) != 0 {
		t.Errorf("expected empty choices array for empty delta event, got len = %d", len(chunk.Choices))
	}
}

func TestTranslateRequest_ContentParts(t *testing.T) {
	h := &Handler{}

	body := `{
		"model": "gpt-4o",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "What is in this image?"},
					{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo...", "detail": "high"}}
				]
			}
		]
	}`

	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("failed to unmarshal request: %v", err)
	}

	canonReq, err := h.translateRequest(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(canonReq.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(canonReq.Messages))
	}

	msg := canonReq.Messages[0]
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(msg.Content))
	}

	if msg.Content[0].Type != "text" || msg.Content[0].Text != "What is in this image?" {
		t.Errorf("unexpected first content part: %+v", msg.Content[0])
	}

	if msg.Content[1].Type != "image" || msg.Content[1].Image == nil {
		t.Fatalf("unexpected second content part: %+v", msg.Content[1])
	}

	if msg.Content[1].Image.URL != "data:image/png;base64,iVBORw0KGgo..." {
		t.Errorf("unexpected image URL: %s", msg.Content[1].Image.URL)
	}

	if msg.Content[1].Image.Detail != "high" {
		t.Errorf("unexpected image Detail: %s", msg.Content[1].Image.Detail)
	}
}
