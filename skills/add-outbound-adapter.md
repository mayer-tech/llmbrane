# Add New Outbound Adapter Skill

Use this skill when adding a new LLM provider (e.g., Google Gemini, Mistral) to LLMbrane.

## Steps

1. **Create Package**: Create `internal/outbound/{provider}/`.
2. **Define Models**: Add `models.go` with the provider's specific request/response structs.
3. **Implement Adapter**: Add `adapter.go` with a struct implementing the (implied) adapter interface:
    - `NewAdapter(cfg config.ProviderConfig) *Adapter`
    - `TranslateRequest(req *canonical.CanonicalChatRequest) (providerReq, error)`
    - `TranslateResponse(resp providerResp) (*canonical.CanonicalChatResponse, error)`
    - `HandleStream(ctx, providerReq) (<-chan *canonical.CanonicalChatStreamEvent, error)`
    - *Note:* If your adapter needs to workaround provider quirks, utilize the generic `endpoint.Extensions` map from the configuration (e.g., `inject_thought_signature_bypass` or `reasoning_effort_mapping` in the OpenAI adapter).
4. **Error Handling**: Use `canonical.MapHTTPStatusCode` or custom logic to return `canonical.GatewayError`.
5. **Register in Core**: Update `internal/core/gateway.go` to recognize the new provider in the factory method (if applicable).
6. **Add Tests**: Create `adapter_test.go` with mock provider responses.
7. **Document Adapter**: Update the `README.md` under the **Available Outbound Adapters** section. Describe the new `api` shape string, how to configure it, and document any `Extensions` your adapter supports.

## Reference
- See `internal/outbound/openai/adapter.go` for a standard implementation.
- See `internal/outbound/anthropic/adapter.go` for handling different API shapes.
