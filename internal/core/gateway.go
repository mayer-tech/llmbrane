package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
	"github.com/mayer-tech/llmbrane/internal/debuglog"
	"github.com/mayer-tech/llmbrane/internal/logger"
	"github.com/mayer-tech/llmbrane/internal/ratelimit"
)

// InboundAdapter defines the interface for inbound translation.
// Typically implemented as an http.Handler that interacts with the Gateway.
type InboundAdapter interface {
}

// OutboundAdapter defines the interface for outbound provider translation.
type OutboundAdapter interface {
	ProcessChat(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*canonical.CanonicalChatResponse, error)
	ProcessChatStream(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error)
}

// Gateway orchestrates inbound and outbound adapters.
type Gateway struct {
	cfg         *config.Config
	adapters    map[string]OutboundAdapter
	rateLimiter *ratelimit.Manager
}

// NewGateway creates a new Core Gateway.
func NewGateway(cfg *config.Config) *Gateway {
	return &Gateway{
		cfg:         cfg,
		adapters:    make(map[string]OutboundAdapter),
		rateLimiter: ratelimit.NewManager(),
	}
}

// RegisterOutboundAdapter registers an API shape to an outbound adapter.
func (g *Gateway) RegisterOutboundAdapter(apiShape string, adapter OutboundAdapter) {
	g.adapters[apiShape] = adapter
}

// ConfiguredModels returns a list of all model names and aliases defined in the configuration.
func (g *Gateway) ConfiguredModels() []string {
	modelsMap := make(map[string]bool)

	// Add all explicitly defined models
	for pName, provider := range g.cfg.Providers {
		for _, ep := range provider.Endpoints {
			for mName := range ep.Models {
				modelsMap[fmt.Sprintf("%s/%s", pName, mName)] = true
			}
		}
	}

	// Add all aliases
	for aliasName := range g.cfg.Aliases {
		modelsMap[aliasName] = true
	}

	var models []string
	for modelName := range modelsMap {
		models = append(models, modelName)
	}

	return models
}

// ResolvedRoute contains routing information for a request.
type ResolvedRoute struct {
	ProviderName string
	APIShape     string
	Model        string
	Endpoint     config.Endpoint
	Capabilities []string
}

// ResolveRoute resolves aliases, extracts provider and model, and determines the endpoint.
func (g *Gateway) ResolveRoute(modelName string) (*ResolvedRoute, error) {
	// 1. Resolve alias
	if alias, ok := g.cfg.Aliases[modelName]; ok {
		modelName = alias
	}

	// 2. Extract provider and model
	parts := strings.SplitN(modelName, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid model format (expected provider/model): %s", modelName)
	}
	providerName := parts[0]
	model := parts[1]

	// 3. Find provider
	provider, ok := g.cfg.Providers[providerName]
	if !ok {
		return nil, fmt.Errorf("provider not found: %s", providerName)
	}

	// 4. Find endpoint & capabilities
	var foundEndpoint *config.Endpoint
	var targetModel string
	var modelCaps []string

	for i := range provider.Endpoints {
		ep := &provider.Endpoints[i]
		if mConf, ok := ep.Models[model]; ok {
			foundEndpoint = ep
			targetModel = mConf.Target
			modelCaps = mConf.Capabilities
			break
		}
	}

	if foundEndpoint == nil {
		return nil, fmt.Errorf("model %s not found in provider %s", model, providerName)
	}

	caps := foundEndpoint.Capabilities
	// Simple union for now, could be improved with intersection as per vision
	effectiveCaps := append([]string{}, caps...)
	if len(modelCaps) > 0 {
		effectiveCaps = append(effectiveCaps, modelCaps...)
	}

	return &ResolvedRoute{
		ProviderName: providerName,
		APIShape:     foundEndpoint.API,
		Model:        targetModel,
		Endpoint:     *foundEndpoint,
		Capabilities: effectiveCaps,
	}, nil
}

func determineRequiredCapabilities(req *canonical.CanonicalChatRequest) []string {
	var reqCaps []string
	if len(req.Tools) > 0 {
		reqCaps = append(reqCaps, "tools")
	}

	if req.Stream {
		reqCaps = append(reqCaps, "streaming")
	}

	hasVision := false
	hasAudio := false
	hasFile := false

	checkContent := func(content []canonical.CanonicalContentPart) {
		for _, part := range content {
			if part.Image != nil {
				hasVision = true
			}
			if part.Audio != nil {
				hasAudio = true
			}
			if part.File != nil {
				hasFile = true
			}
		}
	}

	for _, msg := range req.Messages {
		checkContent(msg.Content)
	}
	checkContent(req.System)

	if hasVision {
		reqCaps = append(reqCaps, "vision")
	}
	if hasAudio {
		reqCaps = append(reqCaps, "audio_input")
	}
	if hasFile {
		reqCaps = append(reqCaps, "file_input")
	}

	if req.ResponseFormat != nil {
		if req.ResponseFormat.Type == "json_schema" {
			reqCaps = append(reqCaps, "json_schema")
		} else if req.ResponseFormat.Type == "json_object" {
			reqCaps = append(reqCaps, "json_object")
		} else {
			reqCaps = append(reqCaps, "json") // generic fallback
		}
	}

	if req.Params.Logprobs != nil && *req.Params.Logprobs {
		reqCaps = append(reqCaps, "logprobs")
	}

	return reqCaps
}

func (g *Gateway) getPolicy(name string) string {
	if val, ok := g.cfg.Policies[name]; ok {
		if strVal, ok := val.(string); ok {
			return strVal
		}
	}
	return ""
}

func (g *Gateway) checkPolicies(req *canonical.CanonicalChatRequest, route *ResolvedRoute) error {
	// Policy: unknown_fields
	if g.getPolicy("unknown_fields") == "reject" && len(req.Extensions) > 0 {
		return &canonical.GatewayError{
			Provider:   route.ProviderName,
			Model:      route.Model,
			StatusCode: 400,
			ErrorKind:  canonical.ErrorKindPolicyViolation,
			Message:    "request contains unknown fields which are not allowed by policy",
		}
	}

	// Policy: unsupported_features
	reqCaps := determineRequiredCapabilities(req)

	supportedCaps := make(map[string]bool)
	for _, cap := range route.Capabilities {
		supportedCaps[cap] = true
	}

	var missingCaps []string
	for _, reqCap := range reqCaps {
		if !supportedCaps[reqCap] {
			missingCaps = append(missingCaps, reqCap)
		}
	}

	if len(missingCaps) > 0 {
		if g.getPolicy("unsupported_features") == "reject" {
			return &canonical.GatewayError{
				Provider:   route.ProviderName,
				Model:      route.Model,
				StatusCode: 400,
				ErrorKind:  canonical.ErrorKindUnsupportedFeature,
				Message:    fmt.Sprintf("request requires unsupported capabilities: %s", strings.Join(missingCaps, ", ")),
			}
		}
	}

	return nil
}

// ProcessChat routes a canonical chat request to the appropriate outbound adapter.
func (g *Gateway) ProcessChat(ctx context.Context, req *canonical.CanonicalChatRequest) (*canonical.CanonicalChatResponse, error) {
	route, err := g.ResolveRoute(req.Model)
	if err != nil {
		return nil, err
	}

	path, _ := canonical.PathFromContext(ctx)
	if path == "" {
		path = "unknown"
	}
	logger.Info.Printf("Request on path %q (model: %q) routed to provider %q (model: %q)", path, req.Model, route.ProviderName, route.Model)

	adapter, ok := g.adapters[route.APIShape]
	if !ok {
		return nil, fmt.Errorf("no outbound adapter registered for api shape: %s", route.APIShape)
	}

	if err := g.checkPolicies(req, route); err != nil {
		return nil, err
	}

	ctx, reservation, conversationID, err := g.reserveTokens(ctx, req, route)
	if err != nil {
		return nil, err
	}

	var response *canonical.CanonicalChatResponse
	if debuglog.Enabled() {
		response, err = g.processChatLogged(ctx, req, route, adapter)
	} else {
		response, err = adapter.ProcessChat(ctx, req, route.Model, route.Endpoint)
	}
	if err != nil {
		reservation.Cancel()
	} else {
		reservation.Finalize(usageFromResponse(response), conversationID)
	}
	return response, err
}

// processChatLogged wraps ProcessChat with canonical debug file logging.
func (g *Gateway) processChatLogged(ctx context.Context, req *canonical.CanonicalChatRequest, route *ResolvedRoute, adapter OutboundAdapter) (*canonical.CanonicalChatResponse, error) {
	start := time.Now()

	entry := &debuglog.Entry{
		Direction: "canonical",
		Provider:  route.ProviderName,
		Model:     route.Model,
	}

	if body, err := json.Marshal(req); err == nil {
		entry.Body = body
	}

	resp, err := adapter.ProcessChat(ctx, req, route.Model, route.Endpoint)

	entry.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		entry.Error = err.Error()
	} else if resp != nil {
		entry.Status = 200
		if respBody, mErr := json.Marshal(resp); mErr == nil {
			entry.ResponseBody = respBody
		}
	}
	debuglog.Write(entry)

	return resp, err
}

// ProcessChatStream routes a canonical chat stream request to the appropriate outbound adapter.
func (g *Gateway) ProcessChatStream(ctx context.Context, req *canonical.CanonicalChatRequest) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	route, err := g.ResolveRoute(req.Model)
	if err != nil {
		return nil, err
	}

	path, _ := canonical.PathFromContext(ctx)
	if path == "" {
		path = "unknown"
	}
	logger.Info.Printf("Request on path %q (model: %q) routed to provider %q (model: %q)", path, req.Model, route.ProviderName, route.Model)

	adapter, ok := g.adapters[route.APIShape]
	if !ok {
		return nil, fmt.Errorf("no outbound adapter registered for api shape: %s", route.APIShape)
	}

	if err := g.checkPolicies(req, route); err != nil {
		return nil, err
	}

	ctx, reservation, conversationID, err := g.reserveTokens(ctx, req, route)
	if err != nil {
		return nil, err
	}

	var stream <-chan *canonical.CanonicalChatStreamEvent
	if debuglog.Enabled() {
		stream, err = g.processChatStreamLogged(ctx, req, route, adapter)
	} else {
		stream, err = adapter.ProcessChatStream(ctx, req, route.Model, route.Endpoint)
	}
	if err != nil {
		reservation.Cancel()
		return nil, err
	}

	return g.finalizeStreamReservation(stream, reservation, conversationID), nil
}

func (g *Gateway) reserveTokens(ctx context.Context, req *canonical.CanonicalChatRequest, route *ResolvedRoute) (context.Context, *ratelimit.Reservation, string, error) {
	settings, err := rateLimitSettings(route.Endpoint)
	if err != nil {
		return ctx, nil, "", err
	}

	scope := fmt.Sprintf("%s\x00%s\x00%s", route.ProviderName, route.Endpoint.URL, route.Model)
	ctx = canonical.WithResponseHeadersObserver(ctx, func(headers http.Header) {
		g.rateLimiter.ObserveHeaders(scope, settings, headers)
	})

	conversationID, _ := canonical.ConversationIDFromContext(ctx)
	reservation, estimate, err := g.rateLimiter.Acquire(ctx, scope, conversationID, settings, req)
	if err == nil {
		return ctx, reservation, conversationID, nil
	}

	var queueErr *ratelimit.QueueError
	if errors.As(err, &queueErr) {
		retryAfter := int(math.Ceil(queueErr.RetryAfter.Seconds()))
		if retryAfter < 1 {
			retryAfter = 1
		}
		return ctx, nil, conversationID, &canonical.GatewayError{
			Provider:   route.ProviderName,
			Model:      route.Model,
			StatusCode: 429,
			ErrorKind:  canonical.ErrorKindRateLimit,
			Message:    queueErr.Message,
			RetryAfter: retryAfter,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ctx, nil, conversationID, &canonical.GatewayError{
			Provider: route.ProviderName, Model: route.Model, StatusCode: 504,
			ErrorKind: canonical.ErrorKindTimeout, Message: "local TPM queue wait exceeded request deadline",
		}
	}
	if errors.Is(err, context.Canceled) {
		return ctx, nil, conversationID, &canonical.GatewayError{
			Provider: route.ProviderName, Model: route.Model, StatusCode: 499,
			ErrorKind: canonical.ErrorKindTimeout, Message: "local TPM queue wait cancelled",
		}
	}
	return ctx, nil, conversationID, fmt.Errorf("failed to reserve local TPM budget (estimated %d tokens): %w", estimate.Total(), err)
}

func rateLimitSettings(endpoint config.Endpoint) (ratelimit.Settings, error) {
	settings := ratelimit.Settings{}
	if endpoint.RateLimit == nil {
		return settings, nil
	}
	settings.TokensPerMinute = endpoint.RateLimit.TokensPerMinute
	settings.Utilization = endpoint.RateLimit.Utilization
	settings.OutputReservationTokens = endpoint.RateLimit.OutputReservationTokens
	if endpoint.RateLimit.QueueTimeout != "" {
		d, err := time.ParseDuration(endpoint.RateLimit.QueueTimeout)
		if err != nil {
			return settings, fmt.Errorf("invalid configured TPM queue timeout: %w", err)
		}
		settings.QueueTimeout = d
		settings.QueueTimeoutConfigured = true
	}
	return settings, nil
}

func usageFromResponse(response *canonical.CanonicalChatResponse) *canonical.CanonicalUsage {
	if response == nil {
		return nil
	}
	return response.Usage
}

func (g *Gateway) finalizeStreamReservation(stream <-chan *canonical.CanonicalChatStreamEvent, reservation *ratelimit.Reservation, conversationID string) <-chan *canonical.CanonicalChatStreamEvent {
	finalized := make(chan *canonical.CanonicalChatStreamEvent, cap(stream))
	go func() {
		defer close(finalized)
		var usage *canonical.CanonicalUsage
		for event := range stream {
			if event != nil && event.Usage != nil {
				usage = mergeUsage(usage, event.Usage)
			}
			finalized <- event
		}
		reservation.Finalize(usage, conversationID)
	}()
	return finalized
}

func mergeUsage(current, update *canonical.CanonicalUsage) *canonical.CanonicalUsage {
	if current == nil {
		return &canonical.CanonicalUsage{
			InputTokens: update.InputTokens, OutputTokens: update.OutputTokens, TotalTokens: update.TotalTokens,
			CachedInputTokens: update.CachedInputTokens, ReasoningTokens: update.ReasoningTokens,
		}
	}
	if update.InputTokens > current.InputTokens {
		current.InputTokens = update.InputTokens
	}
	if update.OutputTokens > current.OutputTokens {
		current.OutputTokens = update.OutputTokens
	}
	if update.TotalTokens > current.TotalTokens {
		current.TotalTokens = update.TotalTokens
	}
	if current.TotalTokens == 0 && (current.InputTokens > 0 || current.OutputTokens > 0) {
		current.TotalTokens = current.InputTokens + current.OutputTokens
	}
	if update.CachedInputTokens > current.CachedInputTokens {
		current.CachedInputTokens = update.CachedInputTokens
	}
	if update.ReasoningTokens > current.ReasoningTokens {
		current.ReasoningTokens = update.ReasoningTokens
	}
	return current
}

// processChatStreamLogged wraps ProcessChatStream with canonical debug file logging.
// Stream events are collected and written as a single entry when the stream ends.
func (g *Gateway) processChatStreamLogged(ctx context.Context, req *canonical.CanonicalChatRequest, route *ResolvedRoute, adapter OutboundAdapter) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	start := time.Now()

	entry := &debuglog.Entry{
		Direction: "canonical",
		Provider:  route.ProviderName,
		Model:     route.Model,
	}

	if body, err := json.Marshal(req); err == nil {
		entry.Body = body
	}

	ch, err := adapter.ProcessChatStream(ctx, req, route.Model, route.Endpoint)
	if err != nil {
		entry.DurationMs = time.Since(start).Milliseconds()
		entry.Error = err.Error()
		debuglog.Write(entry)
		return nil, err
	}

	// Wrap the channel to collect events
	loggedCh := make(chan *canonical.CanonicalChatStreamEvent, cap(ch))
	go func() {
		defer close(loggedCh)
		for event := range ch {
			if event != nil {
				if data, err := json.Marshal(event); err == nil {
					entry.StreamEvents = append(entry.StreamEvents, data)
				}
			}
			loggedCh <- event
		}
		entry.DurationMs = time.Since(start).Milliseconds()
		entry.Status = 200
		debuglog.Write(entry)
	}()

	return loggedCh, nil
}
