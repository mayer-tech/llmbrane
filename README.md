# llmbrane

`llmbrane` is a lightweight gateway for routing LLM requests across multiple providers through a single, configurable interface.

Today, `llmbrane` exposes OpenAI-compatible and Anthropic-compatible inbound APIs. Clients can send normal OpenAI-style or Anthropic-style requests to the gateway, while `llmbrane` resolves model aliases, selects the configured backend provider, translates the request into that provider's API shape, and translates the response back for the client.

The architecture is designed to support multiple inbound APIs over time. That means clients can speak different familiar API formats, such as OpenAI, Anthropic, or other protocol shapes, while still reaching any configured outbound provider behind the gateway.

Use it when you want to:

- switch models or providers without changing client code
- expose one stable API to tools that only understand OpenAI-style requests
- route requests to cloud, local, or self-hosted models from configuration
- keep provider credentials and model mappings centralized
- add new providers or inbound API formats behind a consistent gateway architecture

## Getting Started

// TODO: fix the getting started part

### 1. Create Your Local Configuration

Start from the checked-in examples and keep your real credentials in local, untracked files:

```bash
cp config.example.yaml config.yaml
cp .env.example .env
```

Open `.env` and add the API keys for the providers you want to use. Keep unused placeholders or remove the matching provider blocks from `config.yaml`, because config loading validates every `${...}` reference.

Open `config.yaml` if you want to change the listening address, aliases, provider endpoints, or model mappings. The example file is a runnable starting point that already includes OpenAI, Anthropic, Gemini via OpenAI compatibility, and AWS Bedrock examples.

### 2. Start the Server

The easiest way to run `llmbrane` from a cloned repository is Docker Compose.

```dockercompose
services:
  llmbrane:
    image: ghcr.io/mayer-tech/llmbrane:latest
    ports:
      - "8080:8080"
    volumes:
      - ./config.yaml:/config/config.yaml:ro
    env_file:
      - .env
    restart: unless-stopped
```

### 3. Example Requests

Since `llmbrane` provides an OpenAI-compatible and Anthropic-compatible inbound handler, you can interact with it using standard API structures. Use either model aliases from `config.yaml` or explicit names in the `provider/model` format.

**Requesting via OpenAI API shape (alias `gpt-4o`):**
```bash
curl http://localhost:8080/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello, how are you?"}]
  }'
```

**Requesting via Anthropic API shape (explicit format `anthropic/claude-haiku-4-5`):**
```bash
curl http://localhost:8080/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anthropic/claude-haiku-4-5",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello, Claude!"}]
  }'
```

## Configuration

`config.yaml` defines how the gateway listens, how incoming model names are resolved, and which provider APIs are called.

- `server.listen` controls the bind address. The example uses `:8080`.
- `server.request_timeout` sets the request timeout value stored in configuration.
- `aliases` maps convenient client-facing model names to explicit `provider/model` targets. For example, `gpt-4o: openai/gpt-4o` lets clients send `"model": "gpt-4o"`.
- `providers` contains named backend providers. The provider name becomes the first part of an explicit model reference, such as `anthropic/claude-haiku-4-5`.
- `providers.<name>.endpoints[].api` selects the outbound adapter/API shape.
- `providers.<name>.endpoints[].url` is the provider endpoint. Bedrock URLs can include `{target_model}`, which is replaced with the configured target model.
- `providers.<name>.endpoints[].headers` configures provider-specific HTTP headers. Values like `${OPENAI_API_KEY}` are read from the process environment.
- `providers.<name>.endpoints[].models` maps llmbrane model names to provider-native `target` model IDs.
- `providers.<name>.endpoints[].extensions` enables adapter-specific behavior.

Local secrets are intentionally kept out of the repository and Docker build context. The examples contain placeholders only; real credentials belong in your untracked `.env` file or in the process environment.

### Available Outbound Adapters

`llmbrane` currently supports the following outbound API shapes, configured via the `api` field in an endpoint:

- **`openai-chat-completions`**: Standard OpenAI API (`/v1/chat/completions`). Used for OpenAI and OpenAI-compatible endpoints like local servers or Google Gemini's OpenAI compatibility mode.
  - **Configuration**: Requires standard OpenAI headers, such as `Authorization: Bearer ...`.
  - **Extensions**: `inject_thought_signature_bypass` (boolean) injects a `skip_thought_signature_validator` bypass for tool calls. This is useful when targeting Google Gemini via its OpenAI API compatibility because some clients drop the required `extra_content` signature field.
- **`anthropic-messages`**: Anthropic's Messages API.
  - **Configuration**: Requires Anthropic-specific headers, such as `x-api-key`.
- **`bedrock-converse`**: AWS Bedrock Converse API.
  - **Configuration**: Requires AWS credentials through `extensions` for direct Bedrock calls, or normal HTTP headers when targeting a Bedrock-compatible proxy.

## For Developers

The codebase is organized into several distinct areas to cleanly separate concerns between protocol parsing, gateway routing, and provider-specific mapping.

### Architecture Overview

- **`cmd/llmbrane`**: The application entrypoint. It loads the config, initializes the `Gateway`, registers outbound adapters, attaches inbound handlers to the HTTP router, and starts the server.
- **`internal/canonical`**: Defines the internal, unified data models for requests and responses (e.g., `CanonicalChatRequest`). All incoming requests are converted to these models before being passed to the outbound adapters.
- **`internal/config`**: Configuration parsing. Holds structures like `Provider`, `Endpoint`, and `Model`.
- **`internal/core`**: The heart of `llmbrane`. The `Gateway` resolves models, applies aliases, and matches requests to the correct registered `OutboundAdapter`.
- **`internal/inbound`**: Handlers that receive incoming HTTP requests. For example, `inbound/openai` parses OpenAI-formatted JSON bodies, translates them into the `canonical` format, and invokes the `Gateway`.
- **`internal/outbound`**: Adapters that take `canonical` models and translate them into provider-specific API calls. E.g., `outbound/anthropic` converts `CanonicalChatRequest` into an Anthropic Messages API payload.

### How Routing Works

1. **Inbound Request**: An HTTP request arrives at an inbound handler (e.g., `POST /openai/v1/chat/completions`). The handler parses the JSON and looks at the requested `model` string (e.g., `"gpt-4o"`).
2. **Gateway Resolution**: The handler passes the canonical request to `Gateway`. The Gateway looks up the model:
   - It checks for an **alias** (e.g., `"gpt-4o" -> "openai/gpt-4o"`).
   - It splits the target into **Provider** (`"openai"`) and **Model** (`"gpt-4o"`).
   - It checks the configuration to find the matching provider and endpoint for this model.
3. **Outbound Dispatching**: The config defines an `api` type (API shape) for the endpoint (e.g., `api: "anthropic-messages"`). The `Gateway` looks up the `OutboundAdapter` registered for this API shape.
4. **Translation & Execution**: The `OutboundAdapter` translates the canonical request to the provider's specific API format, sends the HTTP request, translates the response back to canonical format, and returns it to the inbound handler.

### Developing Adapters

#### Creating a New Outbound Adapter

To add a new provider (e.g., Ollama):
1. Create a package in `internal/outbound/ollama`.
2. Implement the `core.OutboundAdapter` interface (`ProcessChat`, `ProcessChatStream`).
3. These methods receive a `canonical.CanonicalChatRequest`. Map this to the provider-specific payload and make the network call.
4. Convert the provider's response into a `canonical.CanonicalChatResponse`.
5. In `cmd/llmbrane/main.go`, register the adapter with the Gateway:
   ```go
   gateway.RegisterOutboundAdapter("ollama", outbound_ollama.NewAdapter())
   ```
6. Users can now define endpoints with `api: "ollama"` in their `config.yaml`.

#### Adding an Inbound Handler

To add a new API interface (e.g., an Anthropic-compatible API for incoming requests):
1. Create a package in `internal/inbound/anthropic`.
2. Implement an HTTP handler that decodes Anthropic requests.
3. Map the decoded request to `canonical.CanonicalChatRequest`.
4. Call the Gateway to process it.
5. Map the returned `canonical.CanonicalChatResponse` back to the expected Anthropic JSON structure and write the HTTP response.
6. Register the handler to the `router` in `cmd/llmbrane/main.go`.
