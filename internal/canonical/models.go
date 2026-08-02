package canonical

import "encoding/json"

// CanonicalRole represents the role of a message sender.
type CanonicalRole string

const (
	RoleSystem    CanonicalRole = "system"
	RoleUser      CanonicalRole = "user"
	RoleAssistant CanonicalRole = "assistant"
	RoleTool      CanonicalRole = "tool"
)

// CanonicalChatRequest represents a unified chat request decoupled from specific provider implementations.
type CanonicalChatRequest struct {
	Model          string                   `json:"model"`
	Messages       []CanonicalMessage       `json:"messages"`
	System         []CanonicalContentPart   `json:"system,omitempty"`
	Tools          []CanonicalTool          `json:"tools,omitempty"`
	ToolChoice     *CanonicalToolChoice     `json:"tool_choice,omitempty"`
	ResponseFormat *CanonicalResponseFormat `json:"response_format,omitempty"`
	Stream         bool                     `json:"stream,omitempty"`
	Params         CanonicalParams          `json:"params,omitempty"`
	Extensions     map[string]any           `json:"extensions,omitempty"`
	Raw            []byte                   `json:"-"`
}

// CanonicalMessage represents a single message in a chat request or response.
type CanonicalMessage struct {
	Role       CanonicalRole          `json:"role"`
	Name       string                 `json:"name,omitempty"`
	Content    []CanonicalContentPart `json:"content,omitempty"`
	ToolCallID string                 `json:"tool_call_id,omitempty"`
	Metadata   map[string]any         `json:"metadata,omitempty"`
}

// CanonicalContentPart represents a heterogeneous content part (e.g., text, image, tool calls).
type CanonicalContentPart struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	Image *CanonicalImage `json:"image,omitempty"`
	Audio *CanonicalAudio `json:"audio,omitempty"`
	File  *CanonicalFile  `json:"file,omitempty"`

	ToolCall   *CanonicalToolCall   `json:"tool_call,omitempty"`
	ToolResult *CanonicalToolResult `json:"tool_result,omitempty"`
	Reasoning  *CanonicalReasoning  `json:"reasoning,omitempty"`

	Data map[string]any `json:"data,omitempty"`
	Raw  []byte         `json:"-"`
}

// CanonicalImage represents an image content part.
type CanonicalImage struct {
	URL      string `json:"url,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// CanonicalAudio represents an audio content part.
type CanonicalAudio struct {
	Data     []byte `json:"data,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

// CanonicalFile represents a file content part.
type CanonicalFile struct {
	Data     []byte `json:"data,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

// CanonicalReasoning represents reasoning content from the model.
type CanonicalReasoning struct {
	Content string `json:"content"`
}

// CanonicalTool defines a tool/function available to the model.
type CanonicalTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// CanonicalToolChoice specifies how tools should be used by the model.
type CanonicalToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// CanonicalToolCall represents a requested tool invocation by the model.
type CanonicalToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

// CanonicalToolResult represents the outcome of a tool invocation.
type CanonicalToolResult struct {
	ToolCallID string                 `json:"tool_call_id"`
	Content    []CanonicalContentPart `json:"content,omitempty"`
	IsError    bool                   `json:"is_error,omitempty"`
}

// CanonicalParams contains parameters governing the LLM text generation.
type CanonicalParams struct {
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	MaxOutputTokens  *int           `json:"max_output_tokens,omitempty"`
	Stop             []string       `json:"stop,omitempty"`
	Seed             *int64         `json:"seed,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	Logprobs         *bool          `json:"logprobs,omitempty"`
	TopLogprobs      *int           `json:"top_logprobs,omitempty"`
	ReasoningEffort  *string        `json:"reasoning_effort,omitempty"`
	ProviderSpecific map[string]any `json:"provider_specific,omitempty"`
}

// CanonicalResponseFormat requests a specific structured output format.
type CanonicalResponseFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
	Strict     bool           `json:"strict,omitempty"`
}

// CanonicalChatResponse represents a unified chat response decoupled from specific implementations.
type CanonicalChatResponse struct {
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Choices []CanonicalChoice `json:"choices"`
	Usage   *CanonicalUsage   `json:"usage,omitempty"`
	Stop    *CanonicalStop    `json:"stop,omitempty"`
	Raw     []byte            `json:"-"`
}

// CanonicalChoice represents one generated response option.
type CanonicalChoice struct {
	Index        int              `json:"index"`
	Message      CanonicalMessage `json:"message"`
	FinishReason string           `json:"finish_reason,omitempty"`
}

// CanonicalUsage tracks token usage for a request/response.
type CanonicalUsage struct {
	InputTokens       int            `json:"input_tokens"`
	OutputTokens      int            `json:"output_tokens"`
	TotalTokens       int            `json:"total_tokens"`
	CachedInputTokens int            `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   int            `json:"reasoning_tokens,omitempty"`
	ProviderSpecific  map[string]any `json:"provider_specific,omitempty"`
}

// CanonicalStop details why generation stopped.
type CanonicalStop struct {
	Reason string `json:"reason"`
}

// CanonicalChatStreamEvent represents a single event in a streaming response.
type CanonicalChatStreamEvent struct {
	Type string `json:"type"`

	MessageIndex int `json:"message_index,omitempty"`
	ContentIndex int `json:"content_index,omitempty"`

	Delta    *CanonicalContentPart   `json:"delta,omitempty"`
	ToolCall *CanonicalToolCallDelta `json:"tool_call,omitempty"`
	Usage    *CanonicalUsage         `json:"usage,omitempty"`
	Stop     *CanonicalStop          `json:"stop,omitempty"`
	Error    *GatewayError           `json:"error,omitempty"`

	Raw []byte `json:"-"`
}

// CanonicalToolCallDelta represents a streaming update to a tool call argument string.
type CanonicalToolCallDelta struct {
	ID             string `json:"id,omitempty"`
	Name           string `json:"name,omitempty"`
	ArgumentsDelta string `json:"arguments_delta,omitempty"`
	Index          int    `json:"index"`
}
