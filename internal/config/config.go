package config

import "gopkg.in/yaml.v3"

type Config struct {
	Version   int                 `yaml:"version"`
	Server    Server              `yaml:"server"`
	Logging   Logging             `yaml:"logging"`
	Retry     *Retry              `yaml:"retry,omitempty"`
	Aliases   map[string]Alias    `yaml:"aliases"`
	Policies  map[string]any      `yaml:"policies"`
	Providers map[string]Provider `yaml:"providers"`
}

// Alias maps a client-facing model name to a 'provider/model' target with
// optional per-alias overrides.
type Alias struct {
	Target string `yaml:"target"`
	// DefaultReasoningEffort overrides the target model's default when set.
	// It is applied when a request does not carry an explicit reasoning
	// effort. Empty means the target model's default is kept.
	DefaultReasoningEffort string `yaml:"default_reasoning_effort,omitempty"`
}

// UnmarshalYAML accepts both the shorthand string form ("provider/model")
// and the full mapping form with a target and optional overrides.
func (a *Alias) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		a.Target = value.Value
		return nil
	}
	type aliasConf Alias
	var conf aliasConf
	if err := value.Decode(&conf); err != nil {
		return err
	}
	*a = Alias(conf)
	return nil
}

type Server struct {
	Listen         string `yaml:"listen"`
	RequestTimeout string `yaml:"request_timeout"`
}

type Logging struct {
	Level          string `yaml:"level"`
	DebugDir       string `yaml:"debug_dir"`
	DebugRetention string `yaml:"debug_retention"`
}

// Retry configures provider-side HTTP 429 retry behavior. Unset fields keep
// the built-in defaults: 6 retries, 1s base delay, 250ms jitter, 60s
// maximum delay. Set max_retries to 0 to disable retries entirely.
type Retry struct {
	MaxRetries *int   `yaml:"max_retries,omitempty"`
	BaseDelay  string `yaml:"base_delay,omitempty"`
	Jitter     string `yaml:"jitter,omitempty"`
	MaxDelay   string `yaml:"max_delay,omitempty"`
}

type Provider struct {
	Endpoints []Endpoint `yaml:"endpoints"`
}

type Endpoint struct {
	API          string               `yaml:"api"`
	URL          string               `yaml:"url"`
	Headers      map[string]string    `yaml:"headers"`
	Models       map[string]ModelConf `yaml:"models"`
	Capabilities []string             `yaml:"capabilities"`
	Extensions   map[string]any       `yaml:"extensions"`
	RateLimit    *RateLimit           `yaml:"rate_limit,omitempty"`
}

// RateLimit configures the optional local TPM queue for an endpoint.
// A queue is only active when TokensPerMinute is positive or the provider
// supplies a supported token-limit response header.
type RateLimit struct {
	TokensPerMinute         int     `yaml:"tokens_per_minute,omitempty"`
	Utilization             float64 `yaml:"utilization,omitempty"`
	QueueTimeout            string  `yaml:"queue_timeout,omitempty"`
	OutputReservationTokens int     `yaml:"output_reservation_tokens,omitempty"`
}

type ModelConf struct {
	Target       string   `yaml:"target"`
	Capabilities []string `yaml:"capabilities,omitempty"`
	// DefaultReasoningEffort is applied when a request does not carry an
	// explicit reasoning effort. Empty means the provider default is kept.
	DefaultReasoningEffort string `yaml:"default_reasoning_effort,omitempty"`
}
