package anthropicparams

import "testing"

func TestMaxOutputTokensSource(t *testing.T) {
	client, configured := 512, 2048
	for _, tt := range []struct {
		name       string
		client     *int
		configured *int
		budget     int
		want       int
		source     string
	}{
		{"client", &client, &configured, 0, 512, "client"},
		{"model", nil, &configured, 0, 2048, "model_default"},
		{"fallback", nil, nil, 0, 16384, "anthropic_fallback"},
		{"model adjusted", nil, &configured, 8192, 12288, "model_default_adjusted_for_thinking"},
		{"fallback adjusted", nil, nil, 32768, 36864, "anthropic_fallback_adjusted_for_thinking"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, source, err := MaxOutputTokens(tt.client, tt.configured, tt.budget)
			if err != nil || got != tt.want || source != tt.source {
				t.Fatalf("limit = %d, source = %q, error = %v", got, source, err)
			}
		})
	}
}

func TestThinkingErrorClassification(t *testing.T) {
	high, invalid := "high", "invalid"
	for _, tt := range []struct {
		name       string
		effort     *string
		extensions map[string]any
		configErr  bool
	}{
		{"invalid client", &invalid, nil, false},
		{"invalid mode", &high, map[string]any{"thinking_mode": "invalid"}, true},
		{"invalid mapping value", &high, map[string]any{"reasoning_effort_mapping": map[string]any{"high": 1}}, true},
		{"invalid mapped effort", &high, map[string]any{"reasoning_effort_mapping": map[string]string{"high": "invalid"}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := Thinking(tt.effort, tt.extensions)
			if err == nil {
				t.Fatal("expected an error")
			}
			_, isConfigErr := err.(*ConfigError)
			if isConfigErr != tt.configErr {
				t.Fatalf("error = %T: %v", err, err)
			}
		})
	}
}
