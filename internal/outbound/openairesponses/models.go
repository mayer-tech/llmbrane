package openairesponses

import "encoding/json"

// ResponsesRequest is the request body for the OpenAI Responses API.
type ResponsesRequest struct {
	Model              string         `json:"model"`
	Input              []InputItem    `json:"input,omitempty"`
	Instructions       string         `json:"instructions,omitempty"`
	Tools              []Tool         `json:"tools,omitempty"`
	ToolChoice         any            `json:"tool_choice,omitempty"`
	Temperature        *float64       `json:"temperature,omitempty"`
	TopP               *float64       `json:"top_p,omitempty"`
	MaxOutputTokens    *int           `json:"max_output_tokens,omitempty"`
	Stream             bool           `json:"stream,omitempty"`
	Reasoning          *Reasoning     `json:"reasoning,omitempty"`
	Text               *TextConfig    `json:"text,omitempty"`
	Store              *bool          `json:"store,omitempty"`
	Include            []string       `json:"include,omitempty"`
	ParallelToolCalls  *bool          `json:"parallel_tool_calls,omitempty"`
	Truncation         string         `json:"truncation,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
}

// InputItem is a single item in the Responses API input array.
type InputItem struct {
	Type    string `json:"type"` // "message", "function_call", "function_call_output"
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"` // string or []ContentPart

	// function_call fields
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output fields
	Output string `json:"output,omitempty"`
}

// ContentPart is a typed content part inside a message input item.
type ContentPart struct {
	Type     string `json:"type"` // "input_text", "input_image", "output_text"
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Tool is a function tool definition for the Responses API.
type Tool struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

// Reasoning configures reasoning effort for the Responses API.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// TextConfig configures text output formatting (e.g. JSON schema).
type TextConfig struct {
	Format *TextFormat `json:"format,omitempty"`
}

// TextFormat describes a structured output format.
type TextFormat struct {
	Type   string         `json:"type"` // "text", "json_object", "json_schema"
	Name   string         `json:"name,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
	Strict *bool          `json:"strict,omitempty"`
}

// ResponsesResponse is the non-streaming response from the Responses API.
type ResponsesResponse struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	CreatedAt         int64          `json:"created_at"`
	Status            string         `json:"status"`
	Model             string         `json:"model"`
	Output            []OutputItem   `json:"output"`
	Usage             *ResponseUsage `json:"usage,omitempty"`
	Error             *ResponseError `json:"error,omitempty"`
	IncompleteDetails any            `json:"incomplete_details,omitempty"`
}

// OutputItem is a single item in the response output array.
type OutputItem struct {
	Type    string              `json:"type"` // "message", "function_call", "reasoning"
	ID      string              `json:"id,omitempty"`
	Status  string              `json:"status,omitempty"`
	Role    string              `json:"role,omitempty"`
	Content []OutputContentPart `json:"content,omitempty"`

	// function_call fields
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// reasoning fields
	Summary []ReasoningSummary `json:"summary,omitempty"`
}

// OutputContentPart is a content part inside a message output item.
type OutputContentPart struct {
	Type        string `json:"type"` // "output_text", "refusal"
	Text        string `json:"text,omitempty"`
	Annotations []any  `json:"annotations,omitempty"`
}

// ReasoningSummary is a summary part of a reasoning output item.
type ReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResponseUsage tracks token usage in the Responses API.
type ResponseUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
	TotalTokens int `json:"total_tokens"`
}

// ResponseError is an error object embedded in a response.
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse is the top-level error envelope returned on HTTP errors.
type ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// StreamEvent is a single SSE event from the Responses API stream.
type StreamEvent struct {
	Type string `json:"type"`

	// response.created / response.completed / response.failed / response.incomplete
	Response *ResponsesResponse `json:"response,omitempty"`

	// response.output_item.added / response.output_item.done
	OutputIndex int         `json:"output_index,omitempty"`
	Item        *OutputItem `json:"item,omitempty"`

	// response.content_part.added / response.content_part.done
	ItemID       string             `json:"item_id,omitempty"`
	ContentIndex int                `json:"content_index,omitempty"`
	Part         *OutputContentPart `json:"part,omitempty"`

	// response.output_text.delta / response.output_text.done
	Delta string `json:"delta,omitempty"`
	Text  string `json:"text,omitempty"`

	// response.function_call_arguments.delta / response.function_call_arguments.done
	Arguments string `json:"arguments,omitempty"`

	// response.reasoning_summary_text.delta
	// (reuses Delta field)

	// error event (nested error object as sent by the API)
	Error *StreamErrorDetail `json:"error,omitempty"`

	// error event (flat fields, kept for compatibility)
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// StreamErrorDetail is the nested error object inside an "error" stream event.
type StreamErrorDetail struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   any    `json:"param,omitempty"`
}

// MarshalJSON for InputItem handles the content field which can be a string or []ContentPart.
var _ = json.Marshal // ensure encoding/json is used
