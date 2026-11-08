package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/core"
)

// Handler handles incoming Anthropic-compatible requests.
type Handler struct {
	gateway *core.Gateway
}

// NewHandler creates a new Anthropic inbound handler.
func NewHandler(gateway *core.Gateway) *Handler {
	return &Handler{
		gateway: gateway,
	}
}

// RegisterRoutes registers the Anthropic inbound routes on the provided ServeMux with the given prefix.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("POST "+prefix+"/v1/messages", h.HandleMessages)
	mux.HandleFunc("GET "+prefix+"/v1/models", h.HandleListModels)
}

// HandleListModels processes GET /v1/models.
func (h *Handler) HandleListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}

	configuredModels := h.gateway.ConfiguredModels()

	models := make([]Model, 0, len(configuredModels))
	for _, m := range configuredModels {
		models = append(models, Model{
			Type:        "model",
			ID:          m,
			DisplayName: m,
			CreatedAt:   "2024-01-01T00:00:00Z", // Mocked, anthropic models API requires created_at string
		})
	}

	firstID := ""
	lastID := ""
	if len(models) > 0 {
		firstID = models[0].ID
		lastID = models[len(models)-1].ID
	}

	res := ModelList{
		Data:    models,
		HasMore: false,
		FirstID: firstID,
		LastID:  lastID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// HandleMessages processes POST /v1/messages.
func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		return
	}
	if !limitMessagesBody(w, r) {
		return
	}

	var raw json.RawMessage
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error")
		return
	}
	var req MessagesRequest
	requestDecoder := json.NewDecoder(bytes.NewReader(raw))
	requestDecoder.UseNumber()
	if err := requestDecoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error(), "invalid_request_error")
		return
	}
	canonicalReq, err := h.translateRequest(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Failed to translate request: "+err.Error(), "invalid_request_error")
		return
	}
	if h.gateway != nil {
		if route, err := h.gateway.ResolveRoute(req.Model); err == nil && route.APIShape == "openai-responses" {
			var trailing any
			if err := decoder.Decode(&trailing); err != io.EOF {
				writeError(w, http.StatusBadRequest, "Invalid JSON payload: trailing data", "invalid_request_error")
				return
			}
			if err := validateResponsesInputFields(raw); err != nil {
				writeError(w, http.StatusBadRequest, "Failed to translate request: "+err.Error(), "invalid_request_error")
				return
			}
			if err := validateResponsesRequest(&req); err != nil {
				writeError(w, http.StatusBadRequest, "Failed to translate request: "+err.Error(), "invalid_request_error")
				return
			}
			if err := applyResponsesInputOptions(&req, canonicalReq); err != nil {
				writeError(w, http.StatusBadRequest, "Failed to translate request: "+err.Error(), "invalid_request_error")
				return
			}
		}
	}
	// A decoded artifact must never be sent to a different provider as a native block.
	for _, message := range canonicalReq.Messages {
		for _, part := range message.Content {
			if part.Native == nil || part.Native.Provider != "openai" || part.Native.Format != "responses.item" {
				continue
			}
			route, routeErr := h.gateway.ResolveRoute(canonicalReq.Model)
			if routeErr != nil || route.APIShape != "openai-responses" {
				writeError(w, http.StatusBadRequest, "OpenAI reasoning replay requires an OpenAI Responses backend", "invalid_request_error")
				return
			}
		}
	}
	ctx, cancel := context.WithCancel(withConversationID(r))
	defer cancel()

	if req.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "Streaming unsupported by client", "api_error")
			return
		}
		stream, err := h.gateway.ProcessChatStream(ctx, canonicalReq)
		if err != nil {
			writeGatewayError(w, err)
			return
		}
		// Some gateway wrappers use blocking sends. Cancel the provider and drain
		// the wrapper if the client disconnects or the handler exits early.
		defer func() {
			cancel()
			go func() {
				for range stream {
				}
			}()
		}()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		started := false
		for event := range stream {
			if event == nil {
				continue
			}
			if event.Error != nil || event.Type == "error" {
				streamErr := event.Error
				if streamErr == nil {
					streamErr = canonical.NewGatewayError("", "", "provider stream reported an unspecified error", http.StatusBadGateway, nil)
				}
				if !started {
					w.Header().Del("Cache-Control")
					w.Header().Del("Connection")
					writeGatewayError(w, streamErr)
					return
				}
				errEvent := StreamEvent{
					Type: "error",
					Error: &ErrorDetail{
						Type:    anthropicErrorType(streamErr),
						Message: streamErr.Message,
					},
				}
				data, _ := json.Marshal(errEvent)
				w.Write([]byte("event: error\n"))
				w.Write([]byte("data: "))
				w.Write(data)
				w.Write([]byte("\n\n"))
				flusher.Flush()
				break
			}

			chunks := h.translateStreamEvent(event, req.Model)
			for _, chunk := range chunks {
				data, _ := json.Marshal(chunk)
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", chunk.Type, data); err != nil {
					return
				}
				started = true
				flusher.Flush()
			}
		}
		return
	}

	canonicalRes, err := h.gateway.ProcessChat(ctx, canonicalReq)
	if err != nil {
		writeGatewayError(w, err)
		return
	}

	res, err := h.translateResponse(canonicalRes)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Failed to translate response: "+err.Error(), "api_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// responsesObject checks exact field names and rejects duplicates instead of
// allowing encoding/json to silently ignore or overwrite provider settings.
func responsesObject(raw json.RawMessage, path string, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%s must be an object", path)
	}
	fields := make(map[string]json.RawMessage)
	valid := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		valid[name] = true
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid %s", path)
		}
		name, ok := token.(string)
		if !ok || !valid[name] {
			return nil, fmt.Errorf("unsupported %s field %q", path, name)
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate %s field %q", path, name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("invalid %s.%s", path, name)
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("invalid %s", path)
	}
	return fields, nil
}

// validateResponsesInputFields prevents unknown Anthropic settings or block
// variants from being mistaken for ordinary text or function tools.
func validateResponsesInputFields(raw json.RawMessage) error {
	fields, err := responsesObject(raw, "request", "model", "messages", "system", "max_tokens", "stop_sequences", "stream", "temperature", "top_p", "top_k", "tools", "tool_choice", "thinking", "output_config")
	if err != nil {
		return err
	}
	for _, entry := range []struct {
		name    string
		allowed []string
	}{
		{"tool_choice", []string{"type", "name", "disable_parallel_tool_use"}},
		{"thinking", []string{"type", "budget_tokens", "display"}},
		{"output_config", []string{"effort", "format"}},
	} {
		if value, ok := fields[entry.name]; ok && string(value) != "null" {
			object, err := responsesObject(value, entry.name, entry.allowed...)
			if err != nil {
				return err
			}
			if entry.name == "thinking" {
				if display, ok := object["display"]; ok {
					var mode string
					if json.Unmarshal(display, &mode) != nil || mode != "summarized" {
						return fmt.Errorf("thinking.display must be summarized on the OpenAI Responses route")
					}
				}
			}
		}
	}
	if rawTools, ok := fields["tools"]; ok && string(rawTools) != "null" {
		var tools []json.RawMessage
		if err := json.Unmarshal(rawTools, &tools); err != nil {
			return fmt.Errorf("invalid tools")
		}
		for _, tool := range tools {
			obj, err := responsesObject(tool, "tool", "type", "name", "description", "input_schema", "strict")
			if err != nil {
				return err
			}
			if rawType, ok := obj["type"]; ok {
				var kind string
				if json.Unmarshal(rawType, &kind) != nil || kind != "custom" {
					return fmt.Errorf("unsupported tool type %s", rawType)
				}
			}
		}
	}
	if rawMessages, ok := fields["messages"]; ok {
		var messages []json.RawMessage
		if err := json.Unmarshal(rawMessages, &messages); err != nil {
			return fmt.Errorf("invalid messages")
		}
		for _, message := range messages {
			obj, err := responsesObject(message, "message", "role", "content")
			if err != nil {
				return err
			}
			if err := validateResponsesContentFields(obj["content"], false); err != nil {
				return err
			}
		}
	}
	if system, ok := fields["system"]; ok && string(system) != "null" {
		if err := validateResponsesContentFields(system, true); err != nil {
			return err
		}
	}
	return nil
}

func validateResponsesContentFields(raw json.RawMessage, system bool) error {
	if len(raw) == 0 || raw[0] != '[' {
		return nil // Value types are checked by validateResponsesRequest.
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return fmt.Errorf("invalid content blocks")
	}
	for _, block := range blocks {
		// Select the field allowlist only after reading the type, then check
		// the object again to detect duplicate and unknown fields.
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(block, &header); err != nil {
			return fmt.Errorf("invalid content block")
		}
		var allowed []string
		switch header.Type {
		case "text":
			allowed = []string{"type", "text"}
		case "thinking":
			allowed = []string{"type", "thinking", "signature"}
		case "redacted_thinking":
			allowed = []string{"type", "data"}
		case "image":
			allowed = []string{"type", "source"}
		case "tool_use":
			allowed = []string{"type", "id", "name", "input"}
		case "tool_result":
			allowed = []string{"type", "tool_use_id", "content", "is_error"}
		default:
			return fmt.Errorf("unsupported content block %q", header.Type)
		}
		if system && header.Type != "text" {
			return fmt.Errorf("unsupported system block %q", header.Type)
		}
		obj, err := responsesObject(block, "content block", allowed...)
		if err != nil {
			return err
		}
		if source, ok := obj["source"]; ok {
			var h struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(source, &h) != nil {
				return fmt.Errorf("invalid image source")
			}
			var sourceFields []string
			switch h.Type {
			case "base64":
				sourceFields = []string{"type", "media_type", "data"}
			case "url":
				sourceFields = []string{"type", "url"}
			default:
				return fmt.Errorf("unsupported image source %q", h.Type)
			}
			if _, err := responsesObject(source, "image source", sourceFields...); err != nil {
				return err
			}
		}
		if content, ok := obj["content"]; ok {
			if err := validateResponsesContentFields(content, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyResponsesInputOptions applies options specific to Responses routes.
func applyResponsesInputOptions(req *MessagesRequest, translated *canonical.CanonicalChatRequest) error {
	if req.OutputConfig != nil && len(req.OutputConfig.Format) != 0 {
		return fmt.Errorf("output_config.format is not supported by OpenAI Responses translation")
	}
	if req.ToolChoice != nil && len(req.ToolChoice.DisableParallelToolUse) != 0 {
		raw := string(req.ToolChoice.DisableParallelToolUse)
		if raw != "true" && raw != "false" {
			return fmt.Errorf("tool_choice.disable_parallel_tool_use must be a boolean")
		}
		translated.Params.ProviderSpecific["llmbrane.anthropic_disable_parallel_tool_use"] = raw == "true"
	}
	for i, tool := range req.Tools {
		strict := false // Responses defaults to strict; Anthropic does not.
		if len(tool.Strict) != 0 {
			raw := string(tool.Strict)
			if raw != "true" && raw != "false" {
				return fmt.Errorf("tools[%d].strict must be a boolean", i)
			}
			strict = raw == "true"
		}
		translated.Tools[i].Metadata = map[string]any{"llmbrane.anthropic_strict": strict}
	}
	return nil
}

// validateResponsesRequest rejects blocks that the Responses translation cannot
// represent instead of silently dropping them. Native Anthropic routes retain
// their existing handling.
func validateResponsesRequest(req *MessagesRequest) error {
	if len(req.Messages) == 0 {
		return fmt.Errorf("messages must not be empty")
	}
	if req.MaxTokens != nil && *req.MaxTokens <= 0 {
		return fmt.Errorf("max_tokens must be positive")
	}
	if req.TopK != nil || len(req.StopSequences) != 0 {
		return fmt.Errorf("top_k and stop_sequences are not supported by OpenAI Responses")
	}
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("unsupported message role %q", m.Role)
		}
		switch content := m.Content.(type) {
		case string:
		case []any:
			if len(content) == 0 {
				return fmt.Errorf("message content must not be empty")
			}
			for _, value := range content {
				block, ok := value.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid content block")
				}
				kind, _ := block["type"].(string)
				switch kind {
				case "text":
					if _, ok := block["text"].(string); !ok {
						return fmt.Errorf("invalid text block")
					}
				case "thinking", "redacted_thinking":
					if m.Role != "assistant" {
						return fmt.Errorf("reasoning requires an assistant message")
					}
				case "image":
					if m.Role != "user" {
						return fmt.Errorf("image requires a user message")
					}
					source, ok := block["source"].(map[string]any)
					if !ok {
						return fmt.Errorf("invalid image source")
					}
					sourceType, _ := source["type"].(string)
					switch sourceType {
					case "base64":
						media, mok := source["media_type"].(string)
						data, dok := source["data"].(string)
						if !mok || media == "" || !dok || data == "" {
							return fmt.Errorf("invalid base64 image")
						}
					case "url":
						url, ok := source["url"].(string)
						if !ok || url == "" {
							return fmt.Errorf("invalid image URL")
						}
					default:
						return fmt.Errorf("unsupported image source")
					}
				case "tool_use":
					id, iok := block["id"].(string)
					name, nok := block["name"].(string)
					_, inputOK := block["input"].(map[string]any)
					if m.Role != "assistant" || !iok || id == "" || !nok || name == "" || !inputOK {
						return fmt.Errorf("invalid tool_use block")
					}
				case "tool_result":
					id, ok := block["tool_use_id"].(string)
					if m.Role != "user" || !ok || id == "" {
						return fmt.Errorf("invalid tool_result block")
					}
					if v, present := block["is_error"]; present {
						if _, ok := v.(bool); !ok {
							return fmt.Errorf("invalid tool_result is_error")
						}
					}
					if value, present := block["content"]; present {
						switch result := value.(type) {
						case string:
						case []any:
							for _, value := range result {
								part, ok := value.(map[string]any)
								if !ok {
									return fmt.Errorf("unsupported tool_result content")
								}
								kind, _ := part["type"].(string)
								if kind != "text" {
									return fmt.Errorf("unsupported tool_result content")
								}
								if _, ok := part["text"].(string); !ok {
									return fmt.Errorf("invalid tool_result text")
								}
							}
						default:
							return fmt.Errorf("unsupported tool_result content")
						}
					}
				default:
					return fmt.Errorf("unsupported content block %q", kind)
				}
			}
		default:
			return fmt.Errorf("invalid message content")
		}
	}
	if req.System != nil {
		switch system := req.System.(type) {
		case string:
		case []any:
			for _, value := range system {
				part, ok := value.(map[string]any)
				if !ok {
					return fmt.Errorf("unsupported system block")
				}
				kind, _ := part["type"].(string)
				if kind != "text" {
					return fmt.Errorf("unsupported system block")
				}
				if _, ok := part["text"].(string); !ok {
					return fmt.Errorf("invalid system text")
				}
			}
		default:
			return fmt.Errorf("invalid system content")
		}
	}
	for _, tool := range req.Tools {
		var schema map[string]json.RawMessage
		if tool.Name == "" || json.Unmarshal(tool.InputSchema, &schema) != nil || schema == nil {
			return fmt.Errorf("invalid tool definition")
		}
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto", "any", "none":
			if req.ToolChoice.Name != "" {
				return fmt.Errorf("tool choice %q does not accept a name", req.ToolChoice.Type)
			}
		case "tool":
			if req.ToolChoice.Name == "" {
				return fmt.Errorf("tool choice requires a name")
			}
		default:
			return fmt.Errorf("unsupported tool choice %q", req.ToolChoice.Type)
		}
	}
	return nil
}

func withConversationID(r *http.Request) context.Context {
	if id := canonical.ConversationIDFromHeaders(r.Header); id != "" {
		return canonical.WithConversationID(r.Context(), id)
	}
	return r.Context()
}

func (h *Handler) translateRequest(req *MessagesRequest) (*canonical.CanonicalChatRequest, error) {
	var messages []canonical.CanonicalMessage
	for _, m := range req.Messages {
		var contentParts []canonical.CanonicalContentPart

		switch c := m.Content.(type) {
		case string:
			contentParts = append(contentParts, canonical.CanonicalContentPart{
				Type: "text",
				Text: c,
			})
		case []any:
			for _, item := range c {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				partType, _ := itemMap["type"].(string)
				if partType == "text" {
					text, _ := itemMap["text"].(string)
					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type: "text",
						Text: text,
					})
				} else if partType == "thinking" || partType == "redacted_thinking" {
					data, err := json.Marshal(itemMap)
					if err != nil {
						return nil, fmt.Errorf("invalid %s block: %w", partType, err)
					}
					thinking, _ := itemMap["thinking"].(string)
					native := &canonical.CanonicalProviderData{Provider: "anthropic", Format: partType, Data: data}
					value, _ := itemMap["signature"].(string)
					if partType == "redacted_thinking" {
						value, _ = itemMap["data"].(string)
					}
					if canonical.IsReasoningEnvelope(value) {
						if m.Role != "assistant" {
							return nil, fmt.Errorf("reasoning envelope requires an assistant message")
						}
						native, err = canonical.DecodeReasoningEnvelope(value)
						if err != nil {
							return nil, fmt.Errorf("invalid reasoning envelope: %w", err)
						}
						if native.Provider == "openai" && partType != "thinking" {
							return nil, fmt.Errorf("OpenAI reasoning envelope requires a thinking block")
						}
					}
					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type:      "reasoning",
						Reasoning: &canonical.CanonicalReasoning{Content: thinking},
						Native:    native,
					})
				} else if partType == "image" {
					if sourceMap, ok := itemMap["source"].(map[string]any); ok {
						sourceType, _ := sourceMap["type"].(string)
						if sourceType == "base64" {
							mediaType, _ := sourceMap["media_type"].(string)
							data, _ := sourceMap["data"].(string)
							url := fmt.Sprintf("data:%s;base64,%s", mediaType, data)
							contentParts = append(contentParts, canonical.CanonicalContentPart{
								Type: "image",
								Image: &canonical.CanonicalImage{
									URL:      url,
									MIMEType: mediaType,
								},
							})
						} else if sourceType == "url" {
							url, _ := sourceMap["url"].(string)
							contentParts = append(contentParts, canonical.CanonicalContentPart{
								Type: "image",
								Image: &canonical.CanonicalImage{
									URL: url,
								},
							})
						}
					}
				} else if partType == "tool_use" {
					id, _ := itemMap["id"].(string)
					name, _ := itemMap["name"].(string)
					inputBytes, err := json.Marshal(itemMap["input"])
					if err != nil {
						return nil, fmt.Errorf("invalid tool_use input: %w", err)
					}
					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type: "tool_call",
						ToolCall: &canonical.CanonicalToolCall{
							ID:        id,
							Name:      name,
							Arguments: inputBytes,
						},
					})
				} else if partType == "tool_result" {
					toolUseID, _ := itemMap["tool_use_id"].(string)
					isError, _ := itemMap["is_error"].(bool)

					var innerContent []canonical.CanonicalContentPart
					if contentVal, ok := itemMap["content"]; ok {
						if contentStr, ok := contentVal.(string); ok {
							innerContent = append(innerContent, canonical.CanonicalContentPart{
								Type: "text",
								Text: contentStr,
							})
						} else if contentArr, ok := contentVal.([]any); ok {
							for _, innerItem := range contentArr {
								innerMap, ok := innerItem.(map[string]any)
								if !ok {
									continue
								}
								innerType, _ := innerMap["type"].(string)
								if innerType == "text" {
									text, _ := innerMap["text"].(string)
									innerContent = append(innerContent, canonical.CanonicalContentPart{
										Type: "text",
										Text: text,
									})
								}
							}
						}
					}

					contentParts = append(contentParts, canonical.CanonicalContentPart{
						Type: "tool_result",
						ToolResult: &canonical.CanonicalToolResult{
							ToolCallID: toolUseID,
							Content:    innerContent,
							IsError:    isError,
						},
					})
				}
			}
		}

		messages = append(messages, canonical.CanonicalMessage{
			Role:    canonical.CanonicalRole(m.Role),
			Content: contentParts,
		})
	}

	var systemParts []canonical.CanonicalContentPart
	if req.System != nil {
		switch s := req.System.(type) {
		case string:
			systemParts = append(systemParts, canonical.CanonicalContentPart{
				Type: "text",
				Text: s,
			})
		case []any:
			for _, item := range s {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				partType, _ := itemMap["type"].(string)
				if partType == "text" {
					text, _ := itemMap["text"].(string)
					systemParts = append(systemParts, canonical.CanonicalContentPart{
						Type: "text",
						Text: text,
					})
				}
			}
		}
	}

	var tools []canonical.CanonicalTool
	for _, t := range req.Tools {
		var inputSchema map[string]any
		decoder := json.NewDecoder(bytes.NewReader(t.InputSchema))
		decoder.UseNumber()
		_ = decoder.Decode(&inputSchema)
		tools = append(tools, canonical.CanonicalTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			InputSchema: inputSchema,
		})
	}

	var toolChoice *canonical.CanonicalToolChoice
	if req.ToolChoice != nil {
		toolChoice = &canonical.CanonicalToolChoice{
			Type: req.ToolChoice.Type,
			Name: req.ToolChoice.Name,
		}
	}

	var reasoningEffort *string
	if req.OutputConfig != nil && req.OutputConfig.Effort != nil {
		eff := *req.OutputConfig.Effort
		switch eff {
		case "low", "medium", "high", "xhigh", "max":
			reasoningEffort = &eff
		default:
			return nil, fmt.Errorf("unsupported output_config.effort %q", eff)
		}
	}
	var thinkingDisplay string
	if req.Thinking != nil {
		if len(req.Thinking.Display) != 0 {
			if err := json.Unmarshal(req.Thinking.Display, &thinkingDisplay); err != nil || (thinkingDisplay != "summarized" && thinkingDisplay != "omitted") {
				return nil, fmt.Errorf("thinking.display must be summarized or omitted")
			}
			if req.Thinking.Type != "adaptive" && req.Thinking.Type != "enabled" {
				return nil, fmt.Errorf("thinking.display requires adaptive or enabled thinking")
			}
		}
		switch req.Thinking.Type {
		case "adaptive":
			if req.Thinking.BudgetTokens != nil {
				return nil, fmt.Errorf("thinking.budget_tokens is not supported with adaptive thinking")
			}
		case "disabled":
			if req.Thinking.BudgetTokens != nil || reasoningEffort != nil {
				return nil, fmt.Errorf("disabled thinking cannot be combined with budget_tokens or output_config.effort")
			}
			eff := "none"
			reasoningEffort = &eff
		case "enabled":
			if reasoningEffort != nil {
				return nil, fmt.Errorf("manual thinking cannot be combined with output_config.effort")
			}
			if req.Thinking.BudgetTokens == nil || *req.Thinking.BudgetTokens < 0 {
				return nil, fmt.Errorf("enabled thinking requires a non-negative budget_tokens")
			}
			b := *req.Thinking.BudgetTokens
			var eff string
			switch {
			case b == 0:
				eff = "none"
			case b <= 1024:
				eff = "minimal"
			case b <= 2048:
				eff = "low"
			case b <= 4096:
				eff = "medium"
			case b <= 8192:
				eff = "high"
			case b <= 16384:
				eff = "xhigh"
			default:
				eff = "max"
			}
			reasoningEffort = &eff
		default:
			return nil, fmt.Errorf("unsupported thinking.type %q", req.Thinking.Type)
		}
	}

	providerSpecific := map[string]any{"llmbrane.anthropic_inbound": true}
	if req.Thinking != nil && req.Thinking.Type == "adaptive" {
		providerSpecific["llmbrane.anthropic_adaptive"] = true
	}
	if thinkingDisplay != "" {
		providerSpecific["llmbrane.anthropic_thinking_display"] = thinkingDisplay
	}
	return &canonical.CanonicalChatRequest{
		Model:      req.Model,
		Messages:   messages,
		System:     systemParts,
		Tools:      tools,
		ToolChoice: toolChoice,
		Stream:     req.Stream,
		Params: canonical.CanonicalParams{
			Temperature:      req.Temperature,
			TopP:             req.TopP,
			MaxOutputTokens:  req.MaxTokens,
			Stop:             req.StopSequences,
			ReasoningEffort:  reasoningEffort,
			ProviderSpecific: providerSpecific,
		},
	}, nil
}

func (h *Handler) translateResponse(res *canonical.CanonicalChatResponse) (*MessagesResponse, error) {
	var content []ContentBlock
	if len(res.Choices) > 0 {
		msg := res.Choices[0].Message
		if msg.Content != nil {
			content = []ContentBlock{}
		}
		for _, p := range msg.Content {
			if p.Type == "text" {
				block := ContentBlock{Type: "text", Text: p.Text}
				if p.Text == "" && p.Native != nil && p.Native.Provider == "openai" && p.Native.Format == "responses.output_text" {
					block.Native = json.RawMessage(`{"type":"text","text":""}`)
				}
				content = append(content, block)
			} else if p.Type == "reasoning" && p.Native != nil && p.Native.Provider == "anthropic" && (p.Native.Format == "thinking" || p.Native.Format == "redacted_thinking") {
				content = append(content, ContentBlock{Type: p.Native.Format, Native: p.Native.Data})
			} else if p.Type == "reasoning" && p.Native != nil && p.Native.Provider == "openai" && p.Native.Format == "responses.item" {
				signature, err := canonical.EncodeReasoningEnvelope("openai", p.Native.Data)
				if err != nil {
					return nil, err
				}
				thinking := ""
				if p.Reasoning != nil {
					thinking = p.Reasoning.Content
				}
				block, err := json.Marshal(map[string]string{"type": "thinking", "thinking": thinking, "signature": signature})
				if err != nil {
					return nil, err
				}
				content = append(content, ContentBlock{Type: "thinking", Native: block})
			} else if p.Type == "tool_call" && p.ToolCall != nil {
				var inputRaw json.RawMessage = p.ToolCall.Arguments
				if len(inputRaw) == 0 {
					inputRaw = []byte(`{}`)
				}

				content = append(content, ContentBlock{
					Type:  "tool_use",
					ID:    p.ToolCall.ID,
					Name:  p.ToolCall.Name,
					Input: inputRaw,
				})
			}
		}
	}

	var stopReason *string
	var stopSequence *string
	if res.Stop != nil {
		reason := res.Stop.Reason
		if reason == "stop" {
			reason = "end_turn"
		} else if reason == "tool_calls" {
			reason = "tool_use"
		} else if reason == "length" {
			reason = "max_tokens"
		}
		stopReason = &reason
	}

	var usage *Usage
	if res.Usage != nil {
		usage = &Usage{
			InputTokens:  res.Usage.InputTokens,
			OutputTokens: res.Usage.OutputTokens,
		}
	}

	return &MessagesResponse{
		ID:           res.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        res.Model,
		Content:      content,
		StopReason:   stopReason,
		StopSequence: stopSequence,
		Usage:        usage,
	}, nil
}

func (h *Handler) translateStreamEvent(event *canonical.CanonicalChatStreamEvent, model string) []*StreamEvent {
	var events []*StreamEvent

	switch event.Type {
	case "message_start":
		id := event.ResponseID
		if id == "" {
			id = "msg_stream"
		}
		usage := &Usage{}
		if event.Usage != nil {
			usage = &Usage{InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens}
		}
		events = append(events, &StreamEvent{
			Type: "message_start",
			Message: &MessagesResponse{
				ID:      id,
				Type:    "message",
				Role:    "assistant",
				Model:   model,
				Content: []ContentBlock{},
				Usage:   usage,
			},
		})
	case "content_start":
		idx := event.ContentIndex
		if event.Delta != nil && event.Delta.Native != nil && event.Delta.Native.Provider == "anthropic" {
			events = append(events, &StreamEvent{
				Type:         "content_block_start",
				Index:        &idx,
				ContentBlock: &ContentBlock{Type: event.Delta.Native.Format, Native: event.Delta.Native.Data},
			})
		}
	case "content_stop":
		idx := event.ContentIndex
		events = append(events, &StreamEvent{Type: "content_block_stop", Index: &idx})
	case "tool_call_start":
		idx := event.ContentIndex
		if event.ToolCall != nil {
			block := &ContentBlock{
				Type:  "tool_use",
				ID:    event.ToolCall.ID,
				Name:  event.ToolCall.Name,
				Input: json.RawMessage(`{}`),
			}
			if event.Delta != nil && event.Delta.Native != nil && event.Delta.Native.Provider == "anthropic" && event.Delta.Native.Format == "tool_use" {
				block.Native = event.Delta.Native.Data
			}
			events = append(events, &StreamEvent{
				Type:         "content_block_start",
				Index:        &idx,
				ContentBlock: block,
			})
		}
	case "content_delta":
		idx := event.ContentIndex
		if event.Delta != nil && event.Delta.Native != nil && event.Delta.Native.Provider == "anthropic" && (event.Delta.Native.Format == "thinking_delta" || event.Delta.Native.Format == "signature_delta") {
			events = append(events, &StreamEvent{
				Type:  "content_block_delta",
				Index: &idx,
				Delta: &Delta{Type: event.Delta.Native.Format, Native: event.Delta.Native.Data},
			})
		} else if event.Delta != nil && event.Delta.Text != "" {
			events = append(events, &StreamEvent{
				Type:  "content_block_delta",
				Index: &idx,
				Delta: &Delta{
					Type: "text_delta",
					Text: event.Delta.Text,
				},
			})
		}
	case "tool_call_delta":
		idx := event.ContentIndex
		if event.ToolCall != nil && event.ToolCall.ArgumentsDelta != "" {
			events = append(events, &StreamEvent{
				Type:  "content_block_delta",
				Index: &idx,
				Delta: &Delta{
					Type:        "input_json_delta",
					PartialJson: event.ToolCall.ArgumentsDelta,
				},
			})
		}
	case "usage":
		if event.Usage != nil {
			events = append(events, &StreamEvent{
				Type:  "message_delta",
				Delta: &Delta{
					// empty delta
				},
				Usage: &Usage{
					OutputTokens: event.Usage.OutputTokens,
				},
			})
		}
	case "done":
		var stopReason *string
		if event.Stop != nil {
			r := event.Stop.Reason
			if r == "stop" {
				r = "end_turn"
			} else if r == "tool_calls" {
				r = "tool_use"
			} else if r == "length" {
				r = "max_tokens"
			}
			stopReason = &r
		} else {
			r := "end_turn"
			stopReason = &r
		}

		usage := &Usage{}
		if event.Usage != nil {
			usage = &Usage{OutputTokens: event.Usage.OutputTokens}
		}
		events = append(events, &StreamEvent{
			Type: "message_delta",
			Delta: &Delta{
				StopReason: stopReason,
			},
			Usage: usage,
		})

		events = append(events, &StreamEvent{
			Type: "message_stop",
		})
	}

	return events
}

func writeError(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorResponse{
		Type: "error",
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
		},
	})
}

func writeGatewayError(w http.ResponseWriter, err error) {
	var gwErr *canonical.GatewayError
	if errors.As(err, &gwErr) && gwErr != nil {
		if gwErr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(gwErr.RetryAfter))
		}
		status := gwErr.StatusCode
		if status < 300 || status > 999 {
			status = http.StatusBadGateway
		}
		writeError(w, status, gwErr.Message, anthropicErrorType(gwErr))
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error(), "api_error")
}

// anthropicErrorType maps HTTP statuses to Anthropic's documented error types.
func anthropicErrorType(err *canonical.GatewayError) string {
	switch err.StatusCode {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	case http.StatusServiceUnavailable:
		// The Anthropic outbound adapter translates an in-stream overload to 503.
		if err.Provider == "anthropic" {
			var response ErrorResponse
			if json.Unmarshal(err.Raw, &response) == nil && response.Error.Type == "overloaded_error" {
				return "overloaded_error"
			}
		}
	}
	return "api_error"
}
