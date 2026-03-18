"""
ASR Router — Diarization endpoint only.

NOTE: File-based ASR has moved to the Node.js CoreML sidecar (port 8101).
This router retains the diarization endpoint that enriches transcripts
from the Node.js sidecar with speaker labels.

POST /diarize — Add speaker labels to a transcript
"""

import logging
from fastapi import APIRouter, File, Form, UploadFile
from typing import Optional

from inference_pool import run_inference

logger = logging.getLogger(__name__)

router = APIRouter()


@router.post("/diarize")
async def diarize(
    audio: UploadFile = File(...),
    transcript: str = Form(...),
):
    """
    Add speaker diarization to an existing transcript.

    Accepts:
        - audio: The original audio file (needed by Sortformer)
        - transcript: JSON string of the transcript from the ASR sidecar
                      (must include text, segments, words fields)

    Returns the transcript enriched with speaker labels.
    """
    import json as json_mod
    from main import get_engine

    audio_bytes = await audio.read()

    try:
        transcript_data = json_mod.loads(transcript)
    except json_mod.JSONDecodeError as e:
        return {"error": {"code": "INVALID_TRANSCRIPT", "message": f"Invalid transcript JSON: {e}"}}

    try:
        diarizer = get_engine("diarizer")
    except RuntimeError:
        return {"error": {"code": "DIARIZER_NOT_LOADED", "message": "Diarization engine not loaded"}}

    # Run diarization in thread pool (non-blocking)
    result = await run_inference(
        diarizer.diarize, audio_bytes, transcript_data, vad_engine=None
    )

    return result
