"""
GoTranscribeSrv — Python Inference Sidecar

FastAPI server providing diarization (Sortformer), TTS (LuxTTS),
and LLM (MLX) endpoints. ASR and VAD have moved to the Node.js
CoreML sidecar for ANE-accelerated inference.

Communicates with the Go backend over localhost HTTP.
"""

import logging
import os
import sys

import uvicorn
from fastapi import FastAPI
from contextlib import asynccontextmanager

from config import Settings
from engines.diarizer import Diarizer
from routers import asr, process

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s | %(levelname)s | %(name)s | %(message)s",
    stream=sys.stdout,
)
logger = logging.getLogger("sidecar")

# Global engine instances
engines: dict = {}


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Load all ML models on startup, cleanup on shutdown."""
    logger.info("Loading ML models...")

    settings = Settings()

    # Offline mode: skip HuggingFace network checks (models must already be cached)
    if settings.offline_mode:
        os.environ["HF_HUB_OFFLINE"] = "1"
        os.environ["TRANSFORMERS_OFFLINE"] = "1"
        os.environ["HF_DATASETS_OFFLINE"] = "1"
        logger.info("Offline mode ON — using cached models only (no network)")

    # NOTE: ASR, VAD, and TTS have moved to the Swift CoreML sidecar (port 8101).
    # This Python sidecar only loads diarization and LLM.

    # Load diarizer (optional — degrade gracefully)
    if settings.enable_diarization:
        try:
            logger.info("Loading diarization models (Sortformer + TitaNet)...")
            engines["diarizer"] = Diarizer()
            logger.info("Diarizer loaded")
        except Exception as e:
            logger.error(f"Diarizer failed to load (continuing without): {e}")

    # Load LLM engine (optional — degrade gracefully)
    if settings.enable_llm:
        try:
            logger.info(f"Loading LLM model: {settings.llm_model}")
            from engines.llm_engine import LLMEngine

            engines["llm"] = LLMEngine(model_name=settings.llm_model)
            logger.info("LLM engine loaded")
        except Exception as e:
            logger.error(f"LLM engine failed to load (continuing without): {e}")

    loaded = list(engines.keys())
    logger.info(f"Startup complete — loaded engines: {loaded}")
    yield

    # Cleanup
    logger.info("Shutting down inference sidecar")
    engines.clear()


# Create FastAPI app
app = FastAPI(
    title="GoTranscribeSrv Inference Sidecar",
    version="0.1.0",
    lifespan=lifespan,
)


@app.get("/health")
async def health():
    """Health check endpoint."""
    from inference_pool import queue_info

    model_status = {}
    for name, engine in engines.items():
        model_status[name] = "loaded" if engine is not None else "not loaded"
    return {
        "status": "ok",
        "models": model_status,
        "inference_pool": queue_info(),
    }


# Mount routers
app.include_router(asr.router)
app.include_router(process.router)


def get_engine(name: str):
    """Get a loaded engine by name. Used by routers."""
    engine = engines.get(name)
    if engine is None:
        raise RuntimeError(f"Engine '{name}' not loaded")
    return engine


if __name__ == "__main__":
    settings = Settings()
    workers = settings.effective_workers

    # reload mode doesn't support multiple workers
    if settings.reload:
        workers = 1

    logger.info(f"Starting sidecar with {workers} worker(s) on port {settings.sidecar_port}")
    if workers > 1:
        logger.info(
            f"  Each worker loads its own model copies (~4 GB each)."
            f" Total model memory: ~{workers * 4} GB"
        )

    uvicorn.run(
        "main:app",
        host=settings.host,
        port=settings.sidecar_port,
        log_level="info",
        reload=settings.reload,
        workers=workers,
    )

