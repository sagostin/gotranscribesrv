# sidecar-llm

The LLM inference sidecar for gotranscribeSrv. OpenAI- and Anthropic-compatible
inference server for Core ML LLMs, image generators, and embeddings, written
in Swift + Vapor. Models are declared in a JSON registry, auto-downloaded from
Hugging Face, compiled once, cached, and served over HTTP with SSE streaming.
Tool calling is pass-through — clients (OpenCode, Claude Code, anything
OpenAI-API-shaped) own their MCP/tool config and get `tool_calls` back to
execute.

Lives next to the audio sidecar (`sidecar-audio/`) — together they form the
native Core ML inference layer for gotranscribeSrv. Each sidecar runs
independently on a Mac mini and is managed via launchd (see
`deploy/macos/com.gotranscribesrv.llm-sidecar.plist`).

## Requirements

- macOS 15+ on Apple Silicon (stateful Core ML models; tested on macOS 26)
- Swift 6 / Xcode 16+
- ~5 GB disk per Int4 7B model; ~2 GB per SD 1.5 image model

## Quick start

```bash
# From the repo root, with the new gotranscribesrv Makefile targets:
make llm-build          # release build (faster startup for production)
make llm-sidecar        # debug run on :8080 (PORT env to override)

# Or directly inside this folder:
swift build
swift run Server        # serves on http://127.0.0.1:8080 (PORT env to override)
```

On first run the default chat model (`mistral-7b-int4`, ~4 GB) downloads in the
background, compiles once, and is cached. Every later start loads from the
compiled cache and is ready in ~20 s. Watch progress:

```bash
curl 127.0.0.1:8080/models/mistral-7b-int4/status
```

> If Docker runs on this machine it may squat `localhost` on IPv6 — use
> `127.0.0.1` explicitly or set `PORT`.

> **Production:** install the launchd agent (`make llm-install` from the repo
> root) so the sidecar auto-starts at login and restarts on crash. Logs land
> in `deploy/macos/logs/llm-sidecar.{out,err}.log`.

## What it serves

| Surface | Endpoint(s) | Notes |
|---|---|---|
| OpenAI chat | `POST /v1/chat/completions` | streaming + tools + `usage` + `tool_choice` |
| OpenAI legacy completions | `POST /v1/completions` | raw prompt → text, streaming |
| OpenAI embeddings | `POST /v1/embeddings` | string or array input, L2-normalized |
| OpenAI images | `POST /v1/images/generations` | SD 1.5 / SDXL, 512² / 1024² |
| OpenAI models | `GET /v1/models` | `{object:"list", data:[…]}` |
| Anthropic Messages | `POST /v1/messages` | full Anthropic dialect + streaming SSE |
| Management | `/models/{id}/{download,load,unload,status}`, `/health` | works across all runtimes |

### Tool calling (pass-through)

Send `tools` (OpenAI function schema). If the model wants a tool you get
`finish_reason: "tool_calls"` and `message.tool_calls`. Execute on your side,
append the assistant message (with `tool_calls`) and a `role: "tool"` message
(with `tool_call_id` + `content`), and send the conversation back. Same flow
for Anthropic via `/v1/messages` with `tools` + `tool_result` blocks. The
backend never executes tools — that's the client's job.

### Quick sanity

```bash
# Plain chat
curl 127.0.0.1:8080/v1/chat/completions -H 'Content-Type: application/json' -d '{
  "model":"mistral-7b-int4","max_tokens":40,
  "messages":[{"role":"user","content":"Capital of Japan? One word."}]
}'
# {"choices":[{"message":{"content":"Tokyo."}}],"usage":{"prompt_tokens":14,"completion_tokens":2,...}}

# Tool round-trip
curl .../v1/chat/completions -d '{"model":"mistral-7b-int4","messages":[{"role":"user","content":"What is 7*6?"}], "tools":[{"type":"function","function":{"name":"calculator","parameters":{"type":"object","properties":{"expression":{"type":"string"}}}}}]}'
# finish_reason: tool_calls; execute, then re-send with role:"tool" → final answer "42"

# Anthropic streaming
curl 127.0.0.1:8080/v1/messages -H 'anthropic-version: 2023-06-01' -d '{
  "model":"mistral-7b-int4","max_tokens":30,"stream":true,
  "messages":[{"role":"user","content":"Count to five."}]
}'
# SSE: event: message_start, content_block_delta (text_delta), message_delta (end_turn), message_stop

# Images
curl 127.0.0.1:8080/v1/images/generations -d '{
  "model":"sd-1.5","prompt":"a red apple on wood","steps":15,"seed":42
}'
# {"data":[{"b64_json":"..."}]}

# Embeddings
curl 127.0.0.1:8080/v1/embeddings -d '{
  "model":"all-minilm-l6-v2","input":["hello world"]
}'
# {"data":[{"embedding":[0.014,-0.027,...384 dims]}],"usage":{"prompt_tokens":3,"total_tokens":3}}
```

## Configuration

`models.json` declares everything; `settings` controls server-wide behavior.

```jsonc
{
  "settings": {
    "autoDownload": true,   // false → un-downloaded models 409 on use; manual /download still works
    "preload": true,        // master switch for boot-time preloading
    "features": {
      "images": true,       // false → image module never initializes, route 403s
      "embeddings": true    // false → embeddings route 403s
    }
  },
  "models": [
    {
      "id": "mistral-7b-int4",
      "kind": "chat",
      "repo": "apple/mistral-coreml",
      "runtime": "standard",          // "standard" | "coreml-llm"
      "include": ["StatefulMistral7BInstructInt4.mlpackage/*"],
      "packageName": "StatefulMistral7BInstructInt4.mlpackage",
      "tokenizerRepo": "mistralai/Mistral-7B-Instruct-v0.3",
      "preload": true,
      "maxNewTokens": 512
    }
  ]
}
```

- **Runtimes**: `standard` (stateful Core ML, in-house) and `coreml-llm` (external
  CoreML-LLM backend for bespoke HF repos like `mlboydaisuke/gemma-4-E2B-coreml`).
  Text chat + streaming work via both; tool calling works only via `standard`.
- `.mlpackage` / `.mlmodel` are compiled once to `Models/compiled/<id>.mlmodelc`;
  `.mlmodelc` loads directly (fastest).
- Gated repos (e.g. `google/gemma-*` tokenizers) need `HUGGING_FACE_HUB_TOKEN`.
- `COREML_MAX_RESIDENT` caps simultaneously loaded models (LRU eviction).
- `COREML_IMAGES=0`, `COREML_EMBEDDINGS=0`, `COREML_AUTO_DOWNLOAD=0`,
  `COREML_PRELOAD=0` override the file-based settings.

## Client setup

### OpenCode

```jsonc
{
  "provider": {
    "coreml": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "http://127.0.0.1:8080/v1" },
      "models": { "mistral-7b-int4": {}, "gemma-4-E2B": {} }
    }
  }
}
```

MCP servers stay in OpenCode's own config.

### Claude Code / Anthropic SDK

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
```

Headers like `anthropic-version` and `x-api-key` are accepted and ignored
(local server, no auth).

## Architecture

```
Sources/
  ModelRuntime/      registry, HF downloader, compile cache, ModelManager,
                     ModelRunner (token-level generation), ChatMessage,
                     JSONValue, TopKLanguageModel (top-k adapter)
  Tooling/           ToolCallParser ([TOOL_CALLS] extraction, balanced JSON)
  ImageRuntime/      Stable Diffusion pipeline management + generation
  EmbeddingRuntime/  swift-embeddings-backed embedding model management
  ExternalRuntime/   CoreML-LLM backend for bespoke chat repos (gemma E2B)
  App/               Vapor routes, OpenAI + Anthropic DTOs, SSE, entrypoint
```

Key implementation notes are in `AGENTS.md`.

## Performance & RAM

- **First run** compiles the model (minutes for a 7B). Subsequent runs load
  from `Models/compiled/<id>.mlmodelc` in seconds.
- **Stateful models** keep the KV cache in MLState — multi-turn is fast; full
  history is re-rendered each round (stateless HTTP).
- **LRU eviction** keeps resident models bounded by `COREML_MAX_RESIDENT`
  (default 2). Loaded models stay resident until evicted.
- **Image models** are large; default `preload: false` for both SD 1.5 and SDXL.
- First token latency for Mistral int4: a few seconds; full streaming then
  follows at ~10–30 tokens/sec on Apple Silicon depending on hardware.

## Tests

```bash
swift test    # registry decoding, prompt rendering, tool-call parser
```

## Recommended models and compatibility — see `docs/models.md`

## Convert your own chat models — see `docs/conversion.md`