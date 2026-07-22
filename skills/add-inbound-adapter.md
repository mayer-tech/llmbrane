# Add New Inbound Adapter Skill

Use this skill when adding a new inbound API protocol (e.g., Anthropic, Ollama) to LLMbrane, allowing clients to connect to LLMbrane using different API formats.

## Steps

1. **Create Package**: Create `internal/inbound/{protocol}/`.
2. **Define Models**: Add `models.go` with the protocol's specific request and response JSON structs.
3. **Implement Handler**: Add `handler.go` containing an HTTP handler struct that wraps `*core.Gateway`:
    - `NewHandler(gateway *core.Gateway) *Handler`
    - `RegisterRoutes(mux *http.ServeMux, prefix string)`
    - HTTP handler methods (e.g., `HandleChatCompletions(w http.ResponseWriter, r *http.Request)`)
    - Translation logic to convert incoming requests to canonical format: `translateRequest(...) (*canonical.CanonicalChatRequest, error)` (including canonicalizing reasoning effort if applicable)
    - Translation logic to convert canonical responses to protocol format: `translateResponse(...) *ProtocolRes`
    - Translation logic to convert canonical stream events to protocol format: `translateStreamEvent(...) *ProtocolStreamRes`
4. **Error Handling**: Implement custom logic (e.g., `writeError`) to map `canonical.GatewayError` back to the external protocol's specific error format.
5. **Register in Main**: Update `cmd/llmbrane/main.go` to initialize the new handler and register its routes with a specific prefix on the router (e.g., `/protocol`).
6. **Add Tests**: Create `handler_test.go` to test the request/response translation logic and route handling.
7. **Document Inbound Adapter**: Update the `README.md` to mention the newly supported inbound protocol, its route prefix, and any specific behaviors.

## Reference
- See `internal/inbound/openai/handler.go` for a standard implementation.
