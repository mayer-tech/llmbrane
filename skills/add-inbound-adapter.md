# Add New Inbound Adapter Skill

Use this skill when adding a new inbound API protocol (e.g., Anthropic, Ollama) to LLMbrane, allowing clients to connect to LLMbrane using different API formats.

## Steps

1. **Create Package**: Create `internal/inbound/{protocol}/`.
2. **Define Models**: Add `models.go` with the protocol's specific request and response JSON structs.
3. **Implement Handler**: Add `handler.go` containing an HTTP handler struct that wraps `*core.Gateway`:
    - `NewHandler(gateway *core.Gateway) *Handler`
    - `RegisterRoutes(mux *http.ServeMux, prefix string)` — registers the protocol routes with method patterns (e.g. `mux.HandleFunc("POST "+prefix+"/v1/chat/completions", ...)`) plus a `GET {prefix}/v1/models` listing endpoint (`HandleListModels`).
    - HTTP handler methods (e.g., `HandleChatCompletions(w http.ResponseWriter, r *http.Request)`); they parse the protocol request, call `gateway.ProcessChat` or `gateway.ProcessChatStream`, and write the translated result.
    - Translation logic to convert incoming requests to canonical format: `translateRequest(...) (*canonical.CanonicalChatRequest, error)` (including canonicalizing reasoning effort if applicable)
    - Translation logic to convert canonical responses to protocol format: `translateResponse(...) *ProtocolRes`
    - Translation logic to convert canonical stream events to protocol format: `translateStreamEvent(...)` — may map one canonical event to multiple protocol events (the Anthropic handler returns `[]*StreamEvent`). Stream responses are written as SSE using `http.Flusher`.
4. **Error Handling**: Implement `writeError` for plain protocol errors and `writeGatewayError` to map `canonical.GatewayError` back to the external protocol's error format. Preserve provider rate-limit semantics: on HTTP 429, forward the normalized `Retry-After` (see `GatewayError.RetryAfter`).
5. **Register in Main**: Update `cmd/llmbrane/main.go` to initialize the new handler and register its routes with a specific prefix on the router (e.g., `/protocol`).
6. **Add Tests**: Create `handler_test.go` to test the request/response translation logic and route handling.
7. **Document Inbound Adapter**: Update the `README.md` to mention the newly supported inbound protocol, its route prefix, and any specific behaviors.

## Reference
- See `internal/inbound/openai/handler.go` for a standard implementation.
- See `internal/inbound/anthropic/handler.go` for a protocol with different message shapes and multi-event stream mapping.
