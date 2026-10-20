package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go/eventstream"
	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	httpretry "github.com/mayer-tech/llmbrane/internal/outbound/retry"
)

type Adapter struct {
	client      *http.Client
	idleTimeout time.Duration
	retryPolicy httpretry.Policy
}

func NewAdapter() *Adapter {
	return NewAdapterWithTimeout(60 * time.Second)
}

func NewAdapterWithTimeout(idleTimeout time.Duration) *Adapter {
	return NewAdapterWithTransport(idleTimeout, nil)
}

// NewAdapterWithTransport creates an adapter with a custom HTTP transport and
// the default retry policy. If transport is nil, http.DefaultTransport is used.
func NewAdapterWithTransport(idleTimeout time.Duration, transport http.RoundTripper) *Adapter {
	return NewAdapterWithPolicy(idleTimeout, transport, httpretry.DefaultPolicy)
}

// NewAdapterWithPolicy creates an adapter with a custom HTTP transport and an
// explicit retry policy for provider-side HTTP 429 responses.
func NewAdapterWithPolicy(idleTimeout time.Duration, transport http.RoundTripper, retryPolicy httpretry.Policy) *Adapter {
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}
	return &Adapter{
		client:      &http.Client{Transport: transport},
		idleTimeout: idleTimeout,
		retryPolicy: retryPolicy,
	}
}

func (a *Adapter) translateRequest(req *canonical.CanonicalChatRequest) ConverseRequest {
	var messages []Message
	var system []SystemContent

	for _, m := range req.Messages {
		if m.Role == "system" {
			// Extract system content text
			text := ""
			for _, part := range m.Content {
				if part.Type == "text" {
					text += part.Text
				}
			}
			system = append(system, SystemContent{Text: text})
			continue
		}

		role := string(m.Role)
		// Bedrock Converse expects only "user" or "assistant"
		if role != "user" && role != "assistant" {
			role = "user" // Default fallback or handle appropriately
		}

		// Extract content
		var contents []Content
		for _, part := range m.Content {
			if part.Type == "text" {
				if part.Text != "" {
					contents = append(contents, Content{Text: part.Text})
				}
			} else if part.Type == "image" && part.Image != nil {
				if strings.HasPrefix(part.Image.URL, "data:") {
					idx := strings.Index(part.Image.URL, ";base64,")
					if idx != -1 {
						mediaType := part.Image.URL[5:idx]
						format := strings.TrimPrefix(mediaType, "image/")
						if format == "jpeg" || format == "png" || format == "gif" || format == "webp" {
							base64Data := part.Image.URL[idx+8:]
							decoded, err := base64.StdEncoding.DecodeString(base64Data)
							if err == nil {
								contents = append(contents, Content{
									Image: &ImageBlock{
										Format: format,
										Source: ImageSource{
											Bytes: decoded,
										},
									},
								})
							}
						}
					}
				}
			} else if part.Type == "reasoning" && part.Reasoning != nil {
				if part.Reasoning.Content != "" {
					contents = append(contents, Content{
						ReasoningContent: &ReasoningContent{
							ReasoningText: part.Reasoning.Content,
						},
					})
				}
			} else if part.Type == "tool_call" && part.ToolCall != nil {
				var input map[string]any
				if err := json.Unmarshal(part.ToolCall.Arguments, &input); err != nil {
					input = make(map[string]any)
				}
				contents = append(contents, Content{
					ToolUse: &ToolUse{
						ToolUseId: part.ToolCall.ID,
						Name:      part.ToolCall.Name,
						Input:     input,
					},
				})
			} else if part.Type == "tool_result" && part.ToolResult != nil {
				status := "success"
				if part.ToolResult.IsError {
					status = "error"
				}
				var trContents []ToolResultContent
				for _, trPart := range part.ToolResult.Content {
					if trPart.Type == "text" && trPart.Text != "" {
						trContents = append(trContents, ToolResultContent{Text: trPart.Text})
					}
				}
				if len(trContents) == 0 {
					trContents = append(trContents, ToolResultContent{Text: "success"})
				}
				contents = append(contents, Content{
					ToolResult: &ToolResult{
						ToolUseId: part.ToolResult.ToolCallID,
						Status:    status,
						Content:   trContents,
					},
				})
			}
		}

		// If a message has no content blocks, Bedrock requires at least one block.
		// We fallback to a single space text block to avoid validation errors.
		if len(contents) == 0 {
			contents = append(contents, Content{Text: " "})
		}

		messages = append(messages, Message{
			Role:    role,
			Content: contents,
		})
	}

	out := ConverseRequest{
		Messages: messages,
		System:   system,
	}

	if req.Params.Temperature != nil || req.Params.TopP != nil || req.Params.MaxOutputTokens != nil || len(req.Params.Stop) > 0 {
		out.InferenceConfig = &InferenceConfig{}

		// AWS Bedrock (speziell Anthropic Modelle über Bedrock) erlaubt oftmals nicht
		// das gleichzeitige Setzen von Temperature und TopP (insbesondere wenn einer von beiden
		// nicht explizit auf einem nicht-default Wert steht). Wir priorisieren Temperature.
		if req.Params.Temperature != nil {
			val := float32(*req.Params.Temperature)
			out.InferenceConfig.Temperature = &val
		} else if req.Params.TopP != nil {
			val := float32(*req.Params.TopP)
			out.InferenceConfig.TopP = &val
		}

		if req.Params.MaxOutputTokens != nil && *req.Params.MaxOutputTokens > 0 {
			out.InferenceConfig.MaxTokens = *req.Params.MaxOutputTokens
		}
		if len(req.Params.Stop) > 0 {
			out.InferenceConfig.StopSequences = req.Params.Stop
		}
	}

	if len(req.Tools) > 0 {
		out.ToolConfig = &ToolConfig{}
		for _, t := range req.Tools {
			out.ToolConfig.Tools = append(out.ToolConfig.Tools, Tool{
				ToolSpec: ToolSpec{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: InputSchema{JSON: t.InputSchema},
				},
			})
		}
		if req.ToolChoice != nil {
			switch req.ToolChoice.Type {
			case "auto":
				out.ToolConfig.ToolChoice = &ToolChoice{Auto: &struct{}{}}
			case "any", "required":
				out.ToolConfig.ToolChoice = &ToolChoice{Any: &struct{}{}}
			case "tool":
				out.ToolConfig.ToolChoice = &ToolChoice{Tool: &SpecificToolChoice{Name: req.ToolChoice.Name}}
			}
		}
	}

	if req.Params.ReasoningEffort != nil {
		eff := *req.Params.ReasoningEffort
		if eff == "none" {
			out.AdditionalModelRequestFields = map[string]any{
				"thinking": map[string]any{
					"type": "disabled",
				},
			}
		} else {
			out.AdditionalModelRequestFields = map[string]any{
				"thinking": map[string]any{
					"type":          "enabled",
					"budget_tokens": canonical.ReasoningTokenBudget(eff),
				},
			}
		}
	}

	return out
}

func (a *Adapter) ProcessChat(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*canonical.CanonicalChatResponse, error) {
	outReq := a.translateRequest(req)

	reqBody, err := json.Marshal(outReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal outbound request: %w", err)
	}

	// For Converse API: https://bedrock-runtime.{region}.amazonaws.com/model/{modelId}/converse
	url := strings.ReplaceAll(endpoint.URL, "{target_model}", model)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range endpoint.Headers {
		httpReq.Header.Set(k, v)
	}

	// Apply AWS SigV4 if credentials are provided in extensions
	if err := signRequestIfRequired(httpReq, reqBody, endpoint.Extensions); err != nil {
		return nil, err
	}

	resp, err := a.retryPolicy.Do(ctx, a.client, httpReq)
	if err != nil {
		return nil, &canonical.GatewayError{Provider: "bedrock", Model: model, StatusCode: http.StatusServiceUnavailable, ErrorKind: canonical.ErrorKindProviderError, Message: fmt.Sprintf("request failed: %v", err)}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &canonical.GatewayError{Provider: "bedrock", Model: model, StatusCode: http.StatusInternalServerError, ErrorKind: canonical.ErrorKindProviderError, Message: fmt.Sprintf("failed to read response: %v", err)}
	}

	if resp.StatusCode != http.StatusOK {
		gatewayErr := canonical.NewGatewayError("bedrock", model, fmt.Sprintf("unexpected status code: %d, body: %s", resp.StatusCode, string(bodyBytes)), resp.StatusCode, bodyBytes)
		return nil, httpretry.ApplyHeaders(gatewayErr, resp.Header)
	}

	var converseResp ConverseResponse
	if err := json.Unmarshal(bodyBytes, &converseResp); err != nil {
		return nil, &canonical.GatewayError{Provider: "bedrock", Model: model, StatusCode: http.StatusInternalServerError, ErrorKind: canonical.ErrorKindProviderError, Message: fmt.Sprintf("failed to decode response: %v", err)}
	}

	var contentParts []canonical.CanonicalContentPart
	for _, part := range converseResp.Output.Message.Content {
		if part.Text != "" {
			contentParts = append(contentParts, canonical.CanonicalContentPart{
				Type: "text",
				Text: part.Text,
			})
		} else if part.ReasoningContent != nil && part.ReasoningContent.ReasoningText != "" {
			contentParts = append(contentParts, canonical.CanonicalContentPart{
				Type: "reasoning",
				Reasoning: &canonical.CanonicalReasoning{
					Content: part.ReasoningContent.ReasoningText,
				},
			})
		} else if part.ToolUse != nil {
			args, _ := json.Marshal(part.ToolUse.Input)
			contentParts = append(contentParts, canonical.CanonicalContentPart{
				Type: "tool_call",
				ToolCall: &canonical.CanonicalToolCall{
					ID:        part.ToolUse.ToolUseId,
					Name:      part.ToolUse.Name,
					Arguments: args,
				},
			})
		}
	}

	return &canonical.CanonicalChatResponse{
		Model: model,
		Choices: []canonical.CanonicalChoice{
			{
				Message: canonical.CanonicalMessage{
					Role:    canonical.CanonicalRole(converseResp.Output.Message.Role),
					Content: contentParts,
				},
			},
		},
		Usage: &canonical.CanonicalUsage{
			InputTokens:  converseResp.Usage.InputTokens,
			OutputTokens: converseResp.Usage.OutputTokens,
			TotalTokens:  converseResp.Usage.TotalTokens,
		},
	}, nil
}

func (a *Adapter) ProcessChatStream(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	outReq := a.translateRequest(req)

	reqBody, err := json.Marshal(outReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal outbound request: %w", err)
	}

	// For ConverseStream API
	url := strings.ReplaceAll(endpoint.URL, "{target_model}", model)
	// Make sure the URL uses the stream endpoint if it ends with /converse
	if strings.HasSuffix(url, "/converse") {
		url = url + "-stream"
	} else if !strings.HasSuffix(url, "/converse-stream") {
		// Fallback simple replacement if user configured it generically
		url = strings.ReplaceAll(url, "/converse", "/converse-stream")
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range endpoint.Headers {
		httpReq.Header.Set(k, v)
	}

	if err := signRequestIfRequired(httpReq, reqBody, endpoint.Extensions); err != nil {
		return nil, err
	}

	resp, err := a.retryPolicy.Do(ctx, a.client, httpReq)
	if err != nil {
		return nil, &canonical.GatewayError{Provider: "bedrock", Model: model, StatusCode: http.StatusServiceUnavailable, ErrorKind: canonical.ErrorKindProviderError, Message: fmt.Sprintf("request failed: %v", err)}
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		gatewayErr := canonical.NewGatewayError("bedrock", model, fmt.Sprintf("unexpected status code: %d, body: %s", resp.StatusCode, string(bodyBytes)), resp.StatusCode, bodyBytes)
		return nil, httpretry.ApplyHeaders(gatewayErr, resp.Header)
	}

	ch := make(chan *canonical.CanonicalChatStreamEvent)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		idleReader := canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout)
		decoder := eventstream.NewDecoder()
		buf := make([]byte, 8192)

		for {
			msg, err := decoder.Decode(idleReader, buf)
			if err != nil {
				if err == io.EOF {
					break
				}
				ch <- &canonical.CanonicalChatStreamEvent{
					Error: &canonical.GatewayError{Provider: "bedrock", Model: model, StatusCode: http.StatusInternalServerError, ErrorKind: canonical.ErrorKindProviderError, Message: fmt.Sprintf("stream read error: %v", err)},
				}
				return
			}

			// Bedrock EventStream structure for ConverseStream
			// Message has Headers: ":event-type" (e.g. "messageStart", "contentBlockDelta", "messageStop")
			var eventType string
			for _, h := range msg.Headers {
				if h.Name == ":event-type" {
					eventType = h.Value.String()
					break
				}
			}

			switch eventType {
			case "contentBlockStart":
				var payload struct {
					ContentBlockIndex int `json:"contentBlockIndex"`
					Start             struct {
						ToolUse *struct {
							ToolUseId string `json:"toolUseId"`
							Name      string `json:"name"`
						} `json:"toolUse"`
					} `json:"start"`
				}
				if err := json.Unmarshal(msg.Payload, &payload); err == nil && payload.Start.ToolUse != nil {
					ch <- &canonical.CanonicalChatStreamEvent{
						Type:         "tool_call_start",
						ContentIndex: payload.ContentBlockIndex,
						ToolCall: &canonical.CanonicalToolCallDelta{
							ID:    payload.Start.ToolUse.ToolUseId,
							Name:  payload.Start.ToolUse.Name,
							Index: payload.ContentBlockIndex,
						},
					}
				}
			case "contentBlockDelta":
				var payload struct {
					ContentBlockIndex int `json:"contentBlockIndex"`
					Delta             struct {
						Text             string `json:"text"`
						ReasoningContent *struct {
							Text string `json:"text"`
						} `json:"reasoningContent"`
						ToolUse *struct {
							Input string `json:"input"`
						} `json:"toolUse"`
					} `json:"delta"`
				}
				if err := json.Unmarshal(msg.Payload, &payload); err == nil {
					if payload.Delta.Text != "" {
						ch <- &canonical.CanonicalChatStreamEvent{
							Type:         "delta",
							ContentIndex: payload.ContentBlockIndex,
							Delta: &canonical.CanonicalContentPart{
								Type: "text",
								Text: payload.Delta.Text,
							},
						}
					} else if payload.Delta.ReasoningContent != nil && payload.Delta.ReasoningContent.Text != "" {
						ch <- &canonical.CanonicalChatStreamEvent{
							Type:         "delta",
							ContentIndex: payload.ContentBlockIndex,
							Delta: &canonical.CanonicalContentPart{
								Type: "reasoning",
								Reasoning: &canonical.CanonicalReasoning{
									Content: payload.Delta.ReasoningContent.Text,
								},
							},
						}
					} else if payload.Delta.ToolUse != nil && payload.Delta.ToolUse.Input != "" {
						ch <- &canonical.CanonicalChatStreamEvent{
							Type:         "tool_call_delta",
							ContentIndex: payload.ContentBlockIndex,
							ToolCall: &canonical.CanonicalToolCallDelta{
								ArgumentsDelta: payload.Delta.ToolUse.Input,
								Index:          payload.ContentBlockIndex,
							},
						}
					}
				}
			case "messageStop":
				var payload struct {
					StopReason string `json:"stopReason"`
				}
				if err := json.Unmarshal(msg.Payload, &payload); err == nil {
					reason := payload.StopReason
					if reason == "tool_use" {
						reason = "tool_calls"
					} else if reason == "end_turn" {
						reason = "stop"
					} else if reason == "max_tokens" {
						reason = "length"
					}
					ch <- &canonical.CanonicalChatStreamEvent{
						Type: "stop",
						Stop: &canonical.CanonicalStop{Reason: reason},
					}
				}
			case "metadata":
				var payload struct {
					Usage struct {
						InputTokens  int `json:"inputTokens"`
						OutputTokens int `json:"outputTokens"`
						TotalTokens  int `json:"totalTokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(msg.Payload, &payload); err == nil {
					ch <- &canonical.CanonicalChatStreamEvent{
						Type: "usage",
						Usage: &canonical.CanonicalUsage{
							InputTokens:  payload.Usage.InputTokens,
							OutputTokens: payload.Usage.OutputTokens,
							TotalTokens:  payload.Usage.TotalTokens,
						},
					}
				}
			case "internalServerException", "modelStreamErrorException", "validationException", "throttlingException":
				var errPayload struct {
					Message string `json:"message"`
				}
				json.Unmarshal(msg.Payload, &errPayload)
				ch <- &canonical.CanonicalChatStreamEvent{
					Error: &canonical.GatewayError{Provider: "bedrock", Model: model, StatusCode: http.StatusInternalServerError, ErrorKind: canonical.ErrorKindProviderError, Message: fmt.Sprintf("%s: %s", eventType, errPayload.Message)},
				}
				return
			}
		}
	}()

	return ch, nil
}

func signRequestIfRequired(req *http.Request, payload []byte, ext map[string]any) error {
	if ext == nil {
		return nil
	}

	// Look for AWS credentials in the endpoint extensions
	accessKeyID, _ := ext["aws_access_key_id"].(string)
	secretAccessKey, _ := ext["aws_secret_access_key"].(string)
	region, _ := ext["aws_region"].(string)

	if accessKeyID == "" || secretAccessKey == "" || region == "" {
		return nil // Not enough info to sign, assume custom endpoint or proxy
	}

	signer := v4.NewSigner()

	hash := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(hash[:])

	creds := credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")
	ctx := req.Context()

	// Check if session token exists
	if sessionToken, ok := ext["aws_session_token"].(string); ok {
		creds = credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, sessionToken)
	}

	credValues, err := creds.Retrieve(ctx)
	if err != nil {
		return &canonical.GatewayError{Provider: "bedrock", StatusCode: http.StatusInternalServerError, ErrorKind: canonical.ErrorKindInternalError, Message: fmt.Sprintf("failed to retrieve credentials: %v", err)}
	}

	err = signer.SignHTTP(ctx, credValues, req, payloadHash, "bedrock", region, time.Now())
	if err != nil {
		return &canonical.GatewayError{Provider: "bedrock", StatusCode: http.StatusInternalServerError, ErrorKind: canonical.ErrorKindInternalError, Message: fmt.Sprintf("failed to sign request: %v", err)}
	}

	return nil
}

// ... streaming implementation will follow using eventstream parsing
