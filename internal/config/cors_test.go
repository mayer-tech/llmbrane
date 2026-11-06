package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestCORSConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want []string
	}{
		{"omitted", "server: {}\n", nil},
		{"disabled", "server:\n  cors:\n    allowed_origins: []\n", []string{}},
		{"allowed", "server:\n  cors:\n    allowed_origins: [https://app.example.com, 'http://localhost:3000']\n", []string{"https://app.example.com", "http://localhost:3000"}},
		{"wildcard", "server:\n  cors:\n    allowed_origins: ['*']\n", []string{"*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Server.CORS.AllowedOrigins, tc.want) {
				t.Fatalf("allowed origins=%#v, want %#v", cfg.Server.CORS.AllowedOrigins, tc.want)
			}
		})
	}
}
