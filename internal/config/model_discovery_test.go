package config_test

import (
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestChatGPTDiscoveryValidation(t *testing.T) {
	endpoint := config.Endpoint{API: "openai-responses", URL: "https://api.openai.com/v1/responses", Auth: &config.EndpointAuth{Type: "chatgpt", CredentialsFile: "credentials.json", AutoDiscoverModels: true}}
	tests := []struct {
		name      string
		endpoints []config.Endpoint
		fail      bool
	}{
		{"single", []config.Endpoint{endpoint}, false},
		{"duplicate", []config.Endpoint{endpoint, endpoint}, true},
		{"invalid auth", []config.Endpoint{{API: endpoint.API, URL: endpoint.URL, Auth: &config.EndpointAuth{Type: "other", CredentialsFile: "credentials.json", AutoDiscoverModels: true}}}, true},
		{"wrong api", []config.Endpoint{{API: "openai-chat-completions", URL: endpoint.URL, Auth: endpoint.Auth}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{Providers: map[string]config.Provider{"p": {Endpoints: tt.endpoints}}}
			if err := cfg.Validate(); (err != nil) != tt.fail {
				t.Fatalf("validation error: %v", err)
			}
		})
	}
}
