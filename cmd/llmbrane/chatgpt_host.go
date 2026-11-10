package main

import (
	"fmt"
	"path/filepath"

	"github.com/mayer-tech/llmbrane/internal/chatgptlogin"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func resolveChatGPTCredentialPaths(cfg *config.Config, dataDir string) {
	for providerName, provider := range cfg.Providers {
		for i := range provider.Endpoints {
			auth := provider.Endpoints[i].Auth
			if auth != nil && auth.Type == "chatgpt" && !filepath.IsAbs(auth.CredentialsFile) {
				auth.CredentialsFile = filepath.Join(dataDir, auth.CredentialsFile)
			}
		}
		cfg.Providers[providerName] = provider
	}
}

func initializeChatGPTHost(cfg *config.Config, dataDir string) error {
	for _, provider := range cfg.Providers {
		for _, endpoint := range provider.Endpoints {
			if endpoint.Auth != nil && endpoint.Auth.Type == "chatgpt" {
				if _, err := chatgptlogin.ServerHostID(dataDir); err != nil {
					return fmt.Errorf("data directory %q: %w", dataDir, err)
				}
				return nil
			}
		}
	}
	return nil
}
