# Structured output in both directions

[Issue overview](coordinated-mapping-issues.md) · [Remaining backlog](responses-message-mapping/mapping-open-questions.md)

Compare the formats and their validation guarantees together, but decide each translation direction independently; support in one direction does not establish equivalence in the other.

## Open issues

- [ ] **R2A-01 — Structured output.** Responses `json_schema` and `json_object` output formats are rejected on the Anthropic route. Determine whether and how their constraints can be represented and enforced without promising equivalent schema guarantees. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **A2R-01 — Structured output configuration.** Anthropic `output_config.format` is rejected on Responses routes. Determine whether its format/schema semantics can be represented by Responses `text.format` and how guarantees differ. File: `internal/inbound/anthropic/handler.go`.

## Decision alternatives

- **Keep rejecting each unsupported direction:** retain explicit guarantees until equivalent formats and enforcement are established.
- **Support a verified subset per direction:** map only confirmed formats and schema features; reject unsupported constraints instead of silently weakening them. Evaluate `json_object`, `json_schema`, and Anthropic format configuration individually.
- **Offer an explicit best-effort mode:** document reduced guarantees and consider gateway-side validation. Validation can detect invalid output but does not make provider generation guarantees equivalent; streaming also needs an explicit failure contract if validation requires complete output.

Actual supported schema subsets and provider semantics remain to be verified; neither mapping direction should be assumed equivalent.

## Verification

Test supported and rejected schema features, invalid output and failure handling in streaming and non-streaming, independently for each direction. Real provider compatibility remains separate from mock testing.
