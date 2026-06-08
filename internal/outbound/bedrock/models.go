package bedrock

// ConverseRequest represents the JSON payload for the Bedrock Converse API.
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html
type ConverseRequest struct {
	Messages        []Message        `json:"messages"`
	System          []SystemContent  `json:"system,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *ToolConfig      `json:"toolConfig,omitempty"`
}

type Message struct {
	Role    string    `json:"role"`
	Content []Content `json:"content"`
}

type Content struct {
	Text       string      `json:"text,omitempty"`
	ToolUse    *ToolUse    `json:"toolUse,omitempty"`
	ToolResult *ToolResult `json:"toolResult,omitempty"`
}

type ToolUse struct {
	ToolUseId string         `json:"toolUseId"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
}

type ToolResult struct {
	ToolUseId string              `json:"toolUseId"`
	Content   []ToolResultContent `json:"content"`
	Status    string              `json:"status"`
}

type ToolResultContent struct {
	Text string `json:"text,omitempty"`
	JSON any    `json:"json,omitempty"`
}

type SystemContent struct {
	Text string `json:"text"`
}

type InferenceConfig struct {
	MaxTokens     int      `json:"maxTokens,omitempty"`
	Temperature   *float32 `json:"temperature,omitempty"`
	TopP          *float32 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type ToolConfig struct {
	Tools      []Tool      `json:"tools"`
	ToolChoice *ToolChoice `json:"toolChoice,omitempty"`
}

type Tool struct {
	ToolSpec ToolSpec `json:"toolSpec"`
}

type ToolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	JSON map[string]any `json:"json"`
}

type ToolChoice struct {
	Auto *struct{}           `json:"auto,omitempty"`
	Any  *struct{}           `json:"any,omitempty"`
	Tool *SpecificToolChoice `json:"tool,omitempty"`
}

type SpecificToolChoice struct {
	Name string `json:"name"`
}

// ConverseResponse represents the non-streaming JSON response.
type ConverseResponse struct {
	Output struct {
		Message Message `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
		TotalTokens  int `json:"totalTokens"`
	} `json:"usage"`
}
