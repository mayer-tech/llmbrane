# Coordinated mapping and protocol issues

This is an overview of discussion backlogs, not a list of approved implementation changes. The issues were moved from [`responses-message-mapping/mapping-open-questions.md`](responses-message-mapping/mapping-open-questions.md) because their decisions should be coordinated. Each linked document contains the original separately reviewable checkboxes, decision alternatives and verification considerations. A group does not require a single implementation or an identical decision in both directions. Check an issue only after the behavior has been agreed, implemented where appropriate, and verified.

**Scope:** Responses ↔ Messages translation, related native paths and stream handling. Unless otherwise noted, possible defects are findings from static inspection, not failures reproduced by a dedicated test. Existing restrictions are intentional safeguards. Do not silently discard data or weaken replay validation without an explicit decision.

## [Empty and incomplete output and replay](empty-incomplete-output-replay.md)

Coordinate EMPTY-01, EMPTY-02 and EMPTY-03: output validity, ordering, status and replay across both directions and streaming modes.

## [Refusal representation and replay](refusal-representation-replay.md)

Coordinate BUG-02 and R2A-10: refusal generation, representation and replay.

## [Structured output in both directions](structured-output-mapping.md)

Compare R2A-01 and A2R-01: supported formats and validation guarantees, with separate decisions per direction.

## [Tool choice and parallelism](tool-choice-parallelism.md)

Coordinate R2A-03 and R2A-05: required tool choice and parallelism across choice modes and tool availability.

## [Tool-result representation and error semantics](tool-result-representation.md)

Coordinate R2A-14, A2R-06 and A2R-08: content shape, call association and error state.

## [Reasoning replay, visibility and trust boundaries](reasoning-replay-trust.md)

Coordinate REPLAY-01 through REPLAY-03 and A2R-04: replayability, visibility and protection material.

## [Unknown fields and content blocks](unknown-fields-content-blocks.md)

Coordinate BUG-06, R2A-13 and A2R-07: preserve-or-reject policy for native output and cross-route input.
