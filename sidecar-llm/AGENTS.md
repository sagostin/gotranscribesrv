# AGENTS.md — quick orientation for future agent sessions

## Build / run / test

```bash
swift build                   # compile everything
swift test                    # 15 unit tests (parser, prompt rendering, registry)
swift run Server              # binds 0.0.0.0:8080 (PORT / LLM_SIDECAR_HOST to override)
PORT=8081 .build/debug/Server # alternate port
```

Live verification smoke tests after a build:

```bash
curl 127.0.0.1:8080/health
curl 127.0.0.1:8080/v1/models
curl 127.0.0.1:8080/models/mistral-7b-int4/status
```

Server log in dev: `/tmp/llm-sidecar.log` (or whatever you redirect to). In
production the launchd agent (`com.gotranscribesrv.llm-sidecar`) writes to
`deploy/macos/logs/llm-sidecar.{out,err}.log`. Note `localhost` may squat on
IPv6 if Docker is running — always use `127.0.0.1`.

## Module map

```
Sources/
  ModelRuntime/      registry, HF downloader, compile cache, ModelManager,
                     ModelRunner, ChatMessage, JSONValue, TopKLanguageModel
  Tooling/           ToolCallParser
  ImageRuntime/      ImageModelManager (Stable Diffusion)
  EmbeddingRuntime/  EmbeddingModelManager (swift-embeddings)
  ExternalRuntime/   CoreMLLLMManager (coreml-llm backend)
  App/               Vapor routes, OpenAI + Anthropic DTOs, SSE, entrypoint
```

## Conventions

- **Chat backend selection** is via `runtime` on the registry entry: `standard`
  (in-tree) or `coreml-llm` (external). `standard` is the only one that supports
  tool calling.
- **`standard` runtime**: a model is loaded as `LanguageModel` (stateful logit
  output) or `TopKLanguageModel` (top-k adapter — vocabulary size from
  `config.json`). `ModelManager.detectInterface` auto-classifies.
- **Mistral tool format** is hand-rolled: the Jinja engine silently drops
  `[AVAILABLE_TOOLS]`. Tool-call ids are normalized to 9 alphanumeric chars
  (`normalizedToolCallID`).
- **Codable defaults**: `ModelRegistryEntry`, `ServerSettings`, and `Features`
  have custom `init(from:)` with `decodeIfPresent` for defaulted fields.
  Synthesized Codable would otherwise require every key.
- **Sendable**: `ModelDownloader` is `@unchecked Sendable` (HubApi is all-let).
  `ChatModel` protocol is `@preconcurrency` (crosses non-Sendable upcalls).
  `ModelContext` is `@unchecked Sendable`.
- **Vendored deps**: `vendor/swift-embeddings` is a fork with `@preconcurrency`
  imports and platform bumped to macOS 15.
- **swift-transformers**: pinned to `from: "1.3.3"` — released mainline, which
  includes the stateful LanguageModel support that lived in the preview branch.
- **Anonymous-style handle**: big route closures are decomposed into free
  functions (`handleAnthropicMessage`, `anthropicStream`, `handleExternalChat`)
  to keep the Swift type-checker happy.

## Common edits

- **Add a new chat model**: drop an entry in `models.json`. If it's a bespoke
  HF repo with no exporters convention, set `runtime: "coreml-llm"`.
- **Add a tool** to the Mistral prompt: edit `ModelRunner.mistralPromptText`
  (string builder) and add tests in `Tests/PromptRenderingTests.swift`.
- **Tweak a route**: `Sources/App/Routes.swift` (OpenAI), `AnthropicRoutes.swift`
  (Anthropic dialect).
- **Adjust gating**: `Settings` block in `models.json` (or env
  `COREML_IMAGES`, `COREML_AUTO_DOWNLOAD`, `COREML_PRELOAD`,
  `COREML_EMBEDDINGS`).
- **Convert a model**: `scripts/convert_chat_model.py` plus `docs/conversion.md`.

## Known small bugs

- **Download progress %** stays at 0% until completion — `HubApi` snapshots
  per-file and `fractionCompleted` reflects files completed, not bytes. Cosmetic.
- **gemma via coreml-llm** supports tool calling via gemma-4's special tokens
  (`<tool>` / `<tool_call>` / `<|tool_response|>`). Tool defs are injected into the
  first user turn and parses `<tool_call>call:NAME{args}<tool_call\>`; tool
  results are synthesized as `<|tool_response>` blocks. OpenAI and Anthropic
  dialects both work; OpenAI is more reliable for round-trips at temp 0.
- **Top-k chat** works best via `/v1/completions` because the bundled Qwen3 is
  a base model and emits EOS first under chat templates.

## When things break

1. Rebuild: `swift build 2>&1 | grep -c error` (should be 0).
2. Tests: `swift test 2>&1 | grep -E "failed|Executed"` (last line should be 14+.
   successes).
3. Model load failure → check `/models/{id}/status` and the server log; the
   error usually says which interface detection step failed (input missing,
   output unknown layout).
4. Embeddings crash with `Unsupported PreTokenizer type: BertPreTokenizer` —
   means the model uses a pre-tokenizer type the current swift-transformers
   version doesn't recognize. Upgrade swift-transformers or pick a different
   model.