# Setup & installation

## Requirements

- **macOS 15+** on Apple Silicon (tested on macOS 26). Stateful Core ML needs
  Apple Silicon; x86 macs are not supported.
- **Swift 6 / Xcode 16+** toolchain.
- **Disk**: ~5 GB per Int4 7B chat model; ~2 GB per SD 1.5; ~5 GB+ for SDXL;
  ~90 MB per MiniLM embedding.
- **RAM**: enough headroom for the resident models you load — see
  [operations.md](operations.md#performance--ram).

## Build

```bash
swift build
```

Build output goes to `.build/`. No Xcode project file is needed; the
`Package.swift` is the source of truth.

## Run

```bash
swift run Server             # serves on http://127.0.0.1:8080
PORT=8081 swift run Server   # alternate port
```

The server reads `models.json` from the current working directory on every
start, then listens for HTTP. Background preloads kick off once the port is
bound, so the server accepts connections immediately even while a large model
is still compiling.

For production, install the launchd agent (`make llm-install` from the
gotranscribesrv repo root). The agent runs the release binary and writes logs
to `deploy/macos/logs/llm-sidecar.{out,err}.log`.

For a debug/dev session without launchd:

```bash
swift run Server 2>&1 | tee /tmp/llm-sidecar.log
```

> **IPv6 caveat.** If Docker is running, it may squat `localhost` on IPv6 and
> the server's HTTP socket ends up on IPv4-only. Always use `127.0.0.1`
> explicitly, not `localhost`.

## Environment variables

| Var | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | TCP port for the HTTP listener. |
| `HUGGING_FACE_HUB_TOKEN` | — | Bearer token for gated repos (e.g. `google/gemma-*`). Passed to `HubApi`. |
| `COREML_MAX_RESIDENT` | `2` | LRU cap for simultaneously loaded chat models (`standard` runtime). Used models stay resident until evicted. |
| `COREML_AUTO_DOWNLOAD` | from `models.json` (`true`) | Disable with `0`/`false`/`no`/`off` to make un-downloaded models fail requests with 409. |
| `COREML_PRELOAD` | from `models.json` (`true`) | Master switch for boot-time preloading of chat entries marked `preload: true`. |
| `COREML_IMAGES` | from `models.json` (`true`) | Disable to skip image-model initialization — `/v1/images/generations` then 403s. |
| `COREML_EMBEDDINGS` | from `models.json` (`true`) | Disable to skip embedding-model initialization — `/v1/embeddings` then 403s. |

See [configuration.md](configuration.md#env-overrides) for how env values
override `models.json`, and `applyingEnvironment` in
[Sources/ModelRuntime/ModelRegistry.swift](../Sources/ModelRuntime/ModelRegistry.swift)
for the exact rule (`"0"`/`"false"`/`"no"`/`"off"` disable, anything else
enables).

## On-disk layout

After the first run:

```
.
├── models.json                   # registry (you edit this)
├── Sources/...                   # server source
├── Models/
│   ├── hf/                       # raw HF downloads, one directory per repo
│   │   └── apple__mistral-coreml/
│   │       └── StatefulMistral7BInstructInt4.mlpackage/
│   └── compiled/                 # compiled (.mlmodelc) caches, one per model id
│       └── mistral-7b-int4.mlmodelc/
├── docs/                         # this folder
├── scripts/convert_chat_model.py # for new chat models
└── .build/                       # SwiftPM output
```

- `Models/hf/<vendor>__<repo>/...` mirrors the Hub layout. The downloader
  matches the `include` globs from `models.json` to find the `.mlpackage`
  within.
- `Models/compiled/<id>.mlmodelc/` is the warmed-up Core ML output. Each chat
  model's `.mlpackage` is compiled once and cached; subsequent server starts
  load from here in seconds.
- `Models/compiled/` is safe to delete to force a recompile.

## First-run smoke tests

```bash
curl 127.0.0.1:8080/health
# {"status":"ok"}

curl 127.0.0.1:8080/v1/models
# {"object":"list","data":[{ "id":"mistral-7b-int4", ..., "status":"ready" }, ...]}

curl 127.0.0.1:8080/models/mistral-7b-int4/status
# {"id":"mistral-7b-int4","status":"downloading(37%)"}
# (transitions through downloading → downloaded → compiling → loading → ready)
```

A cold first run downloads the default chat model (`mistral-7b-int4`, ~4 GB),
compiles it, loads it, and warms up — expect minutes. Every later start
loads the compiled cache and reaches `ready` in roughly 20 s.

To watch progress without polling:

```bash
# Production (launchd-managed):
tail -f deploy/macos/logs/llm-sidecar.out.log
# look for "[ModelManager] compiling mistral-7b-int4 ..." and
#                "[ModelManager] mistral-7b-int4 ready"

# Dev (backgrounded swift run):
tail -f /tmp/llm-sidecar.log
```

## Tests

```bash
swift test
# 15 tests covering registry decoding, prompt rendering, and tool-call parsing
```

## Pointing an OpenAI-compatible client at the server

The HTTP surface is OpenAI-shaped, so any OpenAI / OpenAI-compatible SDK works
by overriding `baseURL`. Two ready-made configurations:

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

MCP servers stay in OpenCode's own config — tool execution is the client's
job.

### Claude Code / Anthropic SDK

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8080
```

Headers like `anthropic-version` and `x-api-key` are accepted and ignored
(local server, no auth). See [endpoints.md](endpoints.md#anthropic) and
[formats.md](formats.md) for the `/v1/messages` request shape and the
Anthropic SSE event sequence.

### OpenAI Python SDK

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="not-used")
resp = client.chat.completions.create(
    model="mistral-7b-int4",
    messages=[{"role": "user", "content": "Capital of Japan? One word."}],
    max_tokens=20,
)
print(resp.choices[0].message.content)
```

## Next steps

- Edit [configuration.md](configuration.md) to register / tune models.
- See [endpoints.md](endpoints.md) for every route.
- Hit [operations.md](operations.md) when something doesn't behave.
