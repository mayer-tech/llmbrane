# Tool choice and parallelism

[Issue overview](coordinated-mapping-issues.md) · [Remaining backlog](responses-message-mapping/mapping-open-questions.md)

Evaluate required tool choice and disabled parallel tool use with a joint matrix of choice modes and tool availability. Strict function schemas (R2A-02 in the remaining backlog) are a separate constraint.

## Open issues

- [ ] **R2A-03 — Required tool choice.** `tool_choice: "required"` is rejected on the Anthropic route. Determine whether `any` provides equivalent semantics, including validation when no tools are supplied. File: `internal/inbound/openai/responses_anthropic.go`.

- [ ] **R2A-05 — Disable parallel tool calls.** `parallel_tool_calls: false` is rejected on the Anthropic route. Determine whether and when `disable_parallel_tool_use` has equivalent behavior for each tool-choice mode. File: `internal/inbound/openai/responses_anthropic.go`.

## Decision alternatives

- **Keep rejecting unsupported settings:** retain predictable behavior until their semantics are verified.
- **Map a verified subset:** evaluate the candidate `required` → `any` and `parallel_tool_calls: false` → `disable_parallel_tool_use` mappings for each choice mode and tool availability; reject unclear combinations. These mappings are not established equivalences.
- **Enforce a gateway contract where feasible:** validate provider output and report violations. Post-generation validation cannot prevent a provider from generating multiple tool calls; streaming needs a separate contract for violations discovered after events were sent.

## Verification

Build a matrix covering zero, one and multiple tools, automatic, required and named choices, parallelism settings and both streaming modes. Verify provider behavior separately from gateway mock tests.
