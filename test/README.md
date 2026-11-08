# Manual tests

These tests call a running gateway and real providers. They are excluded from normal `go test ./...` runs by the `manual` build tag and may incur provider charges.

## Responses-to-Messages smoke test

Start LLMbrane with a configured, reasoning-capable `anthropic-messages` endpoint and valid provider credentials. Choose a model or alias routed to that endpoint; a native `openai-responses` route does not test this mapping. For example, with `config.example.yaml` configured:

```sh
LLMBRANE_MANUAL_MODEL=anthropic/claude-haiku-5-5 go test -tags=manual -v -count=1 ./test -run '^TestResponsesMessagesSmoke$'
```

Optional environment variables:

- `LLMBRANE_MANUAL_BASE_URL`: gateway base URL (default `http://localhost:8888`).
- `LLMBRANE_MANUAL_API_KEY`: bearer token for a gateway or proxy requiring authentication.

The test requests high reasoning effort and asks the model to derive the multiplication arguments from a sum and a difference. It executes the tool locally, then replays the complete response including the reasoning envelope and tool result. It checks for a final answer containing `56726`. The provider may decide not to produce a reasoning item or tool call; in that case the test fails because the intended mapping was not exercised, not necessarily because the gateway is broken. Try a more capable reasoning model or, if supported by the model, configure the endpoint with `thinking_mode: budget`. The test allows 16384 output tokens so the high-effort budget has room for an answer.

## Messages-to-Responses smoke test

Configure an `openai-responses` endpoint with valid provider credentials and a reasoning-capable model. For example, using the `gpt-responses` alias from `config.example.yaml`:

```sh
LLMBRANE_MANUAL_MODEL=openai/gpt-6.1-terra go test -tags=manual -v -count=1 ./test -run '^TestMessagesResponsesSmoke$'
```

The same `LLMBRANE_MANUAL_BASE_URL` and `LLMBRANE_MANUAL_API_KEY` environment variables apply; the latter is sent as `x-api-key` for this Messages request. The test uses the same arithmetic and validates the tool call, then replays all assistant content blocks including the thinking signature and sends the tool result. It requires a reasoning block, so a provider response without one fails the test even if the tool call succeeds.

The logged conversation ID can be used to inspect gateway debug logs, if enabled, for inbound requests and outbound provider requests. Debug logs can contain sensitive request and response data.
