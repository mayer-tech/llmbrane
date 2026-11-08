# Open mapping and protocol questions

This is a discussion backlog, not a list of approved implementation changes. Each checkbox represents a separately reviewable issue. Check it only after the behavior has been agreed, implemented where appropriate, and verified.

**Scope:** Responses ↔ Messages translation, related native paths and stream handling. Unless otherwise noted, possible defects below are findings from static inspection, not failures reproduced by a dedicated test. Existing restrictions are intentional safeguards, not claims that the requested behavior is impossible. Do not silently discard data or weaken replay validation without an explicit decision.

Related issues requiring coordinated decisions are tracked in [`../coordinated-mapping-issues.md`](../coordinated-mapping-issues.md).

## Suspected defects and robustness gaps

- [ ] **BUG-01 — Messages streaming over a Chat Completions backend.** The Chat outbound emits text and tool deltas without the canonical block-start events expected by the Messages inbound. The translated stream can contain `content_block_delta` without `content_block_start`, and tool IDs and names may be missing. This is separate from the Responses backend path. Reproduce text and multiple tool calls, then decide where to build and close ordered blocks. Files: `internal/outbound/openai/adapter.go`, `internal/inbound/anthropic/handler.go`.

- [ ] **BUG-03 — Anthropic `pause_turn` stop reason on the Responses route.** The current status mapping rejects this reason. Decide how a paused turn and its continuation should be represented in Responses, including replay and streaming. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **BUG-04 — Other Anthropic stop reasons on the Responses route.** The status mapper also rejects unhandled values, potentially including `model_context_window_exceeded`. Determine the supported reasons and appropriate incomplete/error semantics without treating unknown values as successful completion. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **BUG-07 — Native Anthropic `stop_sequence` is lost.** The concrete matched stop sequence is not propagated to the Messages response in either non-streaming or streaming, although the stop reason is mapped. Decide how to carry the value through the canonical model or native data and verify round trips. Files: `internal/outbound/anthropic/adapter.go`, `internal/outbound/anthropic/stream.go`, `internal/inbound/anthropic/handler.go`.

- [ ] **BUG-08 — Chat Completions EOF after a finish reason.** The Chat outbound returns both on `[DONE]` and on a clean scanner EOF without signaling the difference. If a finish reason arrived before an unexpected EOF, the inbound can emit a successful completion even though the provider stream was truncated. Decide how to validate terminal markers while considering provider variants and preserve error handling after a finish reason. Files: `internal/outbound/openai/adapter.go`, `internal/inbound/openai/handler.go`.

- [ ] **BUG-12 — Early native Responses SSE parser errors.** The native Responses inbound starts SSE error handling even if the first provider event is malformed and nothing has been sent yet, resulting in HTTP 200 with an SSE error rather than an HTTP provider error. Decide the desired pre-stream error contract and test malformed first events. Files: `internal/inbound/openai/responses.go`, `internal/outbound/openairesponses/native.go`.

## Responses input → Anthropic backend: deliberate restrictions

- [ ] **R2A-02 — Strict function tools.** Responses tools with `strict` enabled are rejected for Anthropic. Decide whether a corresponding Anthropic option can be mapped with equivalent validation and defaults. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-06 — Automatic truncation.** `truncation: "auto"` is rejected because there is no established equivalent policy. Decide whether the gateway should reject, implement, or explicitly translate truncation, including its effect on replay and ordering. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-07 — Request metadata.** Nonempty Responses `metadata` is rejected on the Anthropic route. Decide whether to map, preserve separately, or explicitly reject metadata rather than silently discard it. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-08 — Additional reasoning options.** `reasoning.effort` is handled, but other non-null Responses reasoning settings are rejected. Evaluate each relevant option and whether its semantics can be preserved on Anthropic. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-09 — System/developer messages inside a conversation.** Such items are accepted before conversation messages but rejected after them because Anthropic system instructions are assembled separately. Decide whether a safe ordered representation exists instead of silently moving instructions. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-11 — Output text annotations.** Nonempty Responses output-text annotations are rejected on replay. Decide whether they can be represented, preserved as metadata, or must remain unsupported without loss. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-12 — Output text log probabilities.** Nonempty output-text `logprobs` are rejected on replay. Decide whether preserving them matters for the gateway's replay contract and how to avoid silent loss. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-15 — Non-object function arguments.** A Responses function call must contain valid JSON object arguments for Anthropic; arrays, scalars and invalid JSON are rejected. Determine whether any additional valid cases have a safe mapping and retain explicit errors for invalid data. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-16 — Image detail and data URL variants.** Image detail values other than omitted or `auto`, and unsupported data URL forms, are rejected. Decide which Anthropic image representations preserve the requested behavior and how validation and size limits should work. File: `internal/inbound/openai/responses_anthropic.go`.

## Messages input → Responses backend: deliberate restrictions and loss

- [ ] **A2R-02 — `top_k` sampling.** Anthropic `top_k` is rejected for Responses backends. Decide whether there is an equivalent setting or whether explicit rejection remains necessary. File: `internal/inbound/anthropic/handler.go`.

- [ ] **A2R-03 — Stop sequences.** Nonempty Anthropic `stop_sequences` are rejected for Responses backends. Determine whether they can be supported without changing generation or streaming semantics. File: `internal/inbound/anthropic/handler.go`.

- [ ] **A2R-05 — Unsupported message and system blocks.** The Responses translation accepts only a subset of Anthropic content and system blocks; other block types are rejected. Evaluate each desired type and its ordering and replay behavior instead of discarding it. File: `internal/inbound/anthropic/handler.go`.

## Deferred compatibility work

- [ ] **LATER-01 — Real SDK and provider compatibility.** End-to-end checks with actual client SDKs and providers are explicitly out of scope for now. After the implementation and individual decisions are complete, define a separate compatibility matrix and test plan; mock tests do not establish real-provider compatibility.
