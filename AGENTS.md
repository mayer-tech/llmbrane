# LLMbrane Agent Instructions

LLMbrane is an LLM gateway and multiplexer written in Go. It provides a unified interface to multiple LLM providers.

## Architecture & Component Boundaries

- **`internal/canonical`**: The "source of truth". Contains unified request/response models and standardized error kinds (`ErrorKind`).
- **`internal/core`**: The central logic (`Gateway`). Handles configuration, model/alias resolution, and request dispatching.
- **`internal/inbound`**: Protocol-specific handlers (e.g., `inbound/openai`). They translate external API requests into `canonical` models.
- **`internal/outbound`**: Provider-specific adapters (e.g., `outbound/anthropic`). They translate `canonical` models into provider-specific API calls.
- **`internal/config`**: YAML configuration handling.

## Essential Conventions

- **Clean Translation**: Always translate inbound requests to `canonical.CanonicalChatRequest` before passing them to the gateway.
- **Error Mapping**: Map all provider-specific errors to `canonical.GatewayError` using appropriate `ErrorKind`.
- **Dependency Injection**: Use explicit constructor-based injection (`NewXxx(...)`).
- **Streaming**: Implemented via Go channels (`<-chan *canonical.CanonicalChatStreamEvent`). Use `http.Flusher` for SSE.
- **Links**:
    - [Architecture Overview](README.md)
    - [Project Vision](VISION.md) (German)
    - [Feature Roadmap & Design Docs](~issues/)

## Build & Test

- **Run Server**: `go run cmd/llmbrane/main.go -config config.yaml`
- **Test All**: `go test ./...`
- **Single Package Test**: `go test ./internal/core/...`
