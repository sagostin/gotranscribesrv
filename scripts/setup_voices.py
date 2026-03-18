#!/usr/bin/env python3
"""
Setup voice presets from LibriTTS-R (CC BY 4.0).

Downloads and curates 5 high-quality voice reference clips for LuxTTS
zero-shot voice cloning. Output: sidecar/voices/*.wav

Usage:
    python scripts/setup_voices.py
    python scripts/setup_voices.py --output sidecar/voices
"""

import argparse
import io
import os
import sys
from pathlib import Path

import numpy as np

# LibriTTS-R speaker IDs curated for voice diversity
# Each tuple: (speaker_id, clip_id, voice_name, description)
VOICE_PRESETS = [
    ("8555", "284447-0020", "default", "Neutral, clear American English male"),
    ("4446", "2275-149896-0001", "professional", "Formal, confident female"),
    ("1089", "134686-0001", "friendly", "Warm, conversational male"),
    ("7021", "79740-0004", "narrator", "Deep, documentary style male"),
    ("6829", "68771-0005", "bright", "Energetic, upbeat female"),
]

LIBRITTS_R_BASE = "https://huggingface.co/datasets/blaze999/libritts_r/resolve/main"


def download_and_trim(speaker_id: str, clip_id: str, output_path: Path, duration_sec: float = 12.0):
    """Download a LibriTTS-R clip and trim to target duration."""
    import urllib.request
    import soundfile as sf

    # LibriTTS-R file structure: train-clean-360/{speaker}/{chapter}/{clip}.wav
    # We'll try to find the clip via the HuggingFace API
    url = f"{LIBRITTS_R_BASE}/data/train-clean-360/{speaker_id}/{clip_id}.wav"

    print(f"    Downloading from {url}...")
    try:
        response = urllib.request.urlopen(url)
        audio_data = response.read()
    except Exception as e:
        print(f"    ⚠️  Download failed: {e}")
        print(f"    Generating placeholder tone instead...")
        generate_placeholder(output_path, duration_sec)
        return

    # Read and trim
    audio, sr = sf.read(io.BytesIO(audio_data))
    if len(audio.shape) > 1:
        audio = audio.mean(axis=1)

    # Trim to target duration
    max_samples = int(duration_sec * sr)
    if len(audio) > max_samples:
        audio = audio[:max_samples]

    # Normalize
    audio = audio / (np.abs(audio).max() + 1e-8) * 0.9

    sf.write(str(output_path), audio.astype(np.float32), sr)
    print(f"    ✅ Saved: {output_path.name} ({len(audio)/sr:.1f}s, {sr}Hz)")


def generate_placeholder(output_path: Path, duration_sec: float = 10.0):
    """Generate a placeholder silence file when download fails."""
    import soundfile as sf

    sr = 16000
    silence = np.zeros(int(duration_sec * sr), dtype=np.float32)
    sf.write(str(output_path), silence, sr)
    print(f"    ⚠️  Placeholder saved: {output_path.name} (replace with real voice clip)")


def main():
    parser = argparse.ArgumentParser(description="Setup LuxTTS voice presets from LibriTTS-R")
    parser.add_argument(
        "--output",
        default="sidecar/voices",
        help="Output directory (default: sidecar/voices)",
    )
    args = parser.parse_args()

    output_dir = Path(args.output)
    output_dir.mkdir(parents=True, exist_ok=True)

    print("GoTranscribeSrv — Voice Preset Setup")
    print(f"Output: {output_dir.resolve()}")
    print(f"Source: LibriTTS-R (CC BY 4.0)\n")

    for speaker_id, clip_id, voice_name, description in VOICE_PRESETS:
        print(f"  [{voice_name}] {description}")
        output_path = output_dir / f"{voice_name}.wav"

        if output_path.exists():
            print(f"    ⏭️  Already exists, skipping")
            continue

        download_and_trim(speaker_id, clip_id, output_path)

    print(f"\n{'='*50}")
    print(f"  {len(VOICE_PRESETS)} voice presets ready in {output_dir}/")
    print(f"  Replace placeholders with real clips for best quality.")
    print(f"{'='*50}")


if __name__ == "__main__":
    main()
