package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestConfigLoadAndInterpolate(t *testing.T) {
	os.Setenv("TEST_API_KEY", "secret-key-123")
	defer os.Unsetenv("TEST_API_KEY")

	yamlContent := []byte(`
version: 1
server:
  listen: ":8080"
  request_timeout: 120s
providers:
  openai:
    endpoints:
      - url: "https://api.openai.com/v1"
        api: "openai-chat-completions"
        headers:
          Authorization: "Bearer ${TEST_API_KEY}"
`)

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configFile, yamlContent, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.Listen != ":8080" {
		t.Errorf("expected listen :8080, got %s", cfg.Server.Listen)
	}

	provider, ok := cfg.Providers["openai"]
	if !ok || len(provider.Endpoints) == 0 {
		t.Fatalf("expected provider 'openai' with endpoints to be loaded")
	}

	ep := provider.Endpoints[0]
	if ep.Headers["Authorization"] != "Bearer secret-key-123" {
		t.Errorf("expected interpolated header, got %q", ep.Headers["Authorization"])
	}
}

func TestConfigMissingEnv(t *testing.T) {
	yamlContent := []byte(`
providers:
  openai:
    endpoints:
      - url: "https://api.openai.com/v1"
        api: "openai-chat-completions"
        headers:
          Authorization: "Bearer ${MISSING_VAR}"
`)

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configFile, yamlContent, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := config.Load(configFile)
	if err == nil {
		t.Fatalf("expected error due to missing env var, got nil")
	}
}

func TestConfigInvalidEndpoint(t *testing.T) {
	yamlContent := []byte(`
providers:
  openai:
    endpoints:
      - url: "::invalid-url::"
        api: "openai-chat-completions"
`)

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configFile, yamlContent, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	_, err := config.Load(configFile)
	if err == nil {
		t.Fatalf("expected error due to invalid url, got nil")
	}
}

func TestConfigRateLimit(t *testing.T) {
	yamlContent := []byte(`
providers:
  openai:
    endpoints:
      - url: "https://api.openai.com/v1"
        api: "openai-chat-completions"
        rate_limit:
          tokens_per_minute: 200000
          utilization: 0.9
          queue_timeout: 30s
          output_reservation_tokens: 8192
`)

	configFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configFile, yamlContent, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	rl := cfg.Providers["openai"].Endpoints[0].RateLimit
	if rl == nil || rl.TokensPerMinute != 200000 || rl.Utilization != 0.9 || rl.QueueTimeout != "30s" || rl.OutputReservationTokens != 8192 {
		t.Fatalf("unexpected rate-limit config: %+v", rl)
	}
}
