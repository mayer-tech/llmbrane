package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

// HandleResponses accepts stateless native Responses requests on Responses routes.
func (h *Handler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON request object", "invalid_request")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeError(w, http.StatusBadRequest, "Unexpected data after request object", "invalid_request")
		return
	}
	req, err := translateResponsesRequest(fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	route, err := h.gateway.ResolveRoute(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	if route.APIShape != "openai-responses" {
		writeError(w, http.StatusBadRequest, "native Responses requests require an openai-responses backend", "invalid_request")
		return
	}
	ctx := withConversationID(r)
	if !req.Stream {
		response, err := h.gateway.ProcessChat(ctx, req)
		if err != nil {
			writeGatewayError(w, err)
			return
		}
		if response == nil || response.Native == nil || response.Native.Provider != "openai" || response.Native.Format != "responses.response" || !json.Valid(response.Native.Data) {
			writeError(w, http.StatusBadGateway, "Provider returned an invalid native Responses response", "provider_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response.Native.Data)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming unsupported by client", "internal_error")
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := h.gateway.ProcessChatStream(ctx, req)
	if err != nil {
		writeGatewayError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	terminal := false
	nextSequence := 0
	for event := range stream {
		if event == nil {
			continue
		}
		if event.Delta != nil && event.Delta.Native != nil && event.Delta.Native.Provider == "openai" && event.Delta.Native.Format == "responses.event" {
			var payload struct {
				Type     string `json:"type"`
				Sequence *int   `json:"sequence_number"`
			}
			data := event.Delta.Native.Data
			if json.Unmarshal(data, &payload) != nil || !safeResponsesEventType(payload.Type) {
				writeResponsesStreamError(w, flusher, nextSequence, "Invalid provider stream event")
				return
			}
			if payload.Sequence != nil && *payload.Sequence >= nextSequence {
				nextSequence = *payload.Sequence + 1
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", payload.Type, compactJSON(data)); err != nil {
				return
			}
			flusher.Flush()
			if event.Type == "done" || event.Type == "error" {
				terminal = true
				return
			}
		} else if event.Type == "error" {
			message := "Provider stream failed"
			if event.Error != nil {
				message = event.Error.Message
			}
			writeResponsesStreamError(w, flusher, nextSequence, message)
			return
		} else {
			writeResponsesStreamError(w, flusher, nextSequence, "Provider returned a non-native stream event")
			return
		}
	}
	if !terminal && ctx.Err() == nil {
		writeResponsesStreamError(w, flusher, nextSequence, "Provider stream ended before a terminal event")
	}
}

func safeResponsesEventType(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_') {
			return false
		}
	}
	return true
}

func compactJSON(data []byte) []byte {
	var b bytes.Buffer
	if json.Compact(&b, data) != nil {
		return data
	}
	return b.Bytes()
}

func writeResponsesStreamError(w http.ResponseWriter, flusher http.Flusher, sequence int, message string) {
	data, _ := json.Marshal(map[string]any{"type": "error", "sequence_number": sequence, "code": "provider_error", "message": message, "param": nil})
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
	flusher.Flush()
}

func translateResponsesRequest(fields map[string]json.RawMessage) (*canonical.CanonicalChatRequest, error) {
	allowed := map[string]bool{"model": true, "input": true, "instructions": true, "tools": true, "tool_choice": true, "temperature": true, "top_p": true, "max_output_tokens": true, "stream": true, "reasoning": true, "text": true, "store": true, "include": true, "parallel_tool_calls": true, "truncation": true, "metadata": true}
	for key := range fields {
		if !allowed[key] {
			return nil, fmt.Errorf("unsupported Responses request field %q", key)
		}
	}
	req := &canonical.CanonicalChatRequest{Responses: &canonical.CanonicalResponsesRequest{Settings: make(map[string]json.RawMessage)}}
	if json.Unmarshal(fields["model"], &req.Model) != nil || req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	for _, key := range []string{"stream", "store"} {
		if raw, ok := fields[key]; ok {
			var value bool
			if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
				return nil, fmt.Errorf("%s must be a boolean", key)
			}
			if key == "store" && value {
				return nil, fmt.Errorf("store must be false for stateless Responses")
			}
			if key == "stream" {
				req.Stream = value
			}
		}
	}
	for _, key := range []string{"temperature", "top_p"} {
		if raw, ok := fields[key]; ok {
			var value float64
			if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
				return nil, fmt.Errorf("%s must be a number", key)
			}
			if key == "temperature" {
				req.Params.Temperature = &value
			} else {
				req.Params.TopP = &value
			}
		}
	}
	if raw, ok := fields["max_output_tokens"]; ok {
		var value int
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil || value <= 0 {
			return nil, fmt.Errorf("max_output_tokens must be a positive integer")
		}
		req.Params.MaxOutputTokens = &value
	}
	if raw, ok := fields["instructions"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil || bytes.Equal(raw, []byte("null")) {
			return nil, fmt.Errorf("instructions must be a string")
		}
		req.System = []canonical.CanonicalContentPart{{Type: "text", Text: value}}
	}
	if raw, ok := fields["reasoning"]; ok {
		var value struct {
			Effort *string `json:"effort"`
		}
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return nil, fmt.Errorf("reasoning must be an object")
		}
		if value.Effort != nil && !canonical.IsValidReasoningEffort(*value.Effort) {
			return nil, fmt.Errorf("unsupported reasoning effort %q", *value.Effort)
		}
		req.Params.ReasoningEffort = value.Effort
	}
	if raw, ok := fields["tools"]; ok {
		var tools []struct {
			Type        string         `json:"type"`
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		}
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &tools) != nil {
			return nil, fmt.Errorf("tools must be an array of function tools")
		}
		for _, tool := range tools {
			if tool.Type != "function" || tool.Name == "" {
				return nil, fmt.Errorf("only named function tools are supported")
			}
			req.Tools = append(req.Tools, canonical.CanonicalTool{Type: tool.Type, Name: tool.Name, Description: tool.Description, InputSchema: tool.Parameters})
		}
	}
	if raw, ok := fields["tool_choice"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && (s == "none" || s == "auto" || s == "required") {
			req.ToolChoice = &canonical.CanonicalToolChoice{Type: s}
		} else {
			var choice struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &choice) != nil || choice.Type != "function" || choice.Name == "" {
				return nil, fmt.Errorf("unsupported tool_choice")
			}
			req.ToolChoice = &canonical.CanonicalToolChoice{Type: "function", Name: choice.Name}
		}
	}
	if raw, ok := fields["text"]; ok {
		var text struct {
			Format *struct {
				Type   string         `json:"type"`
				Schema map[string]any `json:"schema"`
				Strict bool           `json:"strict"`
			} `json:"format"`
		}
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &text) != nil {
			return nil, fmt.Errorf("text must be an object")
		}
		if text.Format != nil {
			if text.Format.Type != "text" && text.Format.Type != "json_object" && text.Format.Type != "json_schema" {
				return nil, fmt.Errorf("unsupported text format")
			}
			if text.Format.Type != "text" {
				req.ResponseFormat = &canonical.CanonicalResponseFormat{Type: text.Format.Type, JSONSchema: text.Format.Schema, Strict: text.Format.Strict}
			}
		}
	}
	if raw, ok := fields["parallel_tool_calls"]; ok {
		var value bool
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return nil, fmt.Errorf("parallel_tool_calls must be a boolean")
		}
	}
	if raw, ok := fields["truncation"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil || (value != "auto" && value != "disabled") {
			return nil, fmt.Errorf("truncation must be auto or disabled")
		}
	}
	if raw, ok := fields["metadata"]; ok {
		var value map[string]string
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return nil, fmt.Errorf("metadata must be an object of strings")
		}
	}
	if raw, ok := fields["include"]; ok {
		var include []string
		if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &include) != nil {
			return nil, fmt.Errorf("include must be an array")
		}
		for _, item := range include {
			if item != "reasoning.encrypted_content" {
				return nil, fmt.Errorf("unsupported include %q", item)
			}
		}
	}
	for _, key := range []string{"instructions", "tools", "tool_choice", "reasoning", "text", "include", "parallel_tool_calls", "truncation", "metadata"} {
		if raw, ok := fields[key]; ok {
			req.Responses.Settings[key] = raw
		}
	}
	input, ok := fields["input"]
	if !ok || bytes.Equal(input, []byte("null")) {
		return nil, fmt.Errorf("input is required")
	}
	var text string
	if json.Unmarshal(input, &text) == nil {
		item, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": text})
		input = append(append([]byte("["), item...), ']')
	}
	var items []json.RawMessage
	if json.Unmarshal(input, &items) != nil {
		return nil, fmt.Errorf("input must be a string or an array of items")
	}
	for i, raw := range items {
		message, err := responsesInputItem(raw)
		if err != nil {
			return nil, fmt.Errorf("input item %d: %w", i, err)
		}
		req.Messages = append(req.Messages, message)
	}
	return req, nil
}

func responsesInputItem(raw json.RawMessage) (canonical.CanonicalMessage, error) {
	var item struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments *string         `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return canonical.CanonicalMessage{}, fmt.Errorf("expected an item object")
	}
	msg := canonical.CanonicalMessage{Role: canonical.RoleAssistant}
	switch item.Type {
	case "", "message":
		switch item.Role {
		case "user", "assistant", "system", "developer":
		default:
			return msg, fmt.Errorf("unsupported message role %q", item.Role)
		}
		msg.Role = canonical.CanonicalRole(item.Role)
		var text string
		if json.Unmarshal(item.Content, &text) == nil && !bytes.Equal(item.Content, []byte("null")) {
			msg.Content = append(msg.Content, canonical.CanonicalContentPart{Type: "text", Text: text})
		} else {
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
				Detail   string `json:"detail"`
			}
			if len(item.Content) == 0 || bytes.Equal(item.Content, []byte("null")) || json.Unmarshal(item.Content, &parts) != nil {
				return msg, fmt.Errorf("message content must be a string or array")
			}
			for _, part := range parts {
				switch part.Type {
				case "input_text", "output_text":
					msg.Content = append(msg.Content, canonical.CanonicalContentPart{Type: "text", Text: part.Text})
				case "refusal": // Preserved in the native item.
				case "input_image":
					if part.ImageURL == "" {
						return msg, fmt.Errorf("input_image requires image_url")
					}
					msg.Content = append(msg.Content, canonical.CanonicalContentPart{Type: "image", Image: &canonical.CanonicalImage{URL: part.ImageURL, Detail: part.Detail}})
				default:
					return msg, fmt.Errorf("unsupported message content type %q", part.Type)
				}
			}
		}
	case "reasoning":
	case "function_call":
		if item.CallID == "" || item.Name == "" || item.Arguments == nil {
			return msg, fmt.Errorf("function_call requires call_id, name and arguments")
		}
	case "function_call_output":
		var output string
		if item.CallID == "" || json.Unmarshal(item.Output, &output) != nil || bytes.Equal(item.Output, []byte("null")) {
			return msg, fmt.Errorf("function_call_output requires call_id and string output")
		}
		msg.Role = canonical.RoleTool
	default:
		return msg, fmt.Errorf("unsupported input item type %q", item.Type)
	}
	msg.Content = append(msg.Content, canonical.CanonicalContentPart{Type: "native", Native: &canonical.CanonicalProviderData{Provider: "openai", Format: "responses.item", Data: append(json.RawMessage(nil), raw...)}})
	return msg, nil
}
