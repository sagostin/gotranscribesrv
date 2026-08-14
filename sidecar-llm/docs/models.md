# Models: compatibility, per-task recommendations, gotchas

This server runs on three different "runtimes":

| Runtime | Backend | What it serves |
|---|---|---|
| `standard` | swift-transformers `LanguageModel` (in-tree) | chat LLMs that follow the **stateful exporters convention** (`inputIds` → `logits`, `keyCache`/`valueCache` MLStates), **and** their top-k variant (top-k adapter). |
| `coreml-llm` | external CoreML-LLM package (`john-rocky/CoreML-LLM`) | bespoke HF CoreML repos like `mlboydaisuke/gemma-4-E2B-coreml` that don't follow the exporters convention. Text chat only on this endpoint (no tools). |
| image / embedding | `ml-stable-diffusion` / `swift-embeddings` | SD 1.5 / SDXL, MiniLM-family sentence embeddings. |

A model entry can opt into either chat backend via `"runtime": "standard" | "coreml-llm"`.

## Verified working out of the box

| Model id (in `models.json`) | HF repo | Task | Notes |
|---|---|---|---|
| `mistral-7b-int4` | `apple/mistral-coreml` | chat + tools | The baseline. Int4, ~4 GB, stateful KV cache, function-calling trained. |
| `mistral-7b-fp16` | `apple/mistral-coreml` | chat + tools, higher quality | ~14 GB; same interface, more RAM. |
| `qwen3-1.7b-w8` | `groxaxo/qwen3-1.7b-coreml-int8` | fast chat (base) | Stateful top-k output → served via the top-k adapter. ~2 GB. Use `/v1/completions` for clean output; chat with a base model emits EOS first. |
| `gemma-4-E2B` | `mlboydaisuke/gemma-4-E2B-coreml` | chat + tools (gemma-4 format) | Text-only via `coreml-llm` backend; tool calling works (we inject the gemma-4 `<tool>` declarations and parse `<tool_call>call:NAME{args}<tool_call\|>`). Original is text+image+audio+video — only the text path is exposed on `/v1/chat/completions`. |
| `sd-1.5` | `apple/coreml-stable-diffusion-v1-5-palettized` | image generation | 6-bit palettized, 512×512. |
| `sdxl` | `apple/coreml-stable-diffusion-xl-base` | image generation | 1024×1024, larger download. |
| `all-minilm-l6-v2` | `sentence-transformers/all-MiniLM-L6-v2` | embeddings | 384-dim, sentence-transformers defaults. |

## Recommending models per task

| Task | Best option in this server today |
|---|---|
| Tool-using agent | `mistral-7b-int4` (function-calling trained). Also: `gemma-4-E2B` (via `coreml-llm`). |
| Fast dev / CI / smoke tests | `qwen3-1.7b-w8` via `/v1/completions` (low RAM, instant). |
| Higher quality chat | `mistral-7b-fp16`. |
| Local-only "open-source-ish" alternative to commercial chat | `gemma-4-E2B` (gemma 3n architecture, INT4). |
| Image generation | `sd-1.5` default; `sdxl` for quality. |
| Sentence embeddings | `all-minilm-l6-v2`; register more from `sentence-transformers/*` or `nomic-ai/*`. |
| Coding assistance | Convert `Qwen2.5-Coder-7B-Instruct` via `scripts/convert_chat_model.py` and register. |
| Vision / audio understanding | Not supported on this server yet (gemma E2B is multimodal but only text is exposed). |

## Models that don't drop in

We tried or inspected these and **they don't work directly**:

| HF repo | Why |
|---|---|
| `okayuji/gemma-4-12b-it-coreml-128k` | Speculative-decoding chunked runtime, split encoder/decoder, IO-based KV cache. Not the exporters convention. |
| `mlboydaisuke/qwen3.5-0.8B-CoreML` | Custom chat_config / split decoder chunks. Same reason. |
| `finnvoorhees/coreml-Qwen2.5-0.5B-Instruct-4bit` | Splits into embed + chunk + logits with IO-based KV. swift-transformers explicitly `fatalError`s on this layout. |
| `leok7v/QwenPaw-Flash-*-coreml`, `aufklarer/Qwen3.5-0.8B-Chat-CoreML`, etc. | Bespoke chunked runtimes. |

For any of these: convert the **base** model (the original PyTorch/HF transformers
weights) with `scripts/convert_chat_model.py` into the stateful exporters
convention and register the output. That's the supported path.

For `mlboydaisuke/gemma-4-E2B-coreml`: instead of converting, use the
`runtime: "coreml-llm"` entry that ships in `models.json` — the upstream
CoreML-LLM package handles the bespoke layout.

## Common gotchas

- **Docker squats `localhost` IPv6** on macOS — use `127.0.0.1`.
- **First-run compile** is slow (minutes for a 7B); subsequent runs are fast.
- **AutoDownload off** → un-downloaded models 409 on use. Explicit
  `/models/{id}/download` still works.
- **Top-k adapters** only decode when the model is run with `generate` —
  greedy/temperature sampling is applied to the scattered scores. Vocabulary
  of the base tokenizer must match the exported model.
- **gemma (coreml-llm)** does not pass through tool calling — gemma's tool
  format differs from Mistral's. If you need tools, use Mistral int4.
- **Model size** on disk (GB) = roughly: int4 weights ≈ 0.55 bytes/parameter,
  fp16 ≈ 2.05, plus a ~150 MB per-layer KV cache at the model's context.
- **Gated repos** (google/gemma-*) require `HUGGING_FACE_HUB_TOKEN`.
- **Embedding dims**: not auto-discovered at registration time — current
  entries are 384 (MiniLM). Add others with `architecture: "bert" | "modernbert" | "nomicbert"`.

## Conversion cheatsheet (full guide in `docs/conversion.md`)

```bash
python scripts/convert_chat_model.py \
    --model-id Qwen/Qwen2.5-1.5B-Instruct \
    --quantize int4 --context 1024 \
    --output-dir ./models
```

Add a registry entry pointing at the produced `.mlpackage`. The script prints
the entry for you.

## See also

- [conversion.md](conversion.md) — full recipe for converting a Hugging Face
  causal LM into the stateful Core ML convention.
- [configuration.md](configuration.md) — every `models.json` entry field,
  env-var overrides, per-runtime notes.
- [endpoints.md](endpoints.md) and [formats.md](formats.md) — how to hit
  the entries listed here once they're registered.
- [operations.md](operations.md#status-state-machine) — what
  `downloading → compiling → ready` looks like on these specific models.