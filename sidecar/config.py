"""Sidecar configuration via environment variables."""

from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    """Inference sidecar settings."""

    # Server
    host: str = "0.0.0.0"
    sidecar_port: int = 8100
    reload: bool = False

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

    # Streaming
    vad_threshold: float = 0.5
    chunk_duration_ms: int = 250

    class Config:
        env_prefix = ""
        env_file = "../.env"
        extra = "ignore"
