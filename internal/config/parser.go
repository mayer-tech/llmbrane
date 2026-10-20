package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mayer-tech/llmbrane/internal/canonical"
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
	for aName, alias := range c.Aliases {
		if len(strings.Split(alias.Target, "/")) != 2 {
			return fmt.Errorf("alias %q has invalid target %q (expected provider/model)", aName, alias.Target)
		}
		if alias.DefaultReasoningEffort != "" && !canonical.IsValidReasoningEffort(alias.DefaultReasoningEffort) {
			return fmt.Errorf("alias %q has invalid default_reasoning_effort %q (valid: %v)",
				aName, alias.DefaultReasoningEffort, canonical.ValidReasoningEfforts)
		}
	}
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
			for mName, mConf := range ep.Models {
				if mConf.DefaultReasoningEffort != "" && !canonical.IsValidReasoningEffort(mConf.DefaultReasoningEffort) {
					return fmt.Errorf("provider %q endpoint %d model %q has invalid default_reasoning_effort %q (valid: %v)",
						pName, i, mName, mConf.DefaultReasoningEffort, canonical.ValidReasoningEfforts)
				}
			}
			if ep.RateLimit != nil {
				rl := ep.RateLimit
				if rl.TokensPerMinute < 0 {
					return fmt.Errorf("provider %q endpoint %d has negative rate_limit.tokens_per_minute", pName, i)
				}
				if rl.Utilization < 0 || rl.Utilization > 1 {
					return fmt.Errorf("provider %q endpoint %d has invalid rate_limit.utilization", pName, i)
				}
				if rl.OutputReservationTokens < 0 {
					return fmt.Errorf("provider %q endpoint %d has negative rate_limit.output_reservation_tokens", pName, i)
				}
				if rl.QueueTimeout != "" {
					d, err := time.ParseDuration(rl.QueueTimeout)
					if err != nil || d < 0 {
						return fmt.Errorf("provider %q endpoint %d has invalid rate_limit.queue_timeout %q", pName, i, rl.QueueTimeout)
					}
				}
			}
		}
	}
	return nil
}
