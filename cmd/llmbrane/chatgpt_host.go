package main

import (
	"fmt"

	"github.com/mayer-tech/llmbrane/internal/chatgptlogin"
	"github.com/mayer-tech/llmbrane/internal/config"
)

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
