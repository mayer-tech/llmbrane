package openai

import "github.com/mayer-tech/llmbrane/internal/inbound/openai/chatcompletions"

// Aliases preserve the existing OpenAI model types for callers.
type ChatCompletionRequest = chatcompletions.ChatCompletionRequest
type Message = chatcompletions.Message
type ContentPart = chatcompletions.ContentPart
type ImageURL = chatcompletions.ImageURL
type Tool = chatcompletions.Tool
type Function = chatcompletions.Function
type ToolCall = chatcompletions.ToolCall
type FunctionCall = chatcompletions.FunctionCall
type ChatCompletionResponse = chatcompletions.ChatCompletionResponse
type Choice = chatcompletions.Choice
type Usage = chatcompletions.Usage
type ChatCompletionStreamResponse = chatcompletions.ChatCompletionStreamResponse
type StreamChoice = chatcompletions.StreamChoice
type MessageDelta = chatcompletions.MessageDelta
type ToolCallDelta = chatcompletions.ToolCallDelta
type FunctionCallDelta = chatcompletions.FunctionCallDelta

// Model represents a single model object in the OpenAI API.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList represents a list of model objects in the OpenAI API.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
