"""
TTS Router — Text-to-speech endpoint.

POST /synthesize — Synthesize speech from text using LuxTTS
"""

import base64
import logging

from fastapi import APIRouter
from fastapi.responses import Response
from pydantic import BaseModel
from typing import Optional

from inference_pool import run_inference

logger = logging.getLogger(__name__)

router = APIRouter()


class SynthesizeRequest(BaseModel):
    """TTS synthesis request body."""
    text: str
    voice: str = "default"
    voice_ref: Optional[str] = None  # Base64-encoded audio reference
    speed: float = 1.0
    format: str = "wav"


@router.post("/synthesize")
async def synthesize(req: SynthesizeRequest):
    """Synthesize speech from text using LuxTTS."""
    from main import get_engine

    tts = get_engine("tts")

    # Decode voice reference if provided
    voice_ref_bytes = None
    if req.voice_ref:
        try:
            voice_ref_bytes = base64.b64decode(req.voice_ref)
        except Exception:
            logger.warning("Invalid base64 voice reference, using preset")

    # Synthesize in thread pool (non-blocking)
    audio_bytes, content_type = await run_inference(
        tts.synthesize,
        text=req.text,
        voice=req.voice,
        voice_ref=voice_ref_bytes,
        speed=req.speed,
        audio_format=req.format,
    )

    return Response(
        content=audio_bytes,
        media_type=content_type,
        headers={
            "X-Audio-Sample-Rate": "48000",
        },
    )


@router.get("/voices")
async def list_voices():
    """List available voice presets."""
    from main import get_engine

    tts = get_engine("tts")
    return {"voices": tts.list_voices()}
