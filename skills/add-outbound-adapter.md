# Add New Outbound Adapter Skill

Use this skill when adding a new LLM provider (e.g., Google Gemini, Mistral) to LLMbrane.

## Steps

1. **Create Package**: Create `internal/outbound/{provider}/` with `models.go` (provider request/response structs) and `adapter.go`.
2. **Implement the Interface**: The adapter must implement `core.OutboundAdapter` ([`internal/core/gateway.go`](../internal/core/gateway.go)):
    ```go
    ProcessChat(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (*canonical.CanonicalChatResponse, error)
    ProcessChatStream(ctx context.Context, req *canonical.CanonicalChatRequest, model string, endpoint config.Endpoint) (<-chan *canonical.CanonicalChatStreamEvent, error)
    ```
    Keep translation private: `translateRequest(...)` / `translateResponse(...)` helpers, with a separate stream parser for SSE.
3. **Follow the Constructor Pattern** (see any existing adapter):
    ```go
    type Adapter struct {
        client      *http.Client
        idleTimeout time.Duration
        retryPolicy httpretry.Policy
    }
    func NewAdapter() *Adapter                                              // defaults
    func NewAdapterWithTimeout(idleTimeout time.Duration) *Adapter          // custom idle timeout
    func NewAdapterWithTransport(idleTimeout time.Duration, transport http.RoundTripper) *Adapter // default retry policy
    func NewAdapterWithPolicy(idleTimeout time.Duration, transport http.RoundTripper, retryPolicy httpretry.Policy) *Adapter
    ```
    `transport` may be `nil` (uses `http.DefaultTransport`); the gateway passes `debuglog.NewTransport(nil)` for debug recording.
4. **HTTP Calls & Retries**: Use `resp, err := a.retryPolicy.Do(ctx, a.client, httpReq)` for every provider request — it transparently retries initial HTTP 429 responses according to the configured policy (see [Rate-Limit Handling](../README.md#rate-limit-handling)). Never retry after a streaming response has started. Wrap streaming bodies in `canonical.NewIdleTimeoutReader(resp.Body, a.idleTimeout)`.
5. **Error Handling**: Return `canonical.NewGatewayError(provider, model, message, statusCode, rawBody)` and pass provider 429 headers through `httpretry.ApplyHeaders(gatewayErr, resp.Header)` so the client receives a normalized `Retry-After`.
6. **Provider Quirks**: Use the generic `endpoint.Extensions` map for adapter-specific behavior (e.g. `inject_thought_signature_bypass` or `reasoning_effort_mapping` in the OpenAI adapter) instead of adding config fields.
7. **Register in `main.go`**: Wire the adapter in [`cmd/llmbrane/main.go`](../cmd/llmbrane/main.go):
    ```go
    gateway.RegisterOutboundAdapter("<api-shape>", outbound_{provider}.NewAdapterWithPolicy(requestTimeout, transport, retryPolicy))
    ```
    The gateway dispatches by the endpoint's `api` string — there is no factory in `internal/core`.
8. **Extend the Schema**: Add the new `api` shape to the `api` enum and any `extensions` keys to [`config.schema.json`](../config.schema.json).
9. **Add Tests**: Create `adapter_test.go` using `httptest` mock provider responses (see [`internal/outbound/openai/adapter_test.go`](../internal/outbound/openai/adapter_test.go)).
10. **Document Adapter**: Update the **Available Outbound Adapters** section in [`README.md`](../README.md). Describe the new `api` shape string, how to configure it, and document any `Extensions` your adapter supports.

## Reference
- `internal/outbound/openai/adapter.go`: standard OpenAI-style implementation.
- `internal/outbound/anthropic/adapter.go`: different API shape and stream parsing.
- `internal/outbound/bedrock/adapter.go`: signed requests and `extensions`-based credentials.
- `internal/outbound/retry/retry.go`: shared 429 retry policy injected into every adapter.
