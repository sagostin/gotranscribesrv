"""
VAD Engine — Silero Voice Activity Detection.

Lightweight (~4MB) VAD for detecting speech segments in audio streams.
Runs on ANE/CPU so it doesn't compete with GPU-bound ASR/TTS inference.
"""

import logging
from pathlib import Path

import numpy as np
import torch

logger = logging.getLogger(__name__)


class VADEngine:
    """Silero VAD for speech detection and stream segmentation."""

    SAMPLE_RATE = 16000

    def __init__(self, threshold: float = 0.5):
        self.threshold = threshold
        self.model = None
        self._load_model()

    def _load_model(self):
        """Load Silero VAD model, preferring local cache to avoid network requests."""
        cache_dir = Path(torch.hub.get_dir()) / "snakers4_silero-vad_master"
        if cache_dir.exists():
            self.model, utils = torch.hub.load(
                str(cache_dir), "silero_vad", source="local", onnx=False,
            )
        else:
            self.model, utils = torch.hub.load(
                "snakers4/silero-vad", "silero_vad", onnx=False, trust_repo=True,
            )
        (
            self.get_speech_timestamps,
            self.save_audio,
            self.read_audio,
            self.VADIterator,
            self.collect_chunks,
        ) = utils
        logger.info("Silero VAD loaded")

    def detect_speech(self, audio: np.ndarray, sample_rate: int = 16000) -> list[dict]:
        """
        Detect speech segments in audio.

        Args:
            audio: Numpy audio array (float32, mono)
            sample_rate: Audio sample rate

        Returns:
            List of speech segments: [{"start": float, "end": float}, ...]
        """
        audio_tensor = torch.from_numpy(audio).float()

        speech_timestamps = self.get_speech_timestamps(
            audio_tensor,
            self.model,
            sampling_rate=sample_rate,
            threshold=self.threshold,
            min_speech_duration_ms=250,
            min_silence_duration_ms=300,
        )

        # Convert sample indices to seconds
        segments = []
        for ts in speech_timestamps:
            segments.append({
                "start": round(ts["start"] / sample_rate, 3),
                "end": round(ts["end"] / sample_rate, 3),
            })

        return segments

    def create_iterator(self, sample_rate: int = 16000):
        """Create a streaming VAD iterator for real-time processing."""
        return self.VADIterator(
            self.model,
            sampling_rate=sample_rate,
            threshold=self.threshold,
            min_silence_duration_ms=300,
        )

    def is_speech(self, audio_chunk: np.ndarray, sample_rate: int = 16000) -> bool:
        """Quick check if a single audio chunk contains speech."""
        audio_tensor = torch.from_numpy(audio_chunk).float()
        confidence = self.model(audio_tensor, sample_rate).item()
        return confidence >= self.threshold
