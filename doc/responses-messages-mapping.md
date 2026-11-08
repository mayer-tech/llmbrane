# OpenAI Responses ↔ Anthropic Messages mapping

This document explains how LLMbrane translates between an OpenAI Responses client and an Anthropic Messages backend, and between an Anthropic Messages client and an OpenAI Responses backend. Both routes use the gateway's canonical representation; they are not transparent proxies or complete equivalents of the native APIs. See the [API mapping overview](README.md) for supported routes and restrictions.

The examples are schematic. Model names, IDs, signatures, ciphertexts and `gw1.<ENVELOPE>` are placeholders, not valid credentials or runnable replay artifacts. Event sequences omit protocol fields and are not exhaustive. Each direction is a separate conversation using a fixed backend, not a model switch during a conversation.

## Principles

- **Ordered conversation:** Messages, reasoning artifacts, tool calls and results are translated in order, within the supported subset of each API.
- **Stateless continuation:** LLMbrane does not store the conversation. The client must retain and replay the full history and completed assistant output, including reasoning and tool artifacts.
- **Provider-native reasoning:** The gateway transports the original reasoning artifact for replay to the same compatible backend and model. It does not translate a provider's protected reasoning into another provider's format.
- **Explicit limits:** Unsupported input can be rejected, and some semantics cannot be represented in the other protocol. Successful translation does not guarantee that a provider or client SDK accepts the result.

## Responses client → Anthropic Messages backend

A Responses request with an input message becomes an Anthropic Messages request. Routing selects the backend and its actual model ID. The following shows the conversation content; other required fields and settings depend on the request and endpoint configuration.

Responses input:

```json
{"input": [{"role": "user", "content": "What is 17 × 23?"}]}
```

Anthropic messages:

```json
{"messages": [{"role": "user", "content": "What is 17 × 23?"}]}
```

For a response containing Anthropic thinking followed by text, the conceptual mapping is:

| Anthropic output | Responses output |
| --- | --- |
| `thinking` block with native `signature` | `reasoning` item; visible thinking becomes a summary, and the complete native block is wrapped in `encrypted_content` |
| `redacted_thinking` block | `reasoning` item with an empty summary; the native block is wrapped in `encrypted_content` |
| `text` block | Assistant `message` item with `output_text` |
| `tool_use` block | `function_call` item; `tool_use.id` becomes `call_id` |

For example, a completed native thinking block is wrapped rather than turned into OpenAI ciphertext:

```json
{
  "v": 1,
  "origin": "anthropic-messages",
  "artifact": {
    "type": "thinking",
    "thinking": "Calculate the product.",
    "signature": "CLAUDE_SIGNATURE"
  }
}
```

The gateway encodes this object as `gw1.<ENVELOPE>` and places it in the completed Responses reasoning item's `encrypted_content`. The client retains the complete item, followed by the assistant message and any tool calls. On continuation it sends those items, the original user input, any tool results and the new user input in order. The gateway validates and decodes the envelope and restores the original Anthropic block, including its native signature, for the compatible Anthropic backend.

### Streaming

The provider's block events are translated into Responses item, content and delta events. Thinking deltas map to reasoning summary deltas; text deltas map to output text deltas. The complete native thinking block is required before the gateway can produce the replayable `response.output_item.done` item.

**Delivery is buffered:** `response.created` is sent immediately, but subsequent translated events, including text, thinking and tools, are buffered until the provider completes. Buffering is bounded; an error or cancellation can prevent buffered events from reaching the client. Clients should retain complete items from `response.output_item.done` or the terminal response's `output`, not reconstruct replay items from deltas alone.

## Anthropic Messages client → Responses backend

An Anthropic user message becomes a Responses input message. The gateway uses stateless Responses requests (`store: false`) and normally asks the backend to include `reasoning.encrypted_content`. The exact reasoning settings depend on the client request, model defaults and endpoint configuration; adaptive thinking without an explicit effort does not itself imply a specific effort.

Anthropic messages:

```json
{"messages": [{"role": "user", "content": "What is 17 × 23?"}]}
```

Responses input:

```json
{"input": [{"role": "user", "content": "What is 17 × 23?"}], "store": false,
 "include": ["reasoning.encrypted_content"]}
```

| Responses output | Anthropic output |
| --- | --- |
| `reasoning` item with native `encrypted_content` | `thinking` block; summary text is visible as thinking, and the complete item is wrapped in `signature` |
| `reasoning` item with an empty summary | `thinking` block with empty text and a wrapped `signature` |
| Assistant `message` with `output_text` | `text` block |
| `function_call` item | `tool_use` block; Responses `call_id` becomes `tool_use.id` |

For example, the synthetic signature carries this envelope, including the original Responses item:

```json
{
  "v": 1,
  "origin": "openai-responses",
  "artifact": {
    "type": "reasoning",
    "id": "rs_example",
    "summary": [{"type": "summary_text", "text": "Calculate the product."}],
    "encrypted_content": "OPENAI_CIPHERTEXT"
  }
}
```

The client receives a `thinking` block whose `signature` is `gw1.<ENVELOPE>`. It must replay the complete block and the rest of the conversation, including any tool calls and results. The gateway validates and unwraps the original Responses item for the compatible Responses backend. It does not send the synthetic gateway signature as provider ciphertext. If the provider's reasoning item lacks valid, nonempty native `encrypted_content`, translation fails; the gateway does not fabricate replay data or fall back to a non-replayable summary.

### Streaming

Responses reasoning-summary deltas can become Anthropic `thinking_delta` events, and text deltas can become `text_delta` events. The thinking block remains open until the completed Responses reasoning item supplies the native ciphertext; the gateway then emits the envelope as a `signature_delta` and closes the block. A client must reconstruct and retain complete blocks, including empty thinking blocks and signatures, for continuation.

Multiple output items and parallel tool calls may require bounded buffering to serialize Messages blocks in order. Interleaved tool arguments are tracked per item; clients correlate tool results by call ID, not by output item ID or array position. An interrupted or invalid stream must not be treated as a successful completed turn. If output has already started, a translation failure ends the stream with an error rather than a normal completion.

## Tool calls and results

A function call and its result use corresponding call IDs across the two protocols:

Responses output item and subsequent input item:

```json
[
  {"type": "function_call", "call_id": "call_1", "name": "multiply", "arguments": "{\"a\":17,\"b\":23}"},
  {"type": "function_call_output", "call_id": "call_1", "output": "391"}
]
```

Anthropic assistant and subsequent user content blocks:

```json
[
  {"type": "tool_use", "id": "call_1", "name": "multiply", "input": {"a": 17, "b": 23}},
  {"type": "tool_result", "tool_use_id": "call_1", "content": "391"}
]
```

The client executes the tool and sends its result in the next request, together with the preceding assistant output and complete history. A Responses item `id` is distinct from `call_id`; tool results correlate using the latter. Arguments must be JSON objects for the Anthropic route. Messages-to-Responses currently accepts text tool results; an Anthropic error result is represented by a text prefix rather than a separate error flag.

## Reasoning envelope and trust boundary

A gateway envelope is `gw1.` followed by unpadded base64url-encoded JSON with `v`, `origin` and the complete native `artifact`. The decoded envelope is limited to 1 MiB. Each completed reasoning block or item has its own envelope; visible summary text is not a substitute for the original protected artifact. Multiple artifacts must be retained in order.

**The envelope is reversible transport, not encryption, an authenticated signature or proof of origin.** Anyone who has it can decode or modify it. The gateway validates the version, structure, origin, artifact type and required provider data; invalid, nested or incompatible envelopes are rejected. The provider is responsible for validating its own protected values and may still reject a replay. Protect these values and any debug logs as potentially sensitive data.

Replay depends on the client returning complete, unchanged artifacts and on the original provider's model and context rules. Artifacts are not generally portable between providers or necessarily between models. If a client drops an empty thinking block, signature or encrypted item, reasoning continuity can fail. Actual SDK and provider compatibility must be verified separately.

## Restrictions and further reading

The mapping supports a subset of both protocols. For example, Responses → Messages rejects structured output, strict function tools, `tool_choice: "required"` and `parallel_tool_calls: false`; Messages → Responses rejects `output_config.format`, `top_k`, nonempty stop sequences and non-text tool results. Some refusal semantics are lost on replay. See the [API mapping overview](README.md) for the full documented restrictions and streaming behavior.

- [Open mapping questions](issues/responses-message-mapping/mapping-open-questions.md)
- [Reasoning replay and trust issues](issues/responses-message-mapping/reasoning-replay-trust.md)
- [OpenAI Responses streaming events](https://developers.openai.com/api/reference/resources/responses/streaming-events)
- [Anthropic Messages streaming](https://platform.claude.com/docs/en/build-with-claude/streaming)
