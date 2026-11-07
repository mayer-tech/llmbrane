package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestDefaultMaxOutputTokensConfig(t *testing.T) {
	for _, tt := range []struct {
		name, value   string
		want          int
		present, fail bool
	}{
		{"absent", "", 0, false, false},
		{"positive", "default_max_output_tokens: 8192", 8192, true, false},
		{"zero", "default_max_output_tokens: 0", 0, true, true},
		{"negative", "default_max_output_tokens: -1", 0, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := fmt.Sprintf("providers:\n  test:\n    endpoints:\n      - api: anthropic-messages\n        url: https://example.com\n        models:\n          model:\n            target: model\n            %s\n", tt.value)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if (err != nil) != tt.fail {
				t.Fatalf("error = %v, want error %v", err, tt.fail)
			}
			if tt.fail {
				if !strings.Contains(err.Error(), "default_max_output_tokens") {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			got := cfg.Providers["test"].Endpoints[0].Models["model"].DefaultMaxOutputTokens
			if (got != nil) != tt.present || got != nil && *got != tt.want {
				t.Fatalf("default = %v, want %d (present=%t)", got, tt.want, tt.present)
			}
		})
	}
}
