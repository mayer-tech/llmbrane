# Reasoning replay, visibility and trust boundaries

[Issue overview](coordinated-mapping-issues.md) · [Remaining backlog](responses-message-mapping/mapping-open-questions.md)

Coordinate the contract for replayability and visibility in both directions. Missing provider protection material, invalid or incompatible envelopes, and omitted thinking are different cases and require separate decisions. Preserve the existing trust boundaries: never invent provider protection material, expose omitted content, or weaken envelope validation as an implicit fallback. Individual additional reasoning options (R2A-08 in the remaining backlog) may depend on this contract.

## Open issues

- [ ] **REPLAY-01 — Missing Responses encrypted reasoning.** Cross-protocol replay requires the native `encrypted_content`; when it is missing, the gateway rejects the artifact rather than inventing ciphertext. Decide whether a clearly labeled non-replayable mode is desirable and what clients should receive, without claiming lossless replay. Files: `internal/canonical/reasoning_envelope.go`, `internal/outbound/openairesponses/adapter.go`.

- [ ] **REPLAY-02 — Missing Anthropic signature or redacted data.** Anthropic thinking replay requires the native signature, and redacted thinking requires its data. Decide how to report or handle missing native protection material without synthesizing provider signatures or silently changing replay semantics. Files: `internal/canonical/reasoning_envelope.go`, `internal/inbound/openai/responses_anthropic.go`.

- [ ] **REPLAY-03 — Invalid, nested or incompatible envelopes.** Invalid `gw1` envelopes and artifacts from an incompatible provider/backend are rejected rather than forwarded as native provider data. Review error messages, route transitions and any desired recovery policy without weakening validation. Files: `internal/canonical/reasoning_envelope.go`, `internal/inbound/openai/responses_anthropic.go`, `internal/inbound/anthropic/handler.go`.

- [ ] **A2R-04 — Omitted thinking display.** `thinking.display: "omitted"` is rejected on the Responses route, while `"summarized"` is accepted. Decide how to preserve both the visibility request and replayable reasoning without exposing omitted content. File: `internal/inbound/anthropic/handler.go`.

## Decision alternatives

- **Keep fail-closed replay:** reject missing protection material and invalid or incompatible envelopes, while improving errors to distinguish these cases and explain available recovery options.
- **Offer an explicitly non-replayable display mode for missing material:** only if clients can distinguish it from replayable output and continuation semantics are defined. Never synthesize signatures, ciphertext or redacted data.
- **For omitted thinking:** keep rejecting until both visibility and replay are preserved, or design and verify an opaque-artifact path that does not expose omitted content. Do not silently substitute summarized display.

The `gw1` envelope is a transport format, not an authenticity guarantee. Never forward an incompatible envelope as native provider data or bypass validation after a parse failure. See the [mapping guide](../responses-messages-mapping.md#reasoning-envelope-and-trust-boundary).

## Verification

Test each native artifact type, missing material, malformed, nested and incompatible envelopes, route transitions, visibility and both streaming modes. Confirm client handling of any proposed non-replayable mode.
