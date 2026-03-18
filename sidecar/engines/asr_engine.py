"""
ASR Engine — Parakeet TDT via MLX (Apple Silicon).

Uses parakeet-mlx for hardware-accelerated speech recognition
on Apple Silicon, leveraging the MLX framework for optimal
Neural Engine / GPU utilization.

NOTE: NVIDIA Nemotron ASR Streaming (nemotron-speech-streaming-en-0.6b)
is a strong candidate for the real-time WebSocket streaming path.
Its MLX port requires causal_downsampling support in parakeet-mlx
which is not yet implemented. For GPU deployments, Nemotron can be
loaded natively via NeMo for true cache-aware streaming inference.
"""

import logging
import time
import io
from pathlib import Path

import numpy as np
import soundfile as sf

logger = logging.getLogger(__name__)


class ASREngine:
    """Wraps Parakeet TDT (MLX) for file and streaming ASR."""

    def __init__(self, model_name: str = "mlx-community/parakeet-tdt-0.6b-v3"):
        self.model_name = model_name
        self.model = None
        self._load_model()

    def _load_model(self):
        """Load the Parakeet TDT model via parakeet-mlx."""
        from parakeet_mlx import from_pretrained

        logger.info(f"Loading ASR model (MLX): {self.model_name}")
        self.model = from_pretrained(self.model_name)
        logger.info("ASR model loaded on Apple Silicon (MLX)")

    def warmup(self):
        """Run a short dummy transcription to pre-compile the MLX graph."""
        logger.info("Running ASR warmup (1s silent audio)...")
        try:
            silence = np.zeros(16000, dtype=np.float32)
            buf = io.BytesIO()
            sf.write(buf, silence, 16000, format="WAV")
            buf.seek(0)
            self.transcribe(audio_bytes=buf.getvalue(), filename="warmup.wav")
            logger.info("ASR warmup complete")
        except Exception as e:
            logger.warning(f"ASR warmup failed (non-fatal): {e}")

    def transcribe(
        self,
        audio_bytes: bytes,
        filename: str = "audio.wav",
        language: str = "en",
    ) -> dict:
        """
        Transcribe audio bytes and return text with timestamps.

        Returns:
            dict with keys: text, segments, words, duration, processing_time_ms, model
        """
        start = time.perf_counter()

        # Write to temp file — parakeet-mlx expects a file path
        import tempfile
        with tempfile.NamedTemporaryFile(suffix=self._suffix(filename), delete=False) as tmp:
            tmp.write(audio_bytes)
            tmp_path = tmp.name

        try:
            result = self.model.transcribe(tmp_path)
        finally:
            Path(tmp_path).unlink(missing_ok=True)

        processing_time_ms = int((time.perf_counter() - start) * 1000)

        # Parse AlignedResult into our API format
        transcript = self._parse_result(result, processing_time_ms)
        return transcript

    def _suffix(self, filename: str) -> str:
        """Extract file suffix, defaulting to .wav."""
        if "." in filename:
            return "." + filename.rsplit(".", 1)[-1]
        return ".wav"

    def _parse_result(self, result, processing_time_ms: int) -> dict:
        """Parse parakeet-mlx AlignedResult into our API format."""
        text = result.text

        # Extract word-level timestamps from sentences → tokens
        raw_tokens = []
        segments = []

        for sentence in (result.sentences or []):
            # Build segment from sentence
            segments.append({
                "start": sentence.start,
                "end": sentence.end,
                "text": sentence.text,
            })

            # Collect raw tokens for word merging
            for token in (sentence.tokens or []):
                raw_tokens.append({
                    "text": token.text,
                    "start": token.start,
                    "end": token.end,
                })

        # Merge BPE sub-word tokens into actual words.
        # Tokens starting with a space indicate a new word boundary.
        words = self._merge_tokens_to_words(raw_tokens)

        # Calculate duration from last segment or from words
        if segments:
            duration = segments[-1]["end"]
        elif words:
            duration = words[-1]["end"]
        else:
            duration = 0.0

        return {
            "text": text,
            "segments": segments,
            "words": words,
            "duration": round(duration, 3),
            "processing_time_ms": processing_time_ms,
            "model": self.model_name.split("/")[-1],
            "diarized": False,
        }

    @staticmethod
    def _merge_tokens_to_words(tokens: list[dict]) -> list[dict]:
        """
        Merge BPE sub-word tokens into actual words.

        Parakeet TDT's tokenizer produces sub-word tokens like:
          " W", "el", "c", "ome" → "Welcome"

        A token starting with a space character signals a new word.
        Punctuation tokens (.,!?) are attached to the preceding word.
        """
        if not tokens:
            return []

        words = []
        current_text = ""
        current_start = 0.0
        current_end = 0.0

        for tok in tokens:
            tok_text = tok["text"]
            tok_start = tok["start"]
            tok_end = tok["end"]

            # Detect word boundaries: space prefix or punctuation-only token
            is_new_word = tok_text.startswith(" ") and len(tok_text) > 1
            is_punctuation = tok_text.strip() in {".", ",", "!", "?", ";", ":", "'", '"', "-"}

            if is_new_word and current_text:
                # Flush the current word
                words.append({
                    "word": current_text.strip(),
                    "start": current_start,
                    "end": current_end,
                })
                current_text = tok_text
                current_start = tok_start
                current_end = tok_end
            elif is_punctuation and current_text:
                # Attach punctuation to previous word
                current_text += tok_text
                current_end = tok_end
            elif not current_text:
                # First token
                current_text = tok_text
                current_start = tok_start
                current_end = tok_end
            else:
                # Continue building current word
                current_text += tok_text
                current_end = tok_end

        # Flush final word
        if current_text.strip():
            words.append({
                "word": current_text.strip(),
                "start": current_start,
                "end": current_end,
            })

        return words
