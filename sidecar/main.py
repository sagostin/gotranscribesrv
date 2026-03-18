"""
GoTranscribeSrv — Python Inference Sidecar

FastAPI server providing ASR (Parakeet TDT), diarization (Sortformer),
TTS (LuxTTS), and VAD (Silero) endpoints. Communicates with the Go
backend over localhost HTTP/WebSocket.
"""

import logging
import sys

import uvicorn
from fastapi import FastAPI
from contextlib import asynccontextmanager

from config import Settings
from engines.asr_engine import ASREngine
from engines.diarizer import Diarizer
from engines.tts_engine import TTSEngine
from engines.vad import VADEngine
from routers import asr, tts

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

    # Load ASR engine (required — exit if this fails)
    try:
        logger.info(f"Loading ASR model: {settings.asr_model}")
        engines["asr"] = ASREngine(
            model_name=settings.asr_model,
        )
        engines["asr"].warmup()
        logger.info("ASR engine loaded and warmed up")
    except Exception as e:
        logger.critical(f"ASR engine failed to load — cannot start: {e}")
        sys.exit(1)

    # Load diarizer (optional — degrade gracefully)
    if settings.enable_diarization:
        try:
            logger.info("Loading diarization models (Sortformer + TitaNet)...")
            engines["diarizer"] = Diarizer()
            logger.info("Diarizer loaded")
        except Exception as e:
            logger.error(f"Diarizer failed to load (continuing without): {e}")

    # Load TTS engine (optional — degrade gracefully)
    if settings.enable_tts:
        try:
            logger.info("Loading LuxTTS engine...")
            engines["tts"] = TTSEngine(voices_dir=settings.voices_dir)
            logger.info("TTS engine loaded")
        except Exception as e:
            logger.error(f"TTS engine failed to load (continuing without): {e}")

    # Load VAD (optional — degrade gracefully)
    try:
        logger.info("Loading Silero VAD...")
        engines["vad"] = VADEngine()
        logger.info("VAD loaded")
    except Exception as e:
        logger.error(f"VAD failed to load (continuing without): {e}")

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
    model_status = {}
    for name, engine in engines.items():
        model_status[name] = "loaded" if engine is not None else "not loaded"
    return {"status": "ok", "models": model_status}


# Mount routers
app.include_router(asr.router)
app.include_router(tts.router)


def get_engine(name: str):
    """Get a loaded engine by name. Used by routers."""
    engine = engines.get(name)
    if engine is None:
        raise RuntimeError(f"Engine '{name}' not loaded")
    return engine


if __name__ == "__main__":
    settings = Settings()
    uvicorn.run(
        "main:app",
        host=settings.host,
        port=settings.sidecar_port,
        log_level="info",
        reload=settings.reload,
    )
