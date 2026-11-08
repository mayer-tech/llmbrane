# Refusal representation and replay

[Issue overview](coordinated-mapping-issues.md) · [Remaining backlog](responses-message-mapping/mapping-open-questions.md)

The representation selected for an Anthropic refusal affects how the resulting Responses item can be replayed without losing its meaning. Cover generation and replay together, including streaming. Paused turns (BUG-03 in the remaining backlog) require a separate continuation decision.

## Implemented policy

Anthropic `stop_reason: refusal` maps to a completed Responses response containing a standard `refusal` content part. Only exactly one non-whitespace text block is accepted; thinking, tool calls, and multiple text blocks are rejected, because attributing the refusal to only part of mixed content would be ambiguous. The same rule applies to streaming and non-streaming output.

An assistant refusal part replays to Anthropic as ordinary assistant text, preserving its position among the other message parts. Anthropic has no equivalent input refusal block, so the refusal stop reason and its distinct semantics are lost on replay. Other roles, empty or invalid refusal text, and unknown refusal fields are rejected. Native OpenAI Responses replay remains unchanged.

For Anthropic-backed Responses streams, the gateway emits `response.created` immediately but buffers all subsequent output events until the provider completes. This delays ordinary text, reasoning, and tool events as well as refusals; buffering is bounded and errors or cancellation can prevent the buffered output from being delivered.

- [x] **BUG-02 — Anthropic `refusal` stop reason on the Responses route.**
- [x] **R2A-10 — Refusal replay.**

## Verification

Test refusal output in streaming and non-streaming modes, item ordering, replay and unsupported representations. Confirm destination and client behavior before claiming compatibility.
