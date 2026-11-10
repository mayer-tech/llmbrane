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

func TestResolveChatGPTCredentialPaths(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "credentials.json")
	provider := config.Provider{Endpoints: []config.Endpoint{
		{Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "credentials.json"}},
		{Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: abs}},
	}}
	cfg := &config.Config{Providers: map[string]config.Provider{"p": provider}}
	dataDir := filepath.Join(t.TempDir(), "state")
	resolveChatGPTCredentialPaths(cfg, dataDir)
	endpoints := cfg.Providers["p"].Endpoints
	if got, want := endpoints[0].Auth.CredentialsFile, filepath.Join(dataDir, "credentials.json"); got != want {
		t.Fatalf("relative path = %q, want %q", got, want)
	}
	if got := endpoints[1].Auth.CredentialsFile; got != abs {
		t.Fatalf("absolute path changed to %q", got)
	}
}
