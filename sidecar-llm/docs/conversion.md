# Converting chat models to Core ML

This server serves Core ML chat models that follow the **stateful exporters
convention**: `inputIds` → `logits`, with `keyCache` and `valueCache` as
MLStates (so the Core ML runtime keeps the KV cache in hardware state across
calls). Apple's reference conversion is the `apple/mistral-coreml` package
(see the [HF blog post](https://huggingface.co/blog/mistral-coreml)).

Anything converted with this recipe, or by `scripts/convert_chat_model.py`,
plugs into the `runtime: "standard"` backend with no further work.

## 1. Environment

```bash
python -m venv .venv && source .venv/bin/activate
pip install --upgrade torch transformers coremltools huggingface_hub
# macOS 14+, Python 3.10+ recommended
```

coremltools 7.x+ supports the stateful MLProgram conversion path needed here.
8.x has an updated quantize API — the script handles both.

## 2. Quick start

Convert any causal LM on the Hub into a stateful `.mlpackage`:

```bash
python scripts/convert_chat_model.py \
    --model-id Qwen/Qwen2.5-1.5B-Instruct \
    --quantize int4 \
    --context 1024 \
    --output-dir ./models
```

Produces e.g. `./models/Qwen2.5-1.5B-Instruct-INT4-ctx1024.mlpackage/` and prints a
ready-to-paste `models.json` entry.

## 3. The recipe, step by step

```python
import torch, coremltools as ct
from transformers import AutoModelForCausalLM, AutoTokenizer

model = AutoModelForCausalLM.from_pretrained(
    "Qwen/Qwen2.5-1.5B-Instruct", torch_dtype=torch.float32).eval()
tokenizer = AutoTokenizer.from_pretrained("Qwen/Qwen2.5-1.5B-Instruct")

with torch.no_grad():
    traced = torch.jit.trace(
        model, (torch.zeros(1, 1, dtype=torch.int32),), strict=False)

mlmodel = ct.convert(
    traced,
    inputs=[
        ct.TensorType(name="inputIds",
                      shape=(1, ct.RangeDim(1, 1024)),
                      dtype=int),
        ct.TensorType(name="causalMask",
                      shape=(1, 1, 1, ct.RangeDim(1, 1025)),
                      dtype=float),
    ],
    outputs=[ct.TensorType(name="logits", dtype=float)],
    states=[
        ct.StateType(name="keyCache",   shape=(1, 32, 1, 64), dtype=float),
        ct.StateType(name="valueCache", shape=(1, 32, 1, 64), dtype=float),
    ],
    convert_to="mlprogram",
    compute_units=ct.ComputeUnit.CPU_AND_NE,
)
mlmodel.save("Qwen2.5-1.5B-Instruct-coreml-int4.mlpackage")
tokenizer.save_pretrained("Qwen2.5-1.5B-Instruct-coreml-int4.mlpackage")
```

Key choices:

- **`torch.float32`** as the source dtype. Float16 traces can lead to NaN logits
  during stateful decode (this is the gotcha that bit several community
  conversions). fp32 → coremltools fp16 storage runs cleanly.
- **Ranged sequence dims** (`ct.RangeDim(1, 1024)`). The graph compiles once and
  handles every sequence length in [1, max].
- **`causalMask`** is required by Mistral-family / Gemma-family / Qwen models;
  some small models (GPT-2 derivatives) instead take `attentionMask` and the
  underlying attention handles causality. Match the model's expectations.
- **`keyCache`/`valueCache` states** match the model's KV layout: layers × heads
  × dim. The `(1, 32, 1, 64)` above is a placeholder; real shapes come from
  `model.config.num_hidden_layers × num_attention_heads × head_dim`.

## 4. Quantization

```python
from coremltools.optimize.coreml import linear_quantize_weights, OptimizationConfig
from coremltools.optimize.coreml import OpLinearQuantizerConfig

config = OptimizationConfig(
    global_config=OpLinearQuantizerConfig(
        mode="linear_symmetric", weight_threshold=4096,
        nbits=4, block_size=32,
    ),
)
mlmodel = linear_quantize_weights(mlmodel, config=config)
```

`weight_threshold=4096` skips quantizing tiny ops (biases, norms). `nbits=8`
gives ~2× the size of `nbits=4` with almost no quality loss on most LLMs.
`block_size=32` is the Mistral recipe; for Qwen/Gemma 64 may work better — try
both.

## 5. Registering

Add an entry to `models.json`:

```jsonc
{
  "id": "qwen2.5-1.5b-int4",
  "kind": "chat",
  "runtime": "standard",
  "repo": "your-username/qwen2.5-1.5b-coreml-int4",   // or local path
  "include": ["Qwen2.5-1.5B-Instruct-INT4-ctx1024.mlpackage/*"],
  "packageName": "Qwen2.5-1.5B-Instruct-INT4-ctx1024.mlpackage",
  "tokenizerRepo": "Qwen/Qwen2.5-1.5B-Instruct",
  "preload": false,
  "maxNewTokens": 512,
  "notes": "Converted via scripts/convert_chat_model.py — Int4, 1024 ctx."
}
```

Upload to the Hub (or leave `repo: "local:/abs/path"` to keep it private) and
the server picks it up on next boot.

## 6. Verifying the conversion

After loading on this server:

- `GET /v1/models` shows the entry as `ready`.
- `POST /v1/chat/completions` with a simple message returns a non-empty answer.
- If the model emits EOS immediately, the prompt template isn't right (the
  chat template lives in `tokenizer_config.json` — make sure that file shipped
  with the package and that you're using a chat-tuned base).
- If the model returns repetitive text, double-check the source dtype was
  `float32`.

## 7. Image models

This server hosts Stable Diffusion packages directly from the Hub. To convert
your own SD weights to Core ML use Apple's `ml-stable-diffusion`
[`torch2coreml` script](https://github.com/apple/ml-stable-diffusion#converting-models-to-core-ml),
not this script. Output the compiled `.mlmodelc` set into a folder and register:

```jsonc
{
  "id": "my-sdxl",
  "kind": "image",
  "runtime": "standard",
  "repo": "your-username/my-sdxl-coreml",
  "include": ["Resources/*"],
  "imageSize": 1024,
  "preload": false
}
```

The `include` glob should select the `Resources` directory from the
`--bundle-resources-for-swift-cli` conversion step.

## 8. Embeddings

Use the `swift-embeddings` package's HF targets directly (e.g.
`sentence-transformers/all-MiniLM-L6-v2`). No conversion needed; register
with `kind: "embedding"` and `architecture: "bert" | "modernbert" | "nomicbert"`.

## See also

- [setup.md](setup.md) — install, build, env vars, first-run smoke tests.
- [configuration.md](configuration.md) — full `models.json` schema; what
  the example registry entry on this page would decode to.
- [models.md](models.md) — verified working entries and per-task
  recommendations.
- [operations.md](operations.md#status-state-machine) — the status state
  machine you see while your freshly-converted model downloads + compiles.