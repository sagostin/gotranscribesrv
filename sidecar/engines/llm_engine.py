"""
LLM Engine — Local LLM inference via mlx-lm (Apple Silicon).

Provides text generation for transcript post-processing tasks:
summarization, action item extraction, translation, Q&A, and custom prompts.

Uses mlx-lm for hardware-accelerated inference on Apple Silicon,
leveraging the same MLX framework as the ASR engine.
"""

import logging
import time
from typing import Optional

logger = logging.getLogger(__name__)

# ── Prompt templates ────────────────────────────────────────────

_SYSTEM_PROMPT = (
    "You are a precise assistant that processes speech transcripts. "
    "Respond only with the requested output — no preamble, no commentary."
)

_TASK_PROMPTS = {
    "summarize": (
        "Summarize the following transcript concisely. "
        "Capture the key points, decisions, and outcomes.\n\n"
        "TRANSCRIPT:\n{text}\n\nSUMMARY:"
    ),
    "action_items": (
        "Extract all action items, tasks, and follow-ups from this transcript. "
        "Format each as a bullet point with the responsible person (if mentioned) "
        "and any deadline.\n\n"
        "TRANSCRIPT:\n{text}\n\nACTION ITEMS:"
    ),
    "topics": (
        "Extract the main topics and key entities discussed in this transcript. "
        "List each topic with a one-line description.\n\n"
        "TRANSCRIPT:\n{text}\n\nKEY TOPICS:"
    ),
    "translate": (
        "Translate the following transcript into {language}. "
        "Preserve speaker labels if present.\n\n"
        "TRANSCRIPT:\n{text}\n\nTRANSLATION:"
    ),
    "qa": (
        "Answer the following question based only on the transcript provided. "
        "If the answer is not in the transcript, say so.\n\n"
        "TRANSCRIPT:\n{text}\n\nQUESTION: {prompt}\n\nANSWER:"
    ),
    "custom": (
        "{prompt}\n\n"
        "TRANSCRIPT:\n{text}\n\nRESPONSE:"
    ),
}

# Available task types for the /process/tasks endpoint
AVAILABLE_TASKS = list(_TASK_PROMPTS.keys())


class LLMEngine:
    """Local LLM inference via mlx-lm for transcript processing."""

    def __init__(self, model_name: str = "mlx-community/Meta-Llama-3.1-8B-Instruct-4bit"):
        self.model_name = model_name
        self.model = None
        self.tokenizer = None
        self._load_model()

    def _load_model(self):
        """Load the LLM model via mlx-lm."""
        from mlx_lm import load

        logger.info(f"Loading LLM model (MLX): {self.model_name}")
        self.model, self.tokenizer = load(self.model_name)
        logger.info("LLM model loaded on Apple Silicon (MLX)")

    def process(
        self,
        text: str,
        task: str = "summarize",
        language: Optional[str] = None,
        prompt: Optional[str] = None,
        max_tokens: int = 1024,
        temperature: float = 0.3,
    ) -> dict:
        """
        Process transcript text with the LLM.

        Args:
            text: Transcript text to process
            task: Task type (summarize, action_items, topics, translate, qa, custom)
            language: Target language (for translate task)
            prompt: Custom prompt or question (for qa/custom tasks)
            max_tokens: Maximum tokens to generate
            temperature: Sampling temperature (lower = more deterministic)

        Returns:
            dict with keys: result, task, model, processing_time_ms, tokens_generated
        """
        if self.model is None:
            raise RuntimeError("LLM engine not loaded")

        if task not in _TASK_PROMPTS:
            raise ValueError(
                f"Unknown task '{task}'. Available: {AVAILABLE_TASKS}"
            )

        start = time.perf_counter()

        # Build prompt from template
        user_prompt = self._build_prompt(text, task, language, prompt)

        # Generate response
        from mlx_lm import generate

        # Build chat messages for instruction-tuned models
        messages = [
            {"role": "system", "content": _SYSTEM_PROMPT},
            {"role": "user", "content": user_prompt},
        ]

        # Apply chat template if available
        if hasattr(self.tokenizer, "apply_chat_template"):
            formatted_prompt = self.tokenizer.apply_chat_template(
                messages, tokenize=False, add_generation_prompt=True
            )
        else:
            formatted_prompt = f"{_SYSTEM_PROMPT}\n\n{user_prompt}"

        response = generate(
            self.model,
            self.tokenizer,
            prompt=formatted_prompt,
            max_tokens=max_tokens,
            temp=temperature,
        )

        processing_time_ms = int((time.perf_counter() - start) * 1000)

        # Count generated tokens
        tokens_generated = len(self.tokenizer.encode(response))

        logger.debug(
            f"LLM {task}: {tokens_generated} tokens in {processing_time_ms}ms"
        )

        return {
            "result": response.strip(),
            "task": task,
            "model": self.model_name.split("/")[-1],
            "processing_time_ms": processing_time_ms,
            "tokens_generated": tokens_generated,
        }

    def _build_prompt(
        self,
        text: str,
        task: str,
        language: Optional[str],
        prompt: Optional[str],
    ) -> str:
        """Build the user prompt from a task template."""
        template = _TASK_PROMPTS[task]

        # Truncate text if extremely long (protect against OOM)
        max_chars = 32_000  # ~8k tokens roughly
        if len(text) > max_chars:
            text = text[:max_chars] + "\n\n[... transcript truncated ...]"
            logger.warning(
                f"Transcript truncated from {len(text)} to {max_chars} chars"
            )

        return template.format(
            text=text,
            language=language or "English",
            prompt=prompt or "",
        )
