# Unknown fields and content blocks

[Issue overview](coordinated-mapping-issues.md) · [Remaining backlog](responses-message-mapping/mapping-open-questions.md)

Coordinate the preserve-or-reject policy to avoid silent loss as protocols evolve. The native Anthropic output path and the two cross-route input allowlists have different data and failure paths, so their implementation and tests remain separate.

## Open issues

- [ ] **BUG-06 — Unknown Anthropic output blocks.** The non-stream Anthropic outbound skips content block types it does not recognize. On the native Messages path this can silently lose content; the Responses cross-path has additional checks but does not make the native path safe. Decide whether to preserve native blocks or reject them, and test both paths. File: `internal/outbound/anthropic/adapter.go`.

- [ ] **R2A-13 — Unknown input item and content fields.** The Anthropic translation uses field allowlists and rejects unknown fields and unsupported item/content shapes. Decide the forward-compatibility and preservation policy; blindly accepting fields could silently drop meaning. Files: `internal/inbound/openai/responses.go`, `internal/inbound/openai/responses_anthropic.go`.

- [ ] **A2R-07 — Unknown request, tool and content settings.** Responses-route validation rejects fields outside its allowlists, including unsupported tool types and nested options. Decide how extensions and newly introduced fields should be handled while avoiding silent translation loss. File: `internal/inbound/anthropic/handler.go`.

## Decision alternatives

- **Reject unknown fields and blocks by default:** safest for translation, but requires updates as protocols evolve. On native output this would replace silent loss with an explicit error.
- **Preserve opaque blocks on a demonstrably lossless native path:** retain type, data and ordering, and reject when crossing a route that cannot represent them. Verify that the canonical/native path actually carries the complete data.
- **Add explicit or versioned extension support:** review fields and block types individually and add round-trip tests instead of broadly accepting unknown input. Unknown semantics must not be silently dropped.

## Verification

Test native output and both cross-route input paths separately, including unknown nested settings and blocks mixed with known content, ordering, explicit errors and round trips for any supported opaque data.
