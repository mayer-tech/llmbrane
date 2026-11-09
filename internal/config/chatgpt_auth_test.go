package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestChatGPTAuthConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("providers:\n  openai:\n    endpoints:\n      - api: openai-responses\n        url: https://api.openai.com/v1/responses\n        auth:\n          type: chatgpt\n          credentials_file: ./credentials.json\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	auth := cfg.Providers["openai"].Endpoints[0].Auth
	if auth == nil || auth.Type != "chatgpt" || auth.CredentialsFile != "./credentials.json" {
		t.Fatalf("unexpected authentication configuration: %+v", auth)
	}
}

func TestChatGPTAuthValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint config.Endpoint
		valid    bool
	}{
		{"valid", config.Endpoint{API: "openai-responses", URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "credentials.json"}}, true},
		{"unknown type", config.Endpoint{API: "openai-responses", URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "unknown", CredentialsFile: "credentials.json"}}, false},
		{"missing file", config.Endpoint{API: "openai-responses", URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: " "}}, false},
		{"wrong api", config.Endpoint{API: "openai-chat-completions", URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "credentials.json"}}, false},
		{"conflicting header", config.Endpoint{API: "openai-responses", URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "credentials.json"}, Headers: map[string]string{"authorization": "Bearer key"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Providers: map[string]config.Provider{"openai": {Endpoints: []config.Endpoint{tc.endpoint}}}}
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() error = %v, want valid = %v", err, tc.valid)
			}
		})
	}
}
