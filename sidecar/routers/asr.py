"""
ASR Router — Transcription and streaming endpoints.

POST /transcribe — File-based transcription
WS   /stream    — Real-time streaming ASR
"""

import io
import logging
import json

import numpy as np
from fastapi import APIRouter, File, Form, UploadFile, WebSocket, WebSocketDisconnect
from typing import Optional

logger = logging.getLogger(__name__)

router = APIRouter()


@router.post("/transcribe")
async def transcribe(
    audio: UploadFile = File(...),
    language: str = Form("en"),
    diarize: Optional[str] = Form(None),
):
    """Transcribe an uploaded audio file."""
    from main import get_engine

    audio_bytes = await audio.read()

    # Run ASR
    asr = get_engine("asr")
    result = asr.transcribe(
        audio_bytes=audio_bytes,
        filename=audio.filename or "audio.wav",
        language=language,
    )

    # Run diarization if requested
    if diarize == "true":
        try:
            diarizer = get_engine("diarizer")
            try:
                vad = get_engine("vad")
            except RuntimeError:
                vad = None
            result = diarizer.diarize(audio_bytes, result, vad_engine=vad)
        except RuntimeError:
            logger.warning("Diarization requested but engine not loaded")

    return result



@router.websocket("/stream")
async def stream_asr(websocket: WebSocket):
    """
    Real-time streaming ASR over WebSocket.

    Client sends binary audio chunks (PCM 16-bit, 16kHz, mono).
    Server sends JSON text frames with partial/final transcripts.
    """
    await websocket.accept()
    from main import get_engine

    asr = get_engine("asr")
    vad = get_engine("vad")

    # Create streaming VAD iterator
    vad_iterator = vad.create_iterator(sample_rate=16000)

    # Send ready signal
    await websocket.send_json({"type": "ready"})

    audio_buffer = bytearray()
    sample_rate = 16000

    try:
        while True:
            data = await websocket.receive()

            if "text" in data:
                # Control message
                try:
                    msg = json.loads(data["text"])
                    if msg.get("action") == "stop":
                        break
                except json.JSONDecodeError:
                    pass
                continue

            if "bytes" not in data:
                continue

            chunk = data["bytes"]
            audio_buffer.extend(chunk)

            # Convert chunk to numpy for VAD
            chunk_np = np.frombuffer(chunk, dtype=np.int16).astype(np.float32) / 32768.0

            # Check for speech via VAD
            if not vad.is_speech(chunk_np, sample_rate):
                # No speech detected — if we have buffered audio, process it
                if len(audio_buffer) > sample_rate * 2:  # >1 second of audio
                    audio_np = np.frombuffer(bytes(audio_buffer), dtype=np.int16).astype(np.float32) / 32768.0

                    # Transcribe the buffered audio
                    import soundfile as sf
                    buf = io.BytesIO()
                    sf.write(buf, audio_np, sample_rate, format="WAV")
                    result = asr.transcribe(buf.getvalue(), language="en")

                    await websocket.send_json({
                        "type": "final",
                        "text": result["text"],
                        "start": 0.0,
                        "end": result["duration"],
                        "words": result.get("words", []),
                        "is_final": True,
                    })

                    audio_buffer.clear()
            else:
                # Speech detected — send partial result periodically
                if len(audio_buffer) % (sample_rate * 2) < len(chunk):
                    # Quick partial transcription
                    audio_np = np.frombuffer(bytes(audio_buffer), dtype=np.int16).astype(np.float32) / 32768.0
                    buf = io.BytesIO()
                    import soundfile as sf
                    sf.write(buf, audio_np, sample_rate, format="WAV")

                    try:
                        result = asr.transcribe(buf.getvalue(), language="en")
                        await websocket.send_json({
                            "type": "partial",
                            "text": result["text"],
                            "is_final": False,
                        })
                    except Exception as e:
                        logger.warning(f"Partial transcription failed: {e}")

    except WebSocketDisconnect:
        logger.info("WebSocket client disconnected")
    except Exception as e:
        logger.error(f"WebSocket error: {e}")
        try:
            await websocket.send_json({"type": "error", "message": str(e)})
        except Exception:
            pass
    finally:
        # Process any remaining audio
        if len(audio_buffer) > sample_rate:
            try:
                audio_np = np.frombuffer(bytes(audio_buffer), dtype=np.int16).astype(np.float32) / 32768.0
                buf = io.BytesIO()
                import soundfile as sf
                sf.write(buf, audio_np, sample_rate, format="WAV")
                result = asr.transcribe(buf.getvalue(), language="en")

                await websocket.send_json({
                    "type": "final",
                    "text": result["text"],
                    "start": 0.0,
                    "end": result["duration"],
                    "words": result.get("words", []),
                    "is_final": True,
                })
            except Exception:
                pass

        try:
            await websocket.send_json({"type": "done"})
        except Exception:
            pass

    logger.info("Streaming ASR session ended")
