#!/usr/bin/env python3
"""Convert a Hugging Face causal LM into the stateful Core ML convention served
by this server.

The output .mlpackage has:
  - inputIds (Int32, sequence axis ranged)
  - causalMask (Float16, sequence axis ranged) when the model needs one
  - outputs logits (Float16/32, range shape)
  - states keyCache + valueCache (Float16 MLState)

Quantization choices: fp16, int8 (per-channel symmetric), int4 (block-32).
This is the same recipe apple/mistral-coreml uses; the resulting package
loads through swift-transformers' LanguageModel and serves through this
server's standard chat backend.

Usage:
  python scripts/convert_chat_model.py \
      --model-id Qwen/Qwen2.5-1.5B-Instruct \
      --quantize int4 \
      --context 1024 \
      --output-dir ./models/Qwen2.5-1.5B-Instruct-coreml-int4

Options:
  --model-id   HF repo id (required). Local paths also accepted.
  --quantize   fp16 | int8 | int4 (default int4)
  --context    max exported sequence length (default 1024)
  --source-dtype  float32 | float16 (default float32; safer for stateful)
  --revision   HF revision (branch/tag/sha), default main
  --output-dir destination directory for the .mlpackage
  --tokenizer-repo   HF repo to source tokenizer.json/config.json from when
                     the converted repo doesn't ship them
"""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--model-id", required=True)
    p.add_argument("--quantize", choices=["fp16", "int8", "int4"], default="int4")
    p.add_argument("--context", type=int, default=1024)
    p.add_argument("--source-dtype", choices=["float32", "float16"], default="float32")
    p.add_argument("--revision", default="main")
    p.add_argument("--output-dir", required=True)
    p.add_argument("--tokenizer-repo", default=None,
                  help="HF repo for tokenizer files (defaults to --model-id)")
    p.add_argument("--upload", metavar="REPO_ID",
                  help="If set, upload the converted package to the given HF repo")
    p.add_argument("--upload-private", action="store_true")
    return p.parse_args()


def check_dependencies() -> None:
    missing = []
    for pkg in ("torch", "transformers", "coremltools"):
        try:
            __import__(pkg)
        except ImportError:
            missing.append(pkg)
    if missing:
        print(
            f"Missing Python packages: {', '.join(missing)}\n"
            "Install with:\n"
            "  pip install torch transformers coremltools huggingface_hub",
            file=sys.stderr)
        sys.exit(2)


def main() -> None:
    args = parse_args()
    check_dependencies()

    import torch
    from transformers import AutoModelForCausalLM, AutoTokenizer
    import coremltools as ct
    from coremltools.models.neural_network.quantization_utils import quantize_weights

    out_dir = Path(args.output_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    package_name = (
        Path(args.model_id).name.replace("/", "_")
        + f"-{args.quantize.upper()}-ctx{args.context}.mlpackage"
    )
    package_dir = out_dir / package_name

    print(f"Loading {args.model_id} (revision {args.revision}, dtype={args.source_dtype}) ...")
    dtype = torch.float32 if args.source_dtype == "float32" else torch.float16
    tokenizer = AutoTokenizer.from_pretrained(args.model_id, revision=args.revision)
    model = AutoModelForCausalLM.from_pretrained(
        args.model_id, revision=args.revision, torch_dtype=dtype
    )
    model.eval()

    # Stateful conversion requires ranged sequence-length inputs so the Core ML
    # graph is compiled once and reused across sequence lengths.
    seq_axis = "sequence"
    min_len, max_len = 1, args.context
    sample_ids = torch.zeros((1, 1), dtype=torch.int32)
    sample_mask = torch.zeros((1, 1, 1, 1), dtype=torch.float16)

    print("Tracing model ...")
    with torch.no_grad():
        traced = torch.jit.trace(
            model,
            (sample_ids,),
            strict=False,
        )

    print("Converting to Core ML (stateful, ranged sequence) ...")
    mlmodel = ct.convert(
        traced,
        inputs=[
            ct.TensorType(
                name="inputIds",
                shape=(1, ct.RangeDim(1, max_len)),
                dtype=int,
            ),
            ct.TensorType(
                name="causalMask",
                shape=(1, 1, 1, ct.RangeDim(1, max_len + 1)),
                dtype=float,
            ),
        ],
        outputs=[
            ct.TensorType(name="logits", dtype=float),
        ],
        states=[
            ct.StateType(name="keyCache", shape=(1, 32, 1, 64), dtype=float),
            ct.StateType(name="valueCache", shape=(1, 32, 1, 64), dtype=float),
        ],
        # Forces the stateful code path on macOS 14+.
        convert_to="mlprogram",
        compute_units=ct.ComputeUnit.CPU_AND_NE,
    )

    if args.quantize in ("int8", "int4"):
        print(f"Quantizing weights to {args.quantize.upper()} ...")
        # Note: signature differs across coremltools versions; in 7.x it's
        # `linear_quantize`, in 8.x the optimizer-based API is preferred. The
        # block below covers the most common 8.x path; adjust if needed.
        try:
            from coremltools.optimize.coreml import linear_quantize_weights
            config = None
            if args.quantize == "int4":
                from coremltools.optimize.coreml.config import OptimizationConfig
                from coremltools.optimize.coreml import OpLinearQuantizerConfig
                config = OptimizationConfig(global_config=OpLinearQuantizerConfig(
                    mode="linear_symmetric", weight_threshold=4096,
                    nbits=4, block_size=32))
            else:  # int8
                from coremltools.optimize.coreml.config import OptimizationConfig
                from coremltools.optimize.coreml import OpLinearQuantizerConfig
                config = OptimizationConfig(global_config=OpLinearQuantizerConfig(
                    mode="linear_symmetric", weight_threshold=4096, nbits=8))
            mlmodel = linear_quantize_weights(mlmodel, config=config)
        except Exception as exc:  # pragma: no cover - version-dependent
            print(f"Warning: post-quantize failed ({exc}); keeping fp16 weights.")

    print(f"Writing {package_dir} ...")
    mlmodel.save(str(package_dir))

    # Tokenizer files: prefer the source repo; otherwise pull from
    # --tokenizer-repo (defaults to --model-id).
    tok_repo = args.tokenizer_repo or args.model_id
    print(f"Saving tokenizer files from {tok_repo} ...")
    tok = AutoTokenizer.from_pretrained(tok_repo)
    tok.save_pretrained(str(package_dir))

    # Generated models.json entry.
    entry = {
        "id": Path(args.model_id).name.lower().replace("-", "-") + "-" + args.quantize,
        "kind": "chat",
        "repo": "local:" + str(package_dir),
        "runtime": "standard",
        "include": [],
        "packageName": package_name,
        "tokenizerRepo": tok_repo,
        "preload": False,
        "maxNewTokens": 512,
        "notes": (
            f"Converted locally from {args.model_id} ({args.quantize.upper()}, "
            f"context {args.context})."
        ),
    }
    print("\nAppend this entry to your models.json:\n")
    import json
    print(json.dumps(entry, indent=2))

    if args.upload:
        from huggingface_hub import HfApi
        api = HfApi()
        api.create_repo(args.upload, private=args.upload_private, repo_type="model", exist_ok=True)
        api.upload_folder(folder_path=str(package_dir), repo_id=args.upload, commit_message="Upload Core ML package")
        print(f"Uploaded to https://huggingface.co/{args.upload}")


if __name__ == "__main__":
    main()