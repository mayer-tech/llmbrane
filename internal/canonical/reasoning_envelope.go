package canonical

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reasoning envelopes are reversible transport containers, not authenticated data.
// The bound applies to the decoded JSON, including the envelope metadata.
const maxReasoningEnvelopeBytes = 1 << 20

const reasoningEnvelopePrefix = "gw1."

type reasoningEnvelope struct {
	Version  int             `json:"v"`
	Origin   string          `json:"origin"`
	Artifact json.RawMessage `json:"artifact"`
}

// IsReasoningEnvelope recognizes versioned gateway prefixes, including versions
// this gateway does not support. Ordinary provider values beginning with "gw"
// are not reserved unless followed by decimal digits and a dot.
func IsReasoningEnvelope(value string) bool {
	if !strings.HasPrefix(value, "gw") {
		return false
	}
	i := 2
	for i < len(value) && value[i] >= '0' && value[i] <= '9' {
		i++
	}
	return i > 2 && i < len(value) && value[i] == '.'
}

// EncodeReasoningEnvelope wraps a complete native reasoning artifact without
// authenticating it. Provider validation of protected values remains necessary.
func EncodeReasoningEnvelope(provider string, artifact json.RawMessage) (string, error) {
	if len(artifact) > maxReasoningEnvelopeBytes {
		return "", fmt.Errorf("reasoning artifact exceeds size limit")
	}
	if _, err := validateReasoningArtifact(provider, artifact); err != nil {
		return "", err
	}
	origin := "anthropic-messages"
	if provider == "openai" {
		origin = "openai-responses"
	}
	// Insert the validated JSON value directly: json.Marshal compacts RawMessage
	// and HTML-escapes strings, changing the native artifact's representation.
	data := append([]byte(`{"v":1,"origin":"`+origin+`","artifact":`), bytes.TrimSpace(artifact)...)
	data = append(data, '}')
	if len(data) > maxReasoningEnvelopeBytes {
		return "", fmt.Errorf("reasoning envelope exceeds size limit")
	}
	return reasoningEnvelopePrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeReasoningEnvelope validates and unwraps a supported native artifact.
func DecodeReasoningEnvelope(value string) (*CanonicalProviderData, error) {
	if !strings.HasPrefix(value, reasoningEnvelopePrefix) {
		return nil, fmt.Errorf("unsupported or invalid reasoning envelope prefix")
	}
	encoded := value[len(reasoningEnvelopePrefix):]
	if len(encoded) > base64.RawURLEncoding.EncodedLen(maxReasoningEnvelopeBytes) {
		return nil, fmt.Errorf("reasoning envelope exceeds size limit")
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > maxReasoningEnvelopeBytes {
		return nil, fmt.Errorf("invalid or oversized reasoning envelope encoding")
	}
	// Reject non-canonical encodings, including embedded newlines and padding.
	if base64.RawURLEncoding.EncodeToString(data) != encoded {
		return nil, fmt.Errorf("invalid reasoning envelope encoding")
	}
	fields, err := reasoningObject(data)
	if err != nil || len(fields) != 3 || fields["v"] == nil || fields["origin"] == nil || fields["artifact"] == nil {
		return nil, fmt.Errorf("invalid reasoning envelope JSON")
	}
	var envelope reasoningEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Version != 1 || len(envelope.Artifact) == 0 {
		return nil, fmt.Errorf("invalid or unsupported reasoning envelope version")
	}
	var provider string
	switch envelope.Origin {
	case "anthropic-messages":
		provider = "anthropic"
	case "openai-responses":
		provider = "openai"
	default:
		return nil, fmt.Errorf("unsupported reasoning envelope origin")
	}
	format, err := validateReasoningArtifact(provider, envelope.Artifact)
	if err != nil {
		return nil, err
	}
	return &CanonicalProviderData{Provider: provider, Format: format, Data: envelope.Artifact}, nil
}

func validateReasoningArtifact(provider string, artifact json.RawMessage) (string, error) {
	fields, err := reasoningObject(artifact)
	if err != nil {
		return "", fmt.Errorf("invalid reasoning artifact JSON")
	}
	getString := func(key string, nonempty bool) bool {
		raw, ok := fields[key]
		if !ok || len(raw) == 0 || raw[0] != '"' {
			return false
		}
		var value string
		return json.Unmarshal(raw, &value) == nil && (!nonempty || value != "")
	}
	protectedValue := func(key string) bool {
		var value string
		return json.Unmarshal(fields[key], &value) == nil && !IsReasoningEnvelope(value)
	}
	if !getString("type", true) {
		return "", fmt.Errorf("reasoning artifact has no valid type")
	}
	var kind string
	_ = json.Unmarshal(fields["type"], &kind)
	switch provider {
	case "anthropic":
		switch kind {
		case "thinking":
			if !getString("thinking", false) || !getString("signature", true) || !protectedValue("signature") {
				return "", fmt.Errorf("thinking artifact requires a native thinking signature")
			}
		case "redacted_thinking":
			if !getString("data", true) || !protectedValue("data") {
				return "", fmt.Errorf("redacted thinking artifact requires native data")
			}
		default:
			return "", fmt.Errorf("unsupported Anthropic reasoning artifact type %q", kind)
		}
		return kind, nil
	case "openai":
		if kind != "reasoning" || !getString("id", true) || !getString("encrypted_content", true) || !protectedValue("encrypted_content") {
			return "", fmt.Errorf("OpenAI reasoning artifact requires type, id and native encrypted_content")
		}
		var summary []json.RawMessage
		raw, ok := fields["summary"]
		if !ok || len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &summary) != nil {
			return "", fmt.Errorf("OpenAI reasoning artifact requires a summary array")
		}
		for _, part := range summary {
			item, err := reasoningObject(part)
			if err != nil {
				return "", fmt.Errorf("invalid reasoning summary item")
			}
			var partType, text string
			if json.Unmarshal(item["type"], &partType) != nil || partType != "summary_text" || len(item["text"]) == 0 || item["text"][0] != '"' || json.Unmarshal(item["text"], &text) != nil {
				return "", fmt.Errorf("invalid reasoning summary item")
			}
		}
		return "responses.item", nil
	default:
		return "", fmt.Errorf("unsupported reasoning provider %q", provider)
	}
}

// Reject duplicate keys at the levels used for replay validation. This avoids
// accepting an artifact whose interpretation depends on the JSON parser used.
func reasoningObject(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid UTF-8 in JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	fields := make(map[string]json.RawMessage)
	folded := make(map[string]bool)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("invalid object key")
		}
		// Match encoding/json's Unicode case folding in linear time so that
		// different consumers cannot disagree about replay-relevant keys.
		fold := strings.Map(func(r rune) rune {
			min := r
			for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
				if next < min {
					min = next
				}
			}
			return min
		}, key)
		if folded[fold] {
			return nil, fmt.Errorf("duplicate object key %q", key)
		}
		folded[fold] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON")
	}
	return fields, nil
}
