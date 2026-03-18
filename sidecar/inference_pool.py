"""
Inference Pool — Concurrency control for ML inference.

Provides run_inference() which:
1. Acquires a semaphore to limit queued requests (backpressure)
2. Runs the blocking inference call in a SINGLE-THREAD executor
   so it doesn't block the async event loop

IMPORTANT: MLX and PyTorch/MPS both use Apple's Metal GPU framework,
which is NOT thread-safe for concurrent inference. Running two Metal
calls from different threads causes a segfault. We use a single-worker
executor to serialize all GPU work to one dedicated thread.

The semaphore limits how many requests can queue up waiting for the
GPU thread, preventing unbounded memory growth from buffered audio.
"""

import asyncio
import functools
import logging
from concurrent.futures import ThreadPoolExecutor

logger = logging.getLogger(__name__)

# ── Configuration ──────────────────────────────────────────────
# GPU work is serialized to a single thread (Metal is not thread-safe).
# This semaphore limits how many requests can QUEUE for the GPU.
# Requests beyond this will wait, not error.
MAX_QUEUED_INFERENCE = 8

# Lazily created to avoid spawning threads before MLX/Metal init.
_executor: ThreadPoolExecutor | None = None
_semaphore: asyncio.Semaphore | None = None


def _get_executor() -> ThreadPoolExecutor:
    """
    Get or create the thread pool executor (lazy init).

    Uses max_workers=1 because MLX and MPS (Metal) are NOT thread-safe.
    Two threads calling Metal simultaneously = segfault.
    """
    global _executor
    if _executor is None:
        _executor = ThreadPoolExecutor(
            max_workers=1,
            thread_name_prefix="inference",
        )
    return _executor


def _get_semaphore() -> asyncio.Semaphore:
    """Get or create the queue-depth semaphore for the current event loop."""
    global _semaphore
    if _semaphore is None:
        _semaphore = asyncio.Semaphore(MAX_QUEUED_INFERENCE)
    return _semaphore


async def run_inference(fn, *args, **kwargs):
    """
    Run a blocking inference function without blocking the event loop.

    Usage:
        result = await run_inference(asr.transcribe, audio_bytes=data)

    - Acquires the queue semaphore (limits waiting requests)
    - Runs `fn(*args, **kwargs)` on the dedicated GPU thread
    - Returns the result

    All GPU work is serialized to a single thread because Metal
    (used by both MLX and PyTorch MPS) is not thread-safe.
    """
    sem = _get_semaphore()
    async with sem:
        loop = asyncio.get_running_loop()
        result = await loop.run_in_executor(
            _get_executor(),
            functools.partial(fn, *args, **kwargs),
        )
        return result


def queue_info() -> dict:
    """Return info about the inference pool for health/debug endpoints."""
    sem = _get_semaphore()
    return {
        "max_queued": MAX_QUEUED_INFERENCE,
        "queued": MAX_QUEUED_INFERENCE - sem._value,
        "available_slots": sem._value,
    }

