"""
TTS Engine — LuxTTS wrapper.

Provides text-to-speech synthesis with zero-shot voice cloning.
Supports preset voices (from LibriTTS-R) and custom voice references.
Outputs 48 kHz audio.
"""

import io
import logging
import time
from pathlib import Path
from typing import Optional

import numpy as np
import soundfile as sf

logger = logging.getLogger(__name__)


class TTSEngine:
    """LuxTTS text-to-speech with voice cloning."""

    SAMPLE_RATE = 48000

    def __init__(self, voices_dir: str = "voices"):
        self.voices_dir = Path(voices_dir)
        self.model = None
        self.voice_cache: dict[str, np.ndarray] = {}
        self._load_model()
        self._load_voice_presets()

    def _load_model(self):
        """Load the LuxTTS model."""
        try:
            # Add LuxTTS clone to Python path (for local/make sidecar runs)
            import sys
            luxtts_path = Path(__file__).parent.parent / "LuxTTS"
            if luxtts_path.exists() and str(luxtts_path) not in sys.path:
                sys.path.insert(0, str(luxtts_path))

            from zipvoice.luxvoice import LuxTTS

            import platform
            if platform.system() == "Darwin":
                device = "mps"
            else:
                device = "cpu"

            self.model = LuxTTS("YatharthS/LuxTTS", device=device)
            logger.info("LuxTTS model loaded (device=%s)", device)
        except ImportError:
            logger.warning(
                "LuxTTS/zipvoice not installed. TTS will be unavailable. "
                "See: https://github.com/ysharma3501/LuxTTS"
            )
            self.model = None

    def _load_voice_presets(self):
        """Load pre-built voice presets from the voices directory."""
        if not self.voices_dir.exists():
            logger.warning(f"Voices directory not found: {self.voices_dir}")
            return

        for voice_file in self.voices_dir.glob("*.wav"):
            voice_name = voice_file.stem
            try:
                audio, sr = sf.read(voice_file)
                if len(audio.shape) > 1:
                    audio = audio.mean(axis=1)
                self.voice_cache[voice_name] = audio.astype(np.float32)
                logger.info(f"Loaded voice preset: {voice_name}")
            except Exception as e:
                logger.error(f"Failed to load voice {voice_name}: {e}")

        logger.info(f"Loaded {len(self.voice_cache)} voice presets")

    def synthesize(
        self,
        text: str,
        voice: str = "default",
        voice_ref: Optional[bytes] = None,
        speed: float = 1.0,
        audio_format: str = "wav",
    ) -> tuple[bytes, str]:
        """
        Synthesize speech from text.

        Args:
            text: Text to synthesize
            voice: Preset voice name
            voice_ref: Raw audio bytes for custom voice cloning
            speed: Playback speed (0.5-2.0)
            audio_format: Output format ("wav", "mp3", "opus")

        Returns:
            Tuple of (audio_bytes, content_type)
        """
        if self.model is None:
            raise RuntimeError("TTS engine not loaded")

        start = time.perf_counter()

        # Get reference audio for voice cloning.
        # encode_prompt → process_audio → librosa.load() expects a file path
        # or file-like object, NOT a raw numpy array.  We must wrap the audio
        # data in a BytesIO WAV buffer so librosa can read it.
        ref_audio_buf = None
        if voice_ref:
            # Custom voice reference provided — read it, then re-wrap as WAV
            audio_data, sr = sf.read(io.BytesIO(voice_ref))
            if len(audio_data.shape) > 1:
                audio_data = audio_data.mean(axis=1)
            buf = io.BytesIO()
            sf.write(buf, audio_data.astype(np.float32), sr, format="WAV")
            buf.seek(0)
            ref_audio_buf = buf
        elif voice in self.voice_cache:
            buf = io.BytesIO()
            sf.write(buf, self.voice_cache[voice], 24000, format="WAV")
            buf.seek(0)
            ref_audio_buf = buf
        elif "default" in self.voice_cache:
            buf = io.BytesIO()
            sf.write(buf, self.voice_cache["default"], 24000, format="WAV")
            buf.seek(0)
            ref_audio_buf = buf

        # Run TTS (LuxTTS is a two-step API: encode_prompt → generate_speech)
        try:
            encode_dict = None
            if ref_audio_buf is not None:
                encode_dict = self.model.encode_prompt(ref_audio_buf)

            if encode_dict is None:
                raise RuntimeError(
                    f"No voice reference available. Provide a voice_ref or "
                    f"add a .wav preset to '{self.voices_dir}' "
                    f"(available presets: {list(self.voice_cache.keys())})"
                )

            wav_tensor = self.model.generate_speech(
                text=text,
                encode_dict=encode_dict,
                speed=speed,
            )
            audio = wav_tensor.numpy().squeeze()
        except Exception as e:
            logger.error(f"TTS synthesis failed: {e}")
            raise RuntimeError(f"TTS synthesis failed: {e}")

        elapsed_ms = int((time.perf_counter() - start) * 1000)
        logger.debug(f"TTS synthesized {len(text)} chars in {elapsed_ms}ms")

        # Encode to requested format
        audio_bytes, content_type = self._encode_audio(audio, audio_format)
        return audio_bytes, content_type

    def _encode_audio(self, audio: np.ndarray, fmt: str) -> tuple[bytes, str]:
        """Encode audio array to bytes in the requested format."""
        buf = io.BytesIO()

        if fmt == "mp3":
            sf.write(buf, audio, self.SAMPLE_RATE, format="MP3")
            content_type = "audio/mpeg"
        elif fmt == "opus":
            sf.write(buf, audio, self.SAMPLE_RATE, format="OGG", subtype="OPUS")
            content_type = "audio/ogg"
        else:  # wav
            sf.write(buf, audio, self.SAMPLE_RATE, format="WAV")
            content_type = "audio/wav"

        return buf.getvalue(), content_type

    def list_voices(self) -> list[str]:
        """Return available voice preset names."""
        return list(self.voice_cache.keys())
