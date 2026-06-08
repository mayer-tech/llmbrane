package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

var envVarRegex = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// Load reads a YAML configuration file, interpolates environment variables,
// and validates the structure.
func Load(filepath string) (*Config, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if err := interpolateNode(&root); err != nil {
		return nil, err
	}

	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

// interpolateNode recursively replaces ${VAR} with environment variables.
func interpolateNode(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var missingEnvErr error
		node.Value = envVarRegex.ReplaceAllStringFunc(node.Value, func(m string) string {
			varName := m[2 : len(m)-1]
			val, exists := os.LookupEnv(varName)
			if !exists {
				missingEnvErr = fmt.Errorf("missing environment variable: %s", varName)
				return m
			}
			return val
		})
		if missingEnvErr != nil {
			return missingEnvErr
		}
	}

	for _, child := range node.Content {
		if err := interpolateNode(child); err != nil {
			return err
		}
	}

	return nil
}

// Validate checks for structural correctness of the configuration.
func (c *Config) Validate() error {
	for pName, p := range c.Providers {
		for i, ep := range p.Endpoints {
			if ep.URL == "" {
				return fmt.Errorf("provider %q endpoint %d is missing URL", pName, i)
			}
			if _, err := url.Parse(ep.URL); err != nil {
				return fmt.Errorf("provider %q endpoint %d has invalid URL %q: %w", pName, i, ep.URL, err)
			}
			if ep.API == "" {
				return fmt.Errorf("provider %q endpoint %d is missing API definition", pName, i)
			}
		}
	}
	return nil
}
