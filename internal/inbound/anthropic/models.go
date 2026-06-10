package anthropic

import "encoding/json"

// Message represents an incoming Anthropic message.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []ContentBlock
}

// ContentBlock represents a heterogeneous block of content.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"` // string or []ContentBlock for tool_result
	IsError   *bool           `json:"is_error,omitempty"`
}

// Tool represents a tool available to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolChoice specifies how tools should be used.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// MessagesRequest represents an incoming Anthropic messages request.
type MessagesRequest struct {
	Model         string      `json:"model"`
	Messages      []Message   `json:"messages"`
	System        any         `json:"system,omitempty"` // string or []ContentBlock
	MaxTokens     int         `json:"max_tokens"`
	StopSequences []string    `json:"stop_sequences,omitempty"`
	Stream        bool        `json:"stream,omitempty"`
	Temperature   *float64    `json:"temperature,omitempty"`
	TopP          *float64    `json:"top_p,omitempty"`
	TopK          *int        `json:"top_k,omitempty"`
	Tools         []Tool      `json:"tools,omitempty"`
	ToolChoice    *ToolChoice `json:"tool_choice,omitempty"`
}

// MessagesResponse represents the response back to the client.
type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   *string        `json:"stop_reason,omitempty"`
	StopSequence *string        `json:"stop_sequence,omitempty"`
	Usage        *Usage         `json:"usage,omitempty"`
}

// Usage represents token usage.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ErrorResponse represents an error response.
type ErrorResponse struct {
	Type  string      `json:"type"`
	Error ErrorDetail `json:"error"`
}

// ErrorDetail represents the error specifics.
type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// StreamEvent represents a chunk in the streaming response.
type StreamEvent struct {
	Type         string            `json:"type"`
	Message      *MessagesResponse `json:"message,omitempty"`
	Index        *int              `json:"index,omitempty"`
	ContentBlock *ContentBlock     `json:"content_block,omitempty"`
	Delta        *Delta            `json:"delta,omitempty"`
	Usage        *Usage            `json:"usage,omitempty"`
	Error        *ErrorDetail      `json:"error,omitempty"`
}

// Delta represents a delta inside a stream event.
type Delta struct {
	Type         string  `json:"type"`
	Text         string  `json:"text,omitempty"`
	PartialJson  string  `json:"partial_json,omitempty"`
	StopReason   *string `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

// Model represents a single model object in the Anthropic API.
type Model struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// ModelList represents a list of model objects in the Anthropic API.
type ModelList struct {
	Data    []Model `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID string  `json:"first_id,omitempty"`
	LastID  string  `json:"last_id,omitempty"`
}
