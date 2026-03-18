#!/usr/bin/env python3
"""
Pre-download all ML models from HuggingFace.

Run this before first startup to avoid cold-start delays:
    python scripts/download_models.py

Models are cached in ~/.cache/huggingface/ (or HF_HUB_CACHE).
"""

import argparse
import sys
import os


def download_asr(model_name: str):
    """Download Parakeet TDT model via parakeet-mlx."""
    print(f"\n{'='*50}")
    print(f"  Downloading ASR (MLX): {model_name}")
    print(f"{'='*50}")
    from parakeet_mlx import from_pretrained
    model = from_pretrained(model_name)
    print(f"  ✅ ASR model downloaded: {model_name}")
    del model


def download_diarizer():
    """Download Sortformer + TitaNet diarization models."""
    print(f"\n{'='*50}")
    print(f"  Downloading Diarization: Sortformer + TitaNet")
    print(f"{'='*50}")
    from nemo.collections.asr.models import SortformerEncLabelModel
    model = SortformerEncLabelModel.from_pretrained(
        model_name="nvidia/diar_sortformer_4spk-v1"
    )
    print("  ✅ Sortformer diarizer downloaded")
    del model


def download_vad():
    """Download Silero VAD model."""
    print(f"\n{'='*50}")
    print(f"  Downloading VAD: Silero")
    print(f"{'='*50}")
    import torch
    model, utils = torch.hub.load(
        "snakers4/silero-vad",
        "silero_vad",
        onnx=False,
        trust_repo=True,
    )
    print("  ✅ Silero VAD downloaded")
    del model


def download_tts():
    """Download LuxTTS model."""
    print(f"\n{'='*50}")
    print(f"  Downloading TTS: LuxTTS")
    print(f"{'='*50}")
    try:
        from luxtts import LuxTTS
        model = LuxTTS()
        print("  ✅ LuxTTS model downloaded")
        del model
    except ImportError:
        print("  ⚠️  LuxTTS not installed — skipping (pip install luxtts)")


def main():
    parser = argparse.ArgumentParser(description="Pre-download GoTranscribeSrv ML models")
    parser.add_argument(
        "--model",
        default="mlx-community/parakeet-tdt-0.6b-v3",
        help="ASR model name (default: mlx-community/parakeet-tdt-0.6b-v3)",
    )
    parser.add_argument("--skip-asr", action="store_true", help="Skip ASR download")
    parser.add_argument("--skip-diarizer", action="store_true", help="Skip diarizer download")
    parser.add_argument("--skip-vad", action="store_true", help="Skip VAD download")
    parser.add_argument("--skip-tts", action="store_true", help="Skip TTS download")
    args = parser.parse_args()

    print("GoTranscribeSrv — Model Downloader")
    print(f"Cache dir: {os.environ.get('HF_HUB_CACHE', '~/.cache/huggingface')}")

    if not args.skip_asr:
        download_asr(args.model)
    if not args.skip_diarizer:
        download_diarizer()
    if not args.skip_vad:
        download_vad()
    if not args.skip_tts:
        download_tts()

    print(f"\n{'='*50}")
    print("  All models downloaded! Start with: make up && make sidecar")
    print(f"{'='*50}")


if __name__ == "__main__":
    main()
