# API mappings

LLMbrane resolves the requested alias or `provider/model` to a configured endpoint and translates requests and responses through its canonical model. The endpoint's `api` selects the outbound protocol; a model or provider name alone does not guarantee a particular API shape. See the [mapping matrix](../README.md#api-mapping-support) for supported routes.

Native routes are not transparent proxies and do not implement every feature of their respective APIs. Translation can reject unsupported fields, omit them, or change their semantics, depending on the route. Model capabilities, provider behavior, endpoint extensions, and gateway policies can impose further restrictions. The following describes gateway behavior, not a guarantee of compatibility with every provider or SDK.

## OpenAI Chat Completions inbound

`POST /openai/v1/chat/completions` can target all four configured outbound API shapes. The Chat Completions route uses canonical translation even when the backend also speaks Chat Completions. Text, images, and function calls are mapped where supported, but not all native request or response fields are preserved. In particular, the Chat outbound does not translate the canonical response format or all tool choices.

Cross-protocol routes are also limited by the destination adapter's supported content and parameters. For example, Bedrock Converse request translation accepts supported base64 image data but does not map image URLs; unsupported content may be omitted. Invalid tool-call arguments can be replaced with an empty object. Check the target model and provider before relying on equivalent behavior.

## OpenAI Responses inbound

`POST /openai/v1/responses` accepts an alias or `provider/model` routed to either an `openai-responses` or an `anthropic-messages` endpoint. The request goes through normal gateway policies, model defaults, rate limits, retries, and cancellation. Chat Completions and Bedrock Converse backends are rejected.

The supported stateless input is a string or an ordered array of message, reasoning, function call, and string function call output items. `store: true`, `previous_response_id`, unsupported top-level fields, built-in tools, item references, audio/file content, and other item types are rejected. The native Responses route forwards supported settings and preserves extra fields on supported items; unsupported provider output types or content cause an error. Buffered non-stream Responses provider bodies are limited to 40 MiB for successful responses and 1 MiB for error responses. Oversized successful responses fail with a provider error. The backend still validates its native fields and model support. Endpoint `omit_params` and `reasoning_effort_mapping` apply as usual.

### Responses to Anthropic Messages

Text, image URLs or base64 image data, function tools and results, and reasoning are mapped to Anthropic Messages, including streaming. For examples of both mapping directions and replay, see [Responses ↔ Messages mapping](responses-messages-mapping.md). Restrictions include:

- System and developer messages must precede conversation messages and contain only text. Images support only `detail: auto` or omitted detail.
- Structured output (`json_object` or `json_schema`), nonempty metadata, reasoning settings other than effort, strict function tools, `tool_choice: "required"`, `parallel_tool_calls: false`, and `truncation: "auto"` are not supported. Named tool choices must refer to declared tools. `tool_choice: "none"` disables tool calls while preserving declared tools; provider support may vary.
- Function call arguments must be JSON objects, function outputs must be strings, and replayed items must be completed. Nonempty output text annotations or logprobs and unsupported item/content fields are rejected. Unsupported provider output blocks also cause an error.
- A valid assistant refusal replays as ordinary assistant text; its distinct refusal semantics are lost. Anthropic refusal output has additional restrictions on the content it can contain.

For streaming, `response.created` is emitted immediately, but subsequent output events are buffered until the provider completes. This delays text, reasoning, and tool events as well as refusals; buffering is bounded, and errors or cancellation can prevent delivery of buffered output.

## Anthropic Messages inbound

`POST /anthropic/v1/messages` can target all four outbound API shapes. On the native Anthropic Messages route, selected native blocks and metadata are preserved, but the gateway still translates requests and responses and does not promise complete protocol coverage. For example, the concrete matched stop sequence is not propagated in the response. Other outbound APIs require cross-protocol translation and may omit or alter features.

### Messages to OpenAI Responses

Text, user images (URL or base64), function tools, text tool results, reasoning, and streaming are mapped to Responses. System content must be text, messages must not be empty, and a supplied `max_tokens` must be positive. `output_config.format`, `top_k`, nonempty `stop_sequences`, unsupported content blocks, and non-text tool results are rejected on this route. Tool-use inputs must be JSON objects; an error tool result is sent as text prefixed with `Tool execution failed:`, so its distinct error flag is not preserved. The provider may reject unsupported settings or model capabilities.

Messages streaming over Chat Completions has a separately identified, not yet independently reproduced, risk of missing content-block start events and tool metadata. See the [open mapping questions](issues/responses-message-mapping/mapping-open-questions.md) for this and other unresolved findings.

## Continuation and reasoning replay

LLMbrane does not store conversation state. **The client must retain and replay the complete prior history and output in order**, including reasoning, messages, tool calls, and tool results, on each continuation. Responses clients must send complete output items back in `input`, not reconstruct them from text deltas. In streaming mode, retain complete items from `response.output_item.done` or the terminal response's `output`. Anthropic clients must reconstruct and replay complete assistant content blocks, including thinking signatures, from the response or stream; a client that drops or rewrites these artifacts cannot rely on reasoning continuity, particularly during tool use.

On the native OpenAI Responses route, the gateway sends `store: false` and automatically includes `reasoning.encrypted_content`. Native SSE event JSON and provider metadata are passed through (SSE comments and transport fields such as `id` and `retry` are not); events are limited to 10 MiB. `response.failed`, `error`, and truncated streams do not produce a synthetic successful completion. `response.incomplete` remains a distinct terminal event. The Anthropic route instead produces translated Responses items and events.

The bidirectional mapping preserves provider-native reasoning for replay, not for conversion between backend providers:

- **Responses client → Anthropic backend:** Anthropic `thinking` and `redacted_thinking` blocks become Responses reasoning items. Their `encrypted_content` contains a gateway envelope carrying the original block; readable thinking appears in the summary. Replay the full items to the same compatible Anthropic backend/model.
- **Anthropic Messages client → Responses backend:** Responses reasoning items become Anthropic `thinking` blocks. The summary is exposed as thinking text, and the block's `signature` contains a gateway envelope carrying the complete original reasoning item, including its ciphertext. Empty summaries can still produce a thinking block with an empty `thinking` value. Replay the complete blocks to the same compatible Responses backend/model. The gateway requests encrypted content and, when reasoning is enabled, a summary where applicable. If a returned reasoning item lacks valid, nonempty `encrypted_content`, translation fails rather than falling back to a summary: a non-streaming request returns a provider error, and an already-started stream ends with an error rather than a successful completion.

These `gw1.` envelopes are **reversible transport containers, not encryption or authenticated signatures**. Anyone with an envelope can decode or modify its contents; do not treat it as proof of origin or integrity. The original provider must validate its own protected values, and successful gateway parsing does not guarantee provider acceptance. The envelope origin identifies the artifact format, not a trusted identity or routing instruction. Envelopes and native reasoning may contain sensitive data and may appear in enabled debug logs; protect transport and stored conversation history according to your deployment's threat model. Reasoning artifacts are not portable across backend providers or necessarily across models; incompatible or malformed envelopes are rejected. See the [mapping guide](responses-messages-mapping.md#reasoning-envelope-and-trust-boundary) for the replay trust boundary.

If a compatible Responses backend rejects the include field, endpoint extension `disable_encrypted_reasoning: true` disables automatic inclusion; an explicit Responses client include is still forwarded. This does not provide encrypted reasoning continuity or relax the strict ciphertext requirement for Anthropic clients routed to Responses. See also [Reasoning Effort Support](../README.md#reasoning-effort-support).
