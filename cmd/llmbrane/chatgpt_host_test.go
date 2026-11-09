package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestInitializeChatGPTHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg := &config.Config{}
	if err := initializeChatGPTHost(cfg, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("unexpected data directory: %v", err)
	}
	cfg.Providers = map[string]config.Provider{"openai": {Endpoints: []config.Endpoint{{Auth: &config.EndpointAuth{Type: "chatgpt"}}}}}
	if err := initializeChatGPTHost(cfg, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "chatgpt-host-id")); err != nil {
		t.Fatal(err)
	}
	if err := initializeChatGPTHost(cfg, dir); err != nil {
		t.Fatal(err)
	}
}
