package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
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
	cfg      *config.Config
	adapters map[string]OutboundAdapter
}

// NewGateway creates a new Core Gateway.
func NewGateway(cfg *config.Config) *Gateway {
	return &Gateway{
		cfg:      cfg,
		adapters: make(map[string]OutboundAdapter),
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

	adapter, ok := g.adapters[route.APIShape]
	if !ok {
		return nil, fmt.Errorf("no outbound adapter registered for api shape: %s", route.APIShape)
	}

	if err := g.checkPolicies(req, route); err != nil {
		return nil, err
	}

	return adapter.ProcessChat(ctx, req, route.Model, route.Endpoint)
}

// ProcessChatStream routes a canonical chat stream request to the appropriate outbound adapter.
func (g *Gateway) ProcessChatStream(ctx context.Context, req *canonical.CanonicalChatRequest) (<-chan *canonical.CanonicalChatStreamEvent, error) {
	route, err := g.ResolveRoute(req.Model)
	if err != nil {
		return nil, err
	}

	adapter, ok := g.adapters[route.APIShape]
	if !ok {
		return nil, fmt.Errorf("no outbound adapter registered for api shape: %s", route.APIShape)
	}

	if err := g.checkPolicies(req, route); err != nil {
		return nil, err
	}

	return adapter.ProcessChatStream(ctx, req, route.Model, route.Endpoint)
}
