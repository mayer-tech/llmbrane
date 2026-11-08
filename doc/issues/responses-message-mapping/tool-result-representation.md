# Tool-result representation and error semantics

[Issue overview](coordinated-mapping-issues.md) · [Remaining backlog](responses-message-mapping/mapping-open-questions.md)

Coordinate the bidirectional representation of tool results, including content shape, call association and error state. Multimodal content and preservation of `is_error` remain distinct decisions; supporting one does not imply support for the other.

## Open issues

- [ ] **R2A-14 — Non-string function outputs.** The current function-output translation expects a string and cannot safely map other output representations. Decide which shapes, including multimodal results if applicable, should be supported and how to preserve them. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **A2R-06 — Images and other non-text tool results.** `tool_result` content currently supports strings or text blocks on the Responses route; image and other non-text blocks are rejected. Decide which representations Responses can carry without losing the result or its association with the call. File: `internal/inbound/anthropic/handler.go`.

- [ ] **A2R-08 — Lossy `tool_result.is_error` mapping.** A true error flag becomes the text prefix `Tool execution failed:\n` in the Responses function output. This communicates a failure but is not reliably reversible and can collide with ordinary text. Decide whether and how to preserve the boolean separately or define an explicit lossy contract. File: `internal/outbound/openairesponses/adapter.go`.

## Decision alternatives

- **Keep string/text-only translation:** preserves a limited predictable representation but excludes richer results.
- **Add content shapes incrementally:** verify destination support for each shape and preserve ordering and call association; reject unsupported content rather than flattening it silently.
- **Handle `is_error` separately:** carry an explicit flag only if the entire route can transport and recover it; otherwise choose between rejecting error-bearing results when losslessness is required and documenting an explicitly lossy text convention. The current prefix can collide with ordinary output and is not reversible.

## Verification

Cover both directions, multiple tool calls and call IDs, mixed text/non-text content, error and non-error results including prefix collisions, ordering and replay. Confirm provider support before accepting new shapes.
