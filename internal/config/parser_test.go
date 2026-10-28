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

func TestConfigAliasDefaultReasoningEffort(t *testing.T) {
	yamlContent := []byte(`
aliases:
  gpt-latest: openai/gpt-5.5
  gpt-fast:
    target: openai/gpt-5.5
    default_reasoning_effort: low
providers:
  openai:
    endpoints:
      - url: "https://api.openai.com/v1"
        api: "openai-chat-completions"
        models:
          gpt-5.5:
            target: gpt-5.5
            default_reasoning_effort: high
`)

	configFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configFile, yamlContent, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if a := cfg.Aliases["gpt-latest"]; a.Target != "openai/gpt-5.5" || a.DefaultReasoningEffort != "" {
		t.Errorf("unexpected shorthand alias: %+v", a)
	}
	if a := cfg.Aliases["gpt-fast"]; a.Target != "openai/gpt-5.5" || a.DefaultReasoningEffort != "low" {
		t.Errorf("unexpected mapping alias: %+v", a)
	}

	invalidYAML := []byte(`
aliases:
  bad:
    target: openai/gpt-5.5
    default_reasoning_effort: ultra
providers:
  openai:
    endpoints:
      - url: "https://api.openai.com/v1"
        api: "openai-chat-completions"
        models:
          gpt-5.5:
            target: gpt-5.5
`)

	configFile = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configFile, invalidYAML, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	if _, err := config.Load(configFile); err == nil {
		t.Fatal("expected validation error for invalid alias default_reasoning_effort")
	}
}

func TestConfigDefaultReasoningEffort(t *testing.T) {
	validYAML := []byte(`
providers:
  openai:
    endpoints:
      - url: "https://api.openai.com/v1"
        api: "openai-chat-completions"
        models:
          gpt-5.5:
            target: gpt-5.5
            default_reasoning_effort: high
          gpt-5.5-mini:
            target: gpt-5.5-mini
`)

	configFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configFile, validYAML, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	models := cfg.Providers["openai"].Endpoints[0].Models
	if models["gpt-5.5"].DefaultReasoningEffort != "high" {
		t.Errorf("expected default_reasoning_effort 'high', got %q", models["gpt-5.5"].DefaultReasoningEffort)
	}
	if models["gpt-5.5-mini"].DefaultReasoningEffort != "" {
		t.Errorf("expected empty default_reasoning_effort, got %q", models["gpt-5.5-mini"].DefaultReasoningEffort)
	}

	invalidYAML := []byte(`
providers:
  openai:
    endpoints:
      - url: "https://api.openai.com/v1"
        api: "openai-chat-completions"
        models:
          gpt-5.5:
            target: gpt-5.5
            default_reasoning_effort: ultra
`)

	configFile = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configFile, invalidYAML, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	if _, err := config.Load(configFile); err == nil {
		t.Fatal("expected error due to invalid default_reasoning_effort, got nil")
	}
}

func TestAnthropicExtensionsValidation(t *testing.T) {
	tests := []struct {
		name       string
		extensions map[string]any
		wantError  bool
	}{
		{"defaults", nil, false},
		{"adaptive", map[string]any{"thinking_mode": "adaptive"}, false},
		{"budget and mapping", map[string]any{"thinking_mode": "budget", "reasoning_effort_mapping": map[string]any{"none": "low", "xhigh": "high"}}, false},
		{"string mapping", map[string]any{"reasoning_effort_mapping": map[string]string{"minimal": "low"}}, false},
		{"invalid mode", map[string]any{"thinking_mode": "manual"}, true},
		{"non-string mode", map[string]any{"thinking_mode": 1}, true},
		{"invalid mapping type", map[string]any{"reasoning_effort_mapping": "low"}, true},
		{"invalid mapping key", map[string]any{"reasoning_effort_mapping": map[string]string{"ultra": "low"}}, true},
		{"invalid mapping value", map[string]any{"reasoning_effort_mapping": map[string]any{"low": "ultra"}}, true},
		{"non-string mapping value", map[string]any{"reasoning_effort_mapping": map[string]any{"low": 1}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{Providers: map[string]config.Provider{
				"anthropic": {Endpoints: []config.Endpoint{{API: "anthropic-messages", URL: "https://example.com", Extensions: tt.extensions}}},
			}}
			if err := cfg.Validate(); (err != nil) != tt.wantError {
				t.Fatalf("Validate() error = %v, want error %v", err, tt.wantError)
			}
		})
	}
}

func TestLoadAnthropicExtensions(t *testing.T) {
	yamlContent := []byte(`
providers:
  anthropic:
    endpoints:
      - api: anthropic-messages
        url: https://example.com
        extensions:
          thinking_mode: budget
          reasoning_effort_mapping:
            none: low
            xhigh: high
`)
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configFile, yamlContent, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatal(err)
	}
	ext := cfg.Providers["anthropic"].Endpoints[0].Extensions
	mapping, ok := ext["reasoning_effort_mapping"].(map[string]any)
	if ext["thinking_mode"] != "budget" || !ok || mapping["none"] != "low" || mapping["xhigh"] != "high" {
		t.Fatalf("unexpected extensions: %#v", ext)
	}
}
