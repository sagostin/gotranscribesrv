"""Sidecar configuration via environment variables."""

import os
from pydantic_settings import BaseSettings


def _auto_detect_workers() -> int:
    """
    Calculate safe worker count based on system RAM.

    Each worker loads its own model copies (~4 GB for ASR + TTS + VAD + runtime).
    Reserve ~4 GB for macOS + Go backend + PostgreSQL.
    """
    try:
        import psutil
        total_gb = psutil.virtual_memory().total / (1024 ** 3)
    except ImportError:
        # Fallback: read from sysctl on macOS
        try:
            import subprocess
            result = subprocess.run(
                ["sysctl", "-n", "hw.memsize"],
                capture_output=True, text=True, timeout=5,
            )
            total_gb = int(result.stdout.strip()) / (1024 ** 3)
        except Exception:
            return 1

    os_reserved = 4  # GB for macOS + Go + Postgres
    per_worker = 4   # GB per sidecar worker (models + runtime)
    available = total_gb - os_reserved
    workers = max(1, int(available // per_worker))
    return min(workers, 4)  # Cap at 4 — Metal bandwidth saturates beyond this


class Settings(BaseSettings):
    """Inference sidecar settings."""

    # Server
    host: str = "0.0.0.0"
    sidecar_port: int = 8100
    reload: bool = False

    # Workers — each worker is a separate process with its own model copies.
    # Metal (GPU) is not thread-safe, so parallelism requires separate processes.
    # 0 = auto-detect based on system RAM.
    sidecar_workers: int = 0

    # ASR — MLX-native Parakeet on Apple Silicon
    asr_model: str = "mlx-community/parakeet-tdt-0.6b-v3"

    # Features
    enable_diarization: bool = True
    enable_tts: bool = True
    enable_llm: bool = False  # Opt-in: requires ~4.5 GB extra RAM

    # LLM
    llm_model: str = "mlx-community/Meta-Llama-3.1-8B-Instruct-4bit"

    # Paths
    voices_dir: str = "voices"
    model_cache_dir: str = ""  # defaults to HF_HUB_CACHE

    # Offline mode — skip HuggingFace network checks (models must be cached)
    offline_mode: bool = True

    # Streaming
    vad_threshold: float = 0.5
    chunk_duration_ms: int = 250

    @property
    def effective_workers(self) -> int:
        """Return actual worker count (auto-detect if set to 0)."""
        if self.sidecar_workers > 0:
            return self.sidecar_workers
        return _auto_detect_workers()

    class Config:
        env_prefix = ""
        env_file = "../.env"
        extra = "ignore"

