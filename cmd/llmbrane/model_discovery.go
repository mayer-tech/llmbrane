package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mayer-tech/llmbrane/internal/chatgptlogin"
	"github.com/mayer-tech/llmbrane/internal/config"
)

const maxDiscoveredModelsResponse = 4 << 20

var modelSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

type modelCatalog struct {
	Models []struct {
		Slug       string `json:"slug"`
		Visibility string `json:"visibility"`
	} `json:"models"`
}

type discoveryTokenSource interface {
	Token(context.Context) (string, error)
}

func fetchChatGPTModels(ctx context.Context, client *http.Client, tokens discoveryTokenSource) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	token, err := tokens.Token(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("could not obtain credentials")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("could not create model discovery request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	cl := *client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model discovery request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model discovery returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveredModelsResponse+1))
	if err != nil || len(body) > maxDiscoveredModelsResponse {
		return nil, fmt.Errorf("invalid or oversized model discovery response")
	}
	var catalog modelCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("invalid model discovery response")
	}
	seen := map[string]bool{}
	var slugs []string
	if catalog.Models == nil {
		return nil, fmt.Errorf("invalid model discovery response: missing models array")
	}
	for _, model := range catalog.Models {
		if model.Visibility == "list" && modelSlugPattern.MatchString(model.Slug) && !seen[model.Slug] {
			seen[model.Slug] = true
			slugs = append(slugs, model.Slug)
		}
	}
	return slugs, nil
}

func discoverChatGPTModels(ctx context.Context, cfg *config.Config) {
	client := &http.Client{Timeout: 15 * time.Second}
	discoverChatGPTModelsWith(ctx, cfg, func(ctx context.Context, ep config.Endpoint) ([]string, error) {
		manager, err := chatgptlogin.NewTokenManager(ep.Auth.CredentialsFile, nil)
		if err != nil {
			return nil, err
		}
		return fetchChatGPTModels(ctx, client, manager)
	})
}

func discoverChatGPTModelsWith(ctx context.Context, cfg *config.Config, fetch func(context.Context, config.Endpoint) ([]string, error)) {
	for providerName, provider := range cfg.Providers {
		configured := map[string]bool{}
		for _, ep := range provider.Endpoints {
			for name := range ep.Models {
				configured[name] = true
			}
		}
		for i := range provider.Endpoints {
			ep := &provider.Endpoints[i]
			if ep.Auth == nil || !ep.Auth.AutoDiscoverModels {
				continue
			}
			slugs, err := fetch(ctx, *ep)
			if err != nil {
				logDiscoveryWarning(providerName, err)
				continue
			}
			for _, slug := range slugs {
				if !configured[slug] {
					if ep.Models == nil {
						ep.Models = map[string]config.ModelConf{}
					}
					ep.Models[slug] = config.ModelConf{Target: slug}
				}
			}
		}
		cfg.Providers[providerName] = provider
	}
}

func logDiscoveryWarning(provider string, err error) {
	log.Printf("warning: ChatGPT model discovery for provider %q failed: %v", provider, err)
}
