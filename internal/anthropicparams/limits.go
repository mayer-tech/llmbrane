// Package anthropicparams resolves Anthropic thinking and output limits before dispatch.
package anthropicparams

import (
	"fmt"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

const (
	DefaultMaxOutputTokens = 16384
	AnswerHeadroomTokens   = 4096
)

// ConfigError identifies invalid endpoint settings rather than invalid client input.
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return e.Message }

// Thinking resolves endpoint mappings before calculating the manual budget.
func Thinking(effort *string, extensions map[string]any) (string, string, int, error) {
	mode := "adaptive"
	if raw, ok := extensions["thinking_mode"]; ok {
		var valid bool
		mode, valid = raw.(string)
		if !valid || (mode != "adaptive" && mode != "budget") {
			return "", "", 0, &ConfigError{Message: "invalid thinking_mode: expected adaptive or budget"}
		}
	}
	if effort == nil {
		return mode, "", 0, nil
	}
	eff := *effort
	if !canonical.IsValidReasoningEffort(eff) {
		return "", "", 0, fmt.Errorf("invalid reasoning_effort %q", eff)
	}
	switch mapping := extensions["reasoning_effort_mapping"].(type) {
	case map[string]any:
		if value, ok := mapping[eff]; ok {
			var valid bool
			eff, valid = value.(string)
			if !valid {
				return "", "", 0, &ConfigError{Message: "reasoning_effort_mapping values must be strings"}
			}
		}
	case map[string]string:
		if value, ok := mapping[eff]; ok {
			eff = value
		}
	}
	if !canonical.IsValidReasoningEffort(eff) {
		return "", "", 0, &ConfigError{Message: fmt.Sprintf("invalid mapped reasoning_effort %q", eff)}
	}
	budget := 0
	if mode == "budget" && eff != "none" {
		budget = canonical.ReasoningTokenBudget(eff)
	}
	return mode, eff, budget, nil
}

// MaxOutputTokens applies the configured or built-in default and checks a
// manual thinking budget. An explicit client limit is never changed.
func MaxOutputTokens(client, configured *int, budget int) (int, string, error) {
	if client != nil {
		if *client < 0 {
			return 0, "client", fmt.Errorf("max_tokens must not be negative")
		}
		if budget > 0 && *client <= budget {
			return 0, "client", fmt.Errorf("max_tokens (%d) must exceed thinking budget_tokens (%d)", *client, budget)
		}
		return *client, "client", nil
	}
	limit, source := DefaultMaxOutputTokens, "anthropic_fallback"
	if configured != nil {
		limit, source = *configured, "model_default"
	}
	if budget > 0 && limit < budget+AnswerHeadroomTokens {
		limit = budget + AnswerHeadroomTokens
		source += "_adjusted_for_thinking"
	}
	return limit, source, nil
}
