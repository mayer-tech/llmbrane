package config

type Config struct {
	Version   int                 `yaml:"version"`
	Server    Server              `yaml:"server"`
	Logging   Logging             `yaml:"logging"`
	Aliases   map[string]string   `yaml:"aliases"`
	Policies  map[string]any      `yaml:"policies"`
	Providers map[string]Provider `yaml:"providers"`
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
