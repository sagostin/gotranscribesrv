"""
Process Router — LLM-powered transcript processing endpoints.

Provides POST /process for running LLM tasks (summarize, action items, etc.)
and GET /process/tasks for listing available task types.
"""

import logging
from typing import Optional

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

from inference_pool import run_inference

logger = logging.getLogger(__name__)

router = APIRouter(tags=["process"])


class ProcessRequest(BaseModel):
    """Request body for transcript processing."""

    transcript_text: str = Field(
        ..., description="Transcript text to process", min_length=1
    )
    task: str = Field(
        default="summarize",
        description="Processing task: summarize, action_items, topics, translate, qa, custom",
    )
    language: Optional[str] = Field(
        default=None,
        description="Target language (for translate task)",
    )
    prompt: Optional[str] = Field(
        default=None,
        description="Custom prompt or question (for qa/custom tasks)",
    )
    max_tokens: int = Field(
        default=1024,
        ge=1,
        le=4096,
        description="Maximum tokens to generate",
    )
    temperature: float = Field(
        default=0.3,
        ge=0.0,
        le=2.0,
        description="Sampling temperature (lower = more deterministic)",
    )


class ProcessResponse(BaseModel):
    """Response from transcript processing."""

    result: str
    task: str
    model: str
    processing_time_ms: int
    tokens_generated: int


@router.post("/process", response_model=ProcessResponse)
async def process_transcript(req: ProcessRequest):
    """Process a transcript with a local LLM."""
    from main import get_engine

    try:
        llm = get_engine("llm")
    except RuntimeError:
        raise HTTPException(
            status_code=503,
            detail="LLM engine not available. Set ENABLE_LLM=true to enable.",
        )

    # Validate task-specific requirements
    if req.task == "translate" and not req.language:
        raise HTTPException(
            status_code=400,
            detail="'language' field is required for translate task",
        )
    if req.task in ("qa", "custom") and not req.prompt:
        raise HTTPException(
            status_code=400,
            detail=f"'prompt' field is required for {req.task} task",
        )

    try:
        result = await run_inference(
            llm.process,
            text=req.transcript_text,
            task=req.task,
            language=req.language,
            prompt=req.prompt,
            max_tokens=req.max_tokens,
            temperature=req.temperature,
        )
        return ProcessResponse(**result)
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e))
    except Exception as e:
        logger.error(f"LLM processing failed: {e}")
        raise HTTPException(status_code=500, detail=f"Processing failed: {e}")


@router.get("/process/tasks")
async def list_tasks():
    """List available LLM processing tasks."""
    from engines.llm_engine import AVAILABLE_TASKS

    return {
        "tasks": AVAILABLE_TASKS,
        "descriptions": {
            "summarize": "Generate a concise summary of the transcript",
            "action_items": "Extract tasks, decisions, and follow-ups",
            "topics": "Identify main topics and key entities",
            "translate": "Translate transcript to another language (requires 'language' field)",
            "qa": "Answer a question about the transcript (requires 'prompt' field)",
            "custom": "Run a custom prompt against the transcript (requires 'prompt' field)",
        },
    }
