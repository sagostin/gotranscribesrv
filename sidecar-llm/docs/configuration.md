# Configuration (`models.json`)

The server reads `models.json` from the working directory on every start.
The file has two top-level keys: `settings` (server-wide switches) and
`models` (the registry of available models).

This document covers the schema exhaustively. For the high-level
"how do I register a model?" workflow, jump to
[conversion.md — §5 Registering](conversion.md#5-registering).

## Top-level shape

```jsonc
{
  "settings": { ... },
  "models":   [ ... ]
}
```

A bad `models.json` makes the server print a warning and start with an empty
registry:

```
[server] WARNING: could not load models.json (...); starting with an empty registry
```

Fix the file and restart.

---

## `settings` block

```jsonc
{
  "settings": {
    "autoDownload": true,
    "preload":      true,
    "features": {
      "images":     true,
      "embeddings": true
    }
  }
}
```

| Field | Type | Default | Purpose |
|---|---|---|---|
| `autoDownload` | bool | `true` | If `false`, un-downloaded models 409 on use. Explicit `POST /models/:id/download` still works. |
| `preload` | bool | `true` | Master switch for boot-time preloading of entries marked `preload: true`. Setting this to `false` keeps cold-start fast. |
| `features.images` | bool | `true` | If `false`, `ImageModelManager` never initializes. `POST /v1/images/generations` returns `403 feature_disabled`. |
| `features.embeddings` | bool | `true` | If `false`, `EmbeddingModelManager` never initializes. `POST /v1/embeddings` returns `403 feature_disabled`. |

### Env overrides

Each boolean above can be overridden by an environment variable; see the full
table in [setup.md](setup.md#environment-variables).

| Setting | Override |
|---|---|
| `autoDownload` | `COREML_AUTO_DOWNLOAD=0` |
| `preload` | `COREML_PRELOAD=0` |
| `features.images` | `COREML_IMAGES=0` |
| `features.embeddings` | `COREML_EMBEDDINGS=0` |

Any value matching `"0"`/`"false"`/`"no"`/`"off"` (case-insensitive) disables
the flag; anything else enables it. See
`ServerSettings.applyingEnvironment` in
[ModelRegistry.swift:128](../Sources/ModelRuntime/ModelRegistry.swift).

---

## `models` array

Each entry describes one model the server can serve. Two shapes are common:

### Minimal chat entry

```jsonc
{
  "id":   "my-model-int4",
  "kind": "chat",
  "repo": "your-hf-user/my-model-coreml-int4",
  "include": ["*.mlpackage"],
  "tokenizerRepo": "your-hf-user/my-model-base",
  "preload": true
}
```

### Full entry (all fields)

```jsonc
{
  "id":            "mistral-7b-int4",
  "kind":          "chat",
  "repo":          "apple/mistral-coreml",
  "runtime":       "standard",
  "include":       ["StatefulMistral7BInstructInt4.mlpackage/*"],
  "packageName":   "StatefulMistral7BInstructInt4.mlpackage",
  "tokenizerRepo": "mistralai/Mistral-7B-Instruct-v0.3",
  "preload":       true,
  "maxNewTokens":  512,
  "notes":         "Official Apple conversion. Known-good baseline; supports function calling."
}
```

### Field reference

| Field | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `id` | string | yes | — | Local id used in API requests (`"model": "..."`). Must be unique. |
| `kind` | enum | no | `chat` | `chat` \| `image` \| `embedding`. Dispatches to the matching runtime module. |
| `repo` | string | yes | — | Hugging Face repo id (e.g. `apple/mistral-coreml`). Prefixing with `local:` (e.g. `"local:/abs/path/to/repo"`) uses a local checkout instead. |
| `runtime` | enum | no | `standard` | `standard` (in-house stateful backend) \| `coreml-llm` (external `john-rocky/CoreML-LLM`). Only used for chat entries. |
| `include` | string[] | no | `[]` | Glob patterns selecting which files to download from `repo`. Path-anchored patterns (`StatefulMistral7BInstructInt4.mlpackage/*`) are the norm; bare globs like `*.mlpackage` work for full-package repos. |
| `packageName` | string | no | first match sorted alphabetically | Explicit `.mlpackage` / `.mlmodelc` directory name inside the repo. Required when `include` matches more than one package. |
| `tokenizerRepo` | string | no | `repo` | Where to fetch `tokenizer.json` / `tokenizer_config.json` / `config.json`. Point this at the base HF model when the converted repo doesn't ship them. |
| `preload` | bool | no | `false` | AND-gated with the global `settings.preload`: download + compile + load + warm up at server start (background). |
| `maxNewTokens` | int | no | `512` | Default per-request generation cap. Clients can still set `max_tokens` lower. |
| `architecture` | string | only for embedding | — | One of `"bert"` \| `"modernbert"` \| `"nomicbert"`. Selects the swift-embeddings adapter. |
| `imageSize` | int | only for image | `512` | Native pixel size — 512 for SD 1.x/2.x, 1024 for SDXL. `POST /v1/images/generations` 400s if `size` doesn't match. |
| `notes` | string | no | — | Free-form. Echoed back in `/v1/models` for humans reading the registry. |

---

## Per-`kind` notes

### `chat` (default)

- Pick `runtime: "standard"` for any model following the **stateful
  exporters convention** (`inputIds` → `logits`, `keyCache`/`valueCache`
  MLStates) — this is `apple/mistral-coreml`, the Qwen3 top-k package, and
  anything you produce with `scripts/convert_chat_model.py`. Tool calling
  works.
- Pick `runtime: "coreml-llm"` for bespoke HF Core ML repos that don't
  follow the convention (currently: `mlboydaisuke/gemma-4-E2B-coreml`). Text
  chat only on this runtime today; tool calling requires `standard`.
- `tokenizerRepo` should be a standard transformers-compatible repo so
  `AutoTokenizer.from(...)` and `LanguageModelConfigurationFromHub`
  succeed.

### `image`

- `include` should select the `Resources/` directory from
  `ml-stable-diffusion`'s `--bundle-resources-for-swift-cli` step, or the
  `original/compiled/*` directory from Apple's pre-compiled SD repos.
- `imageSize` must match the model's native size; mismatched `size` requests
  fail with 400.
- Image models are large — leave `preload: false` unless you want them warm
  on boot.

### `embedding`

- No conversion needed: `swift-embeddings` reads `safetensors` directly.
- Set `architecture` to one of:
  - `"bert"` for the original BERT family (e.g. MiniLM).
  - `"modernbert"` for ModernBERT-family models.
  - `"nomicbert"` for nomic-embed / nomic-bert variants.
- Embedding dim is not auto-discovered; match it when documenting.

---

## Per-`runtime` notes (chat)

### `standard`

- Uses swift-transformers' stateful `LanguageModel` (and `TopKLanguageModel`
  for top-k-exported variants).
- Detection is automatic: `ModelManager.detectInterface` probes the compiled
  model for `logits` (→ `.logits`) or `int32`+`float` outputs (→ `.topK`).
  See [ModelManager.swift:252](../Sources/ModelRuntime/ModelManager.swift).
- Tool calling is supported (Mistral `[TOOL_CALLS]` format).

### `coreml-llm`

- Uses `john-rocky/CoreML-LLM` via the `ExternalRuntime` module.
- For repos like `mlboydaisuke/gemma-4-E2B-coreml` that ship their own
  custom chat template; the server injects gemma-4-style `<tool>`
  declarations and parses `<tool_call>call:NAME{…}<tool_call|>` outputs.
  See `Sources/ExternalRuntime/Gemma4ToolsSupport.swift`.
- The provided `gemma-4-E2B` entry in `models.json` is the canonical config.

---

## Validation behavior

| Bad input | Server action |
|---|---|
| Missing file | Warns, starts with empty registry. |
| `models` missing | Same — empty registry. |
| Entry missing `id` | Skip the entry, log a warning. |
| Entry missing `repo` | Skip the entry, log a warning. |
| `include` matches no package | `download()` fails with `ModelError.packageNotFound`. `/v1/models` shows `"failed"`. |
| Duplicate `id` | Server uses the first; later ones in the array are effectively shadowed. |
| `tokenizerRepo` not accessible | `runner(for:)` fails at load time with a `downloadFailed` message; the entry's status goes to `failed`. |

---

## See also

- [setup.md](setup.md) — install / build / run, env vars.
- [models.md](models.md) — recommended entries per task, what doesn't drop in.
- [conversion.md](conversion.md) — converting a new HF causal LM into a
  registered `standard` entry.
- [operations.md](operations.md#status-state-machine) — what each
  `/v1/models` `status` value means.
