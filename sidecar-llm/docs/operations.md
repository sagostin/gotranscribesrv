# Operations

Day-to-day operation: the status state machine, HTTP error reference, the
LRU eviction model, performance and RAM characteristics, and a
troubleshooting checklist for the things that bite most often.

For setup/build/install see [setup.md](setup.md). For configuration knobs
see [configuration.md](configuration.md). For request/response shapes see
[formats.md](formats.md).

## Contents

- [Status state machine](#status-state-machine)
- [HTTP error code reference](#http-error-code-reference)
- [LRU eviction & memory model](#lru-eviction--memory-model)
- [Performance & RAM](#performance--ram)
- [Troubleshooting](#troubleshooting)
- [Disk layout & cache management](#disk-layout--cache-management)

---

## Status state machine

Every registry entry's status — reported by `GET /v1/models` and
`GET /models/{id}/status` — is one of these labels.

```
        (boot / start)
              │
              ▼
     ┌─────────────────┐
     │ not_downloaded  │
     └─────────────────┘
              │  download starts
              ▼
     ┌─────────────────┐
     │ downloading(N%) │◀──── (progress callbacks)
     └─────────────────┘
              │  files present
              ▼
     ┌─────────────────┐
     │ downloaded      │
     └─────────────────┘
              │  first request OR /models/:id/load
              ▼
     ┌─────────────────┐
     │ compiling       │   (first run only; skips if Models/compiled/<id>.mlmodelc exists)
     └─────────────────┘
              │  MLModel.compileModel finishes
              ▼
     ┌─────────────────┐
     │ loading         │   (also: tokenizer load + MLModel(contentsOf:) + warmup)
     └─────────────────┘
              │  warmup decode succeeds
              ▼
     ┌─────────────────┐
     │ ready           │◀─────────────────────────────┐
     └─────────────────┘                              │
              │   POST /models/:id/unload             │
              │   OR  LRU eviction                    │
              ▼                                       │
     ┌─────────────────┐                              │
     │ downloaded      │ ─── request again ───────────┘
     └─────────────────┘

  ANY  ── exception caught ──▶  ┌─────────────────┐
                                 │ failed: <msg>   │   (autoDownload off + no files ⇒ conflict)
                                 └─────────────────┘
```

Transitions are recorded in the actor at
`Sources/ModelRuntime/ModelManager.swift:8` (`ModelStatus` enum + helpers).

Notes:

- `downloading(N%)` may stay near `0%` until completion — `HubApi` emits
  per-file progress, not per-byte. Cosmetic.
- `failed` is sticky until the next operation tries again
  (`/models/:id/load` or another `/download`).
- Image embeddings and external-backend models have analogous state inside
  their own managers (`ImageModelManager`, `EmbeddingModelManager`,
  `CoreMLLLMManager`).

---

## HTTP error code reference

The server uses standard HTTP status codes plus an OpenAI-shaped error
envelope (see [formats.md — Error envelope](formats.md#error-envelope)).

| HTTP | `error.type` | When | Fix |
|---|---|---|---|
| `400 invalid_request_error` | `Unknown model: <id>` | `model` not in registry. | Update `models.json`. |
| `400 invalid_request_error` | `Wrong kind` | A `kind: "image"` model was used on `/v1/chat/completions`, or vice versa. | Hit the right endpoint for the entry's `kind`. |
| `400 invalid_request_error` | `Incompatible layout` | The compiled Core ML model didn't have the expected `inputIds` / `logits` I/O. | See logs — usually signals a corrupt `.mlpackage`. Re-download. |
| `400 invalid_request_error` | `Package not found` | `include` globs matched no `.mlpackage` in the repo. | Adjust globs in `models.json`. |
| `409 invalid_request_error` | `Auto download disabled` | `settings.autoDownload = false` and the model isn't downloaded yet. | `POST /models/{id}/download` first, or set `autoDownload = true`. |
| `403 feature_disabled` | `Image generation is disabled…` | `settings.features.images = false`. | `COREML_IMAGES=1` or set `features.images` to `true`. |
| `403 feature_disabled` | `Embeddings are disabled…` | `settings.features.embeddings = false`. | `COREML_EMBEDDINGS=1` or set `features.embeddings` to `true`. |
| `404 invalid_request_error` | `Unknown model: <id>` | `POST /models/:id/...` with an id that's not in the registry. | Update `models.json`. |
| `404 invalid_request_error` | `External runtime not available…` | A `runtime: "coreml-llm"` model is requested but the external manager is `nil`. | Register a chat entry with `runtime: "coreml-llm"` or use a `standard` entry. |
| `500 server_error` | `<message>` | Generation threw unexpectedly. | Check server log — usually model-side OOM or ANE fallback failure. |
| `500` (streaming) | `data: {"error":"..."}` | Generation threw mid-stream. | Catch-up error frame, then `data: [DONE]`. |

---

## LRU eviction & memory model

`ModelManager` keeps a small in-memory map: `[id: ModelRunner]`. When a new
model is loaded and the map exceeds `COREML_MAX_RESIDENT`, the LRU runner
is dropped (its resident ANE state, KV cache, and any of its in-progress
state are released). The next request for an evicted model re-loads +
re-warms — paying compile-cache-load cost, not first-run cost.

| Knob | Default | Effect |
|---|---|---|
| `COREML_MAX_RESIDENT` | `2` | Maximum simultaneously-resident chat runners (LRU). Bumping to `3`+ lets you keep multiple models hot at the cost of RAM. |

`POST /models/{id}/unload` forces eviction of one specific entry.

Image models (`ImageModelManager`) keep a separate resident set with the same
LRU rules.

---

## Performance & RAM

### Cold-start cost

- **First request ever** for a model: download (network-bound, depends on
  HF) → `MLModel.compileModel(at:)` (minutes for a 7B on first compile) →
  one-shot load + warmup decode.
- **Subsequent requests**: load from `Models/compiled/<id>.mlmodelc` —
  seconds, not minutes.
- After the first compile, server restarts are 20-ish seconds to `ready` for
  the default chat model.

### Per-request latency (rough, Apple Silicon)

- **First-token latency** for Mistral 7B Int4: a few seconds (KV cache
  initialize).
- **Steady-state streaming throughput**: ~10–30 tokens/sec depending on
  hardware and context length.
- Top-k models (`qwen3-1.7b-w8`) are proportionally faster and smaller.
- Tool-enabled requests buffer the full output before parsing → expect
  slightly higher time-to-first-frame than plain chat.

### Memory & disk

Approximate on-disk sizes:

| Model | Size |
|---|---|
| Int4 chat (Mistral 7B) | ~0.55 B/param → **~4 GB** |
| FP16 chat (Mistral 7B) | ~2.05 B/param → **~14 GB** |
| Top-k chat (Qwen3 1.7B) | ~2 GB |
| SD 1.5 (palettized) | ~2 GB |
| SDXL (compiled) | ~5 GB+ |
| MiniLM-L6-v2 (embedding) | ~90 MB |

KV cache overhead at the model's full context is roughly 150 MB per layer.
The cache lives in MLState and grows with the actual prompt size.

### Multi-model coexistence

- Set `preload: true` only on the model(s) you want warm at boot. Image
  models in particular are large and rarely need preloading.
- `COREML_MAX_RESIDENT=2` keeps two chat models hot at once. Bumping it
  past RAM headroom triggers Core ML CPU fallback and large slowdowns.
- Stateful models keep the per-conversation KV cache in MLState across
  calls in a single session — multi-turn is fast. The server itself is
  stateless at the HTTP layer: each `/v1/chat/completions` re-renders the
  full message history, so long conversations pay the prefill cost each
  turn.

---

## Troubleshooting

### `Unsupported PreTokenizer type: BertPreTokenizer`

`swift-embeddings` doesn't recognize the embedding model's pre-tokenizer.
**Fix:** upgrade `swift-transformers` (in `Package.swift`) or pick a model
whose `tokenizer_config.json` uses a supported pre-tokenizer
(`WhitespacePreTokenizer`, `BertPreTokenizer` for legacy BERT, etc.).

### Model load fails with interface-detect error

`/models/{id}/status` will read `failed: ...`. The log line will name the
step:

- `input missing` — the `.mlpackage` didn't expose `inputIds`.
- `unrecognized outputs: <names>` — output shape isn't `.logits` (single
  float tensor) and isn't `.topK` (one int + one float).

See `Sources/ModelRuntime/ModelManager.swift:252` (`detectInterface`). For
bespoke layouts not in this list, drop the `runtime` to `"coreml-llm"` and
register a matching external entry, or convert with
`scripts/convert_chat_model.py` to the exporters convention.

### `409 invalid_request_error` on first use

`autoDownload` is off. Run:

```bash
curl -X POST 127.0.0.1:8080/models/<id>/download
```

…and poll `/models/<id>/status`. Or set `COREML_AUTO_DOWNLOAD=1`.

### Docker squats localhost

Use `127.0.0.1` instead of `localhost`. Or set `PORT` and pass it to the
client too.

### Gated repos 401/403 from Hub

`google/gemma-*` and similar gated repos require a token:

```bash
export HUGGING_FACE_HUB_TOKEN=hf_...
swift run App
```

### Tool calls don't parse

The model isn't emitting a `[TOOL_CALLS] [...]` block (Mistral format) or
`<tool_call>call:...<tool_call|>` (gemma format). Common causes:

- Wrong base model: a base model (not chat-tuned) won't follow the tool
  prompt reliably. Use a chat-tuned variant.
- Wrong registry entry: confirm `runtime: "standard"` for Mistral;
  `runtime: "coreml-llm"` for gemma.
- Hit `models.md — Recommending models per task` for the best supported
  tool-using entries in this server.

### Model emits EOS on first chat turn

Likely a base (not instruct) model — e.g. `qwen3-1.7b-w8`. Use
`/v1/completions` with a hand-crafted prompt, or switch to an
instruct-tuned model.

### Compile cache takes minutes every restart

`Models/compiled/` was wiped (or the model's id was changed). Recompile is
unavoidable. To preserve across machines, archive
`Models/compiled/<id>.mlmodelc/`.

### Streaming stalls / hangs

`swift run Server` is bound to stdout buffering; pipe through `tee` or
redirect to a file (e.g. `> /tmp/llm-sidecar.log 2>&1`) to keep log output
flowing. In production the launchd agent handles this for you — logs land in
`deploy/macos/logs/llm-sidecar.{out,err}.log`. Also note that requests with
`tools` always buffer the full output before responding — this is by design.

---

## Disk layout & cache management

```
Models/
├── hf/                       # raw HF downloads (mirror of the Hub layout)
│   └── apple__mistral-coreml/
│       └── StatefulMistral7BInstructInt4.mlpackage/
└── compiled/                 # Models/compiled/<id>.mlmodelc — cached CoreML compilations
```

Ops commands:

```bash
# Force a single model to recompile on next request
rm -rf Models/compiled/<id>.mlmodelc

# Wipe a downloaded model (forces re-download on next use)
rm -rf Models/hf/<vendor>__<repo>

# Clear all downloads
rm -rf Models/hf Models/compiled
```

These are untracked; safe to delete. The compile cache is the expensive
thing to lose (minutes to rebuild); the HF cache re-pulls quickly.

---

## See also

- [models.md](models.md) — what works on day 1, what doesn't.
- [conversion.md](conversion.md) — if you're converting a new model.
- [architecture.md](architecture.md) — module-by-module runtime map.
