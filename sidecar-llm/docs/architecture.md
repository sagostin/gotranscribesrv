# Architecture

How the pieces fit together. For the day-to-day operations view (status
state machine, eviction, error codes, troubleshooting) see
[operations.md](operations.md). For the per-field JSON shapes see
[formats.md](formats.md). For module/idiom conventions see
[AGENTS.md](../AGENTS.md).

## Module map

```text
Sources/
├── ModelRuntime/        registry, HF downloader, compile cache, ModelManager,
│                        ModelRunner (token-level generation), ChatMessage,
│                        JSONValue, TopKLanguageModel
├── Tooling/             ToolCallParser ([TOOL_CALLS] extraction, balanced JSON)
├── ImageRuntime/        Stable Diffusion pipeline management + generation
├── EmbeddingRuntime/    swift-embeddings-backed embedding manager
├── ExternalRuntime/     CoreML-LLM backend (gemma) + gemma-4 tool support
└── App/                 Vapor routes, OpenAI + Anthropic DTOs, SSE, entrypoint
```

### One-line responsibilities

| Module | Responsibility |
|---|---|
| `ModelRuntime/ModelRegistry` | Pure-data parsing of `models.json`. Env-override layer (`ServerSettings.applyingEnvironment`). |
| `ModelRuntime/ModelDownloader` | `HubApi` wrapper that pulls the `include` globs from the registry repo (or `local:` prefix) and pulls tokenizer files. |
| `ModelRuntime/ModelManager` | Actor owning: per-entry `ModelStatus`, in-memory `ModelRunner`s, LRU eviction, compile cache (`Models/compiled/`). |
| `ModelRuntime/ModelRunner` | Stateful token-level generation loop. Render prompt → `LanguageModel.generate(...)` → decode → emit deltas. See `Sources/ModelRuntime/ModelRunner.swift:1`. |
| `ModelRuntime/TopKLanguageModel` | Adapter for top-k-exported variants (qwen3-1.7b-w8). Scatters top-k logits back to vocab for sampling. |
| `Tooling/ToolCallParser` | Parses Mistral `[TOOL_CALLS] [...]` from generated text. Tolerant of missing marker. |
| `ImageRuntime/ImageModelManager` | Compiles / caches / drives Stable Diffusion pipelines (palettized SD 1.5, SDXL). |
| `EmbeddingRuntime/EmbeddingModelManager` | Wraps `swift-embeddings`. One manager per registered embedding entry. |
| `ExternalRuntime/CoreMLLLMManager` | Wraps the `john-rocky/CoreML-LLM` external backend for bespoke HF CoreML repos. |
| `ExternalRuntime/Gemma4ToolsSupport` | Parses `<tool_call>...<tool_call|>` blocks; promotes them to OpenAI-shape tool calls / Anthropic-shape `tool_use` blocks. |
| `App/Entrypoint` | Boots the registry, sets up managers, wires Vapor, kicks off background preloads. |
| `App/Routes` | HTTP routing: `/health`, `/v1/models`, `/{download,load,unload,status}`, `/v1/{chat/completions,completions,embeddings,images/generations}`. |
| `App/AnthropicRoutes` | `/v1/messages` and the Anthropic SSE event pipeline. |
| `App/DTOs` | Request/response types and `SSEChunk` frame builders. |

---

## Request data flow

### Non-streaming chat (`POST /v1/chat/completions`, `stream: false`)

```text
Routes.POST /v1/chat/completions                          (Routes.swift:201)
  ├── decode ChatCompletionRequest                        (DTOs.swift:56)
  ├── lookup entry in registry (404 if unknown)
  ├── branch on entry.runtime
  │     ├── .standard  → acquire ModelRunner from ModelManager (download + compile + load on first use)
  │     └── .coremlLLM → handleExternalChat                (Routes.swift:450)
  ├── runChatRound(runner, messages, toolsJSON, …)         (Routes.swift:31)
  │     ├── runner.applyChat(messages, toolsJSON)          (tokenize + render chat template)
  │     └── runner.generate(tokens, maxNewTokens, …)       (stateful Core ML KV-cache loop)
  ├── decode generated tokens → text
  ├── if toolsJSON: ToolCallParser.parse(text)             (Tooling/ToolCallParser.swift:15)
  └── emit JSON response (OpenAI shape)
```

### Streaming chat (`stream: true`)

The same flow as above, but `generate(...)` calls the supplied `onText` for
each token delta:

```text
streamingResponse { emit in                           (Routes.swift:564)
    runChatRound(..., onText: { delta in emit(SSEChunk.delta(delta)) })
    emit(SSEChunk.finish(reason, usage: ...))
    emit(SSEChunk.done)
}
```

With tools, the model output is fully buffered (so it can be parsed) and the
final answer (or `tool_calls`) is emitted as the terminal frames. Without
tools, deltas stream live.

### Anthropic (`POST /v1/messages`)

The same runner + parsing path runs, but the result is mapped to
`{type:"text"}` / `{type:"tool_use"}` blocks (non-stream) or to the
`message_start → content_block_start → content_block_delta → content_block_stop → message_delta → message_stop`
event sequence (stream). See `anthropicStream` and `handleAnthropicMessage`
in `Sources/App/AnthropicRoutes.swift`.

---

## Compile cache flow

`ModelManager.runner(for: id)` is the only entry point that loads a chat
runner. The flow (see `ModelManager.swift:98`) is:

```text
runner(for: id)
  ├── if cached, return the resident runner (touch LRU timestamp)
  ├── entry.kind == .chat else → ModelError.wrongKind
  ├── entry.runtime != .coremlLLM else → ModelError.unsupportedRuntime
  ├── downloadFiles(entry, implicit: true)            (respects autoDownload)
  ├── statuses[id] = .compiling
  ├── findPackage(localRepoURL(entry.repo), entry)    (locate the .mlpackage glob)
  ├── compiledModelURL(for: packageURL, id: id)       (file cache below)
  ├── statuses[id] = .loading
  ├── detectInterface(modelURL, id)                    (.logits | .topK)
  ├── downloadTokenizerFiles + tokenizer (AutoTokenizer.from)
  ├── build ChatModel
  │     ├── .logits → LanguageModel.loadCompiled(url:, computeUnits: .all)
  │     └── .topK   → TopKLanguageModel(MLModel(...), vocabSize: from config.json)
  ├── construct ModelRunner and cache it
  ├── evictIfNeeded(except: id)                       (LRU eviction)
  ├── runner.warmup()                                  (one-token decode to ensure ANE is hot)
  ├── statuses[id] = .ready
  └── return runner
```

`compiledModelURL(for:id:)` (`ModelManager.swift:226`):

```text
compiledModelURL(packageURL, id)
  ├── if extension == "mlmodelc" → return as-is (no compile step)
  ├── destination = Models/compiled/<id>.mlmodelc
  ├── if exists → return cached
  └── Task.detached { try MLModel.compileModel(at: packageURL) }
       └── move result to destination
```

The first request for a model after a fresh `Models/compiled/` removal will
re-compile and cache again. Subsequent server starts load from the cache in
seconds. Image models (`ImageModelManager`) and the external coreml-llm
backend have analogous caches managed inside their own modules.

---

## Runtime dispatch

| `runtime` | Backend | Where in code | Tool calling | Streaming |
|---|---|---|---|---|
| `standard` (default) | `swift-transformers` `LanguageModel` (stateful logits) and `TopKLanguageModel` (stateful top-k) | `Sources/ModelRuntime/ModelManager.swift:131` | Yes — Mistral `[TOOL_CALLS]` parser | Yes |
| `coreml-llm` | External `john-rocky/CoreML-LLM` package | `Sources/ExternalRuntime/CoreMLLLMManager.swift` | Yes — gemma-4 `<tool>` / `<tool_call>` (see `Sources/ExternalRuntime/Gemma4ToolsSupport.swift`) | Yes |
| image | `ml-stable-diffusion` | `Sources/ImageRuntime/ImageModelManager.swift` | n/a | n/a |
| embedding | `swift-embeddings` | `Sources/EmbeddingRuntime/EmbeddingModelManager.swift` | n/a | n/a |

`runtime` is only meaningful for `kind: "chat"`. The other kinds ignore it.

---

## LRU eviction

`ModelManager` keeps at most `COREML_MAX_RESIDENT` (default `2`) runners in
memory. On each new `runner(for:)` it touches `lastUsed[id]`; if the count
exceeds the cap it unloads the runner with the oldest `lastUsed`, freeing
the LRU slot. See `evictIfNeeded(except:)` in
`ModelManager.swift:281`. ImageModelManager has the same pattern.

Forcing eviction: `POST /models/{id}/unload`.

---

## Streaming pipeline detail

`streamingResponse` (in `Sources/App/Routes.swift:564`) returns a Vapor
`Response` whose body is a stream writer. The body writer spawns a `Task`
that runs the handler with an `emit` closure; each `emit` is a single
SSE-formatted string written synchronously to the connection:

```
data: {... JSON frame ...}\n\n
```

The terminal frame is `data: [DONE]\n\n` (OpenAI) or
`event: message_stop\ndata: {"type":"message_stop"}\n\n` (Anthropic).

---

## Concurrency notes

- `ModelManager`, `ImageModelManager`, `EmbeddingModelManager`,
  `CoreMLLLMManager` are actors.
- `ModelDownloader` is `@unchecked Sendable` (HubApi is all-let).
- `ChatModel` protocol is `@preconcurrency` (crosses non-Sendable upcalls).
- `ModelContext` is `@unchecked Sendable`.
- `routes(...)` decomposes route handlers into free functions
  (`handleAnthropicMessage`, `anthropicStream`, `handleExternalChat`) to
  keep the Swift type-checker happy on big route closures.

These are described further in [AGENTS.md](../AGENTS.md).
