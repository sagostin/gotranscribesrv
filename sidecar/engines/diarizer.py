"""
Diarizer Engine — Sortformer + VAD speaker diarization.

Runs speaker diarization on an ASR transcript to assign speaker
labels to segments. Uses NeMo's Sortformer (end-to-end) model.

Pipeline:
  1. VAD pre-filter — strip non-speech (music, silence, IVR tones)
  2. Sortformer inference — produce per-frame speaker probabilities
  3. Parse diarization output into speaker segments
  4. Split ASR segments at speaker change boundaries
  5. Assign word-level speaker labels (consistent with segments)
  6. Merge consecutive same-speaker segments
  7. Build per-speaker summary (counts, durations)
"""

import io
import logging

import time


import numpy as np
import soundfile as sf
import torch

logger = logging.getLogger(__name__)

# Frame duration for Sortformer (0.08s per frame)
_FRAME_DUR = 0.08

# Minimum duration (seconds) for a speaker segment to be kept
_MIN_SEGMENT_DUR = 0.3


def _best_device() -> str:
    """Pick the best available PyTorch device (MPS > CPU)."""
    if torch.backends.mps.is_available():
        return "mps"
    return "cpu"


class Diarizer:
    """Speaker diarization using NeMo Sortformer."""

    def __init__(self):
        self.model = None
        self.device = "cpu"
        self._load_model()

    def _load_model(self):
        """Load NeMo's neural diarizer (Sortformer), with MPS acceleration if available."""
        import os
        from pathlib import Path
        from nemo.collections.asr.models import SortformerEncLabelModel

        logger.info("Loading Sortformer diarization model...")

        # NeMo's from_pretrained() always hits the network for HF Hub models,
        # which fails when HF_HUB_OFFLINE=1.  If we're in offline mode, use
        # restore_from() with the locally cached .nemo file instead.
        cached_nemo = self._find_cached_nemo(
            "models--nvidia--diar_sortformer_4spk-v1",
            "diar_sortformer_4spk-v1.nemo",
        )

        if cached_nemo:
            logger.info(f"Loading Sortformer from cache: {cached_nemo}")
            self.model = SortformerEncLabelModel.restore_from(
                restore_path=str(cached_nemo),
            )
        else:
            logger.info("Cached .nemo not found — downloading via from_pretrained")
            self.model = SortformerEncLabelModel.from_pretrained(
                model_name="nvidia/diar_sortformer_4spk-v1"
            )
        self.model.eval()

        # Try to move to MPS (Apple Silicon GPU)
        target = _best_device()
        if target != "cpu":
            try:
                self.model = self.model.to(target)
                self.device = target
                logger.info(f"Sortformer diarizer loaded on {target.upper()}")
            except Exception as e:
                logger.warning(
                    f"Failed to move Sortformer to {target}, falling back to CPU: {e}"
                )
                self.model = self.model.to("cpu")
                self.device = "cpu"
        else:
            self.device = "cpu"
            logger.info("Sortformer diarizer loaded on CPU")

    # ── Cache helpers ────────────────────────────────────────────

    @staticmethod
    def _find_cached_nemo(repo_dir_name: str, filename: str):
        """
        Find a cached .nemo file in the HuggingFace Hub cache.

        Searches: $HF_HUB_CACHE → $HF_HOME/hub → ~/.cache/huggingface/hub
        Returns the Path if found, else None.
        """
        import os
        from pathlib import Path

        cache_dirs = []
        if os.environ.get("HF_HUB_CACHE"):
            cache_dirs.append(Path(os.environ["HF_HUB_CACHE"]))
        if os.environ.get("HF_HOME"):
            cache_dirs.append(Path(os.environ["HF_HOME"]) / "hub")
        cache_dirs.append(Path.home() / ".cache" / "huggingface" / "hub")

        for cache_dir in cache_dirs:
            repo_dir = cache_dir / repo_dir_name
            if not repo_dir.exists():
                continue
            # Look in snapshots/<hash>/<filename>
            snapshots = repo_dir / "snapshots"
            if snapshots.exists():
                for snapshot in snapshots.iterdir():
                    candidate = snapshot / filename
                    if candidate.is_file():
                        return candidate
        return None

    # ── Public API ──────────────────────────────────────────────

    def diarize(
        self,
        audio_bytes: bytes,
        transcript: dict,
        vad_engine=None,
    ) -> dict:
        """
        Add speaker labels to transcript segments.

        Args:
            audio_bytes: Raw audio bytes
            transcript: Dict with 'text', 'segments', 'words' from ASR engine
            vad_engine: Optional VADEngine for pre-filtering non-speech

        Returns:
            Updated transcript dict with speaker labels on segments and words,
            plus 'num_speakers' (int) and 'speakers' (per-speaker summary).
        """
        start = time.perf_counter()

        # Decode audio
        audio_array, sample_rate = sf.read(io.BytesIO(audio_bytes))
        if len(audio_array.shape) > 1:
            audio_array = audio_array.mean(axis=1)

        # ── Step 1: VAD pre-filter ──────────────────────────────
        speech_regions = None
        if vad_engine is not None:
            try:
                speech_regions = vad_engine.detect_speech(audio_array, sample_rate)
                logger.debug(f"VAD detected {len(speech_regions)} speech regions")
            except Exception as e:
                logger.warning(f"VAD pre-filter failed (continuing without): {e}")

        # ── Step 2: Sortformer inference ────────────────────────
        # SortformerEncLabelModel.diarize() is not implemented in NeMo;
        # use forward() directly with audio tensors.
        audio_tensor = torch.tensor(audio_array, dtype=torch.float32).unsqueeze(0)  # [1, T]
        audio_length = torch.tensor([audio_tensor.shape[1]], dtype=torch.long)

        audio_tensor = audio_tensor.to(self.device)
        audio_length = audio_length.to(self.device)

        with torch.no_grad():
            preds = self.model.forward(
                audio_signal=audio_tensor,
                audio_signal_length=audio_length,
            )  # [1, frames, num_speakers]

        # preds is already a tensor the parser can handle
        diarization_result = preds

        # ── Step 3: Parse diarization output ────────────────────
        speaker_segments = self._parse_diarization(diarization_result)

        if not speaker_segments:
            logger.warning("Diarization produced no speaker segments")
            transcript["diarized"] = True
            return transcript

        # ── Step 3b: Filter by VAD speech regions ───────────────
        if speech_regions:
            speaker_segments = self._filter_by_vad(speaker_segments, speech_regions)

        # Filter out very short segments (noise)
        speaker_segments = [
            s for s in speaker_segments if (s["end"] - s["start"]) >= _MIN_SEGMENT_DUR
        ]

        logger.debug(
            f"Parsed {len(speaker_segments)} speaker segments after filtering"
        )

        # ── Step 4: Split ASR segments at speaker boundaries ────
        transcript = self._split_segments_at_speaker_changes(
            transcript, speaker_segments
        )

        # ── Step 5: Assign word-level speaker labels ────────────
        transcript = self._assign_word_speakers(transcript, speaker_segments)

        # ── Step 6: Merge consecutive same-speaker segments ─────
        transcript = self._merge_same_speaker_segments(transcript)

        transcript["diarized"] = True

        diarize_time = int((time.perf_counter() - start) * 1000)
        transcript["processing_time_ms"] += diarize_time

        # ── Step 7: Build speaker summary ───────────────────────
        speaker_summary = self._build_speaker_summary(transcript)
        transcript["num_speakers"] = len(speaker_summary)
        transcript["speakers"] = speaker_summary

        logger.info(
            f"Diarization complete: {len(speaker_summary)} speakers, "
            f"{len(transcript.get('segments', []))} segments in {diarize_time}ms"
        )

        return transcript

    # ── Sortformer output parsing ───────────────────────────────

    def _parse_diarization(self, result) -> list[dict]:
        """
        Parse NeMo Sortformer diarization output.

        Handles known output formats:
          1. List of Annotation objects (pyannote-style, with .itertracks())
          2. List of RTTM string lines
          3. Raw T×S probability tensor
          4. Objects with start/end attributes
        """
        speaker_segments = []

        # Log result structure for debugging
        if isinstance(result, (list, tuple)) and len(result) > 0:
            item0 = result[0]
            if isinstance(item0, (list, tuple)):
                logger.debug(
                    f"Sortformer returned list[list] with {len(item0)} entries"
                )
            else:
                logger.debug(
                    f"Sortformer returned list[{type(item0).__name__}]"
                )

        if result is None or (isinstance(result, list) and len(result) == 0):
            return speaker_segments

        # Unwrap single-item list (batch_size=1)
        item = result[0] if isinstance(result, list) else result

        # ── Format 1: pyannote Annotation ───────────────────────
        if hasattr(item, "itertracks"):
            try:
                for segment, track, label in item.itertracks(yield_label=True):
                    speaker_segments.append({
                        "speaker": str(label),
                        "start": float(segment.start),
                        "end": float(segment.end),
                    })
                if speaker_segments:
                    logger.debug(
                        f"Parsed {len(speaker_segments)} segments "
                        f"from pyannote Annotation"
                    )
                    return speaker_segments
            except Exception as e:
                logger.warning(f"Failed to parse Annotation: {e}")

        # ── Format 2: list of string lines ────────────────────────
        # Sortformer returns: ["0.240 3.120 speaker_0", ...]
        # Or full RTTM: ["SPEAKER file 1 0.24 2.88 <NA> <NA> speaker_0 ...", ...]
        if isinstance(item, str) or (
            isinstance(item, list)
            and len(item) > 0
            and isinstance(item[0], str)
        ):
            lines = [item] if isinstance(item, str) else item
            for line in lines:
                parsed = self._parse_speaker_line(line)
                if parsed:
                    speaker_segments.append(parsed)
            if speaker_segments:
                logger.info(
                    f"Parsed {len(speaker_segments)} segments from "
                    f"string lines (first: {lines[0][:50]})"
                )
                return speaker_segments

        # ── Format 3: T×S probability tensor ────────────────────
        try:
            import torch

            tensor = None
            if isinstance(item, torch.Tensor):
                tensor = item
            elif isinstance(item, np.ndarray):
                tensor = torch.from_numpy(item)
            elif isinstance(item, list) and len(item) > 0:
                # Could be nested list wrapping
                inner = item[0] if isinstance(item[0], (torch.Tensor, np.ndarray)) else None
                if inner is not None:
                    tensor = (
                        inner if isinstance(inner, torch.Tensor)
                        else torch.from_numpy(inner)
                    )

            if tensor is not None and tensor.dim() >= 2:
                # Squeeze batch dim if present
                if tensor.dim() == 3:
                    tensor = tensor.squeeze(0)  # [T, S]

                speaker_segments = self._tensor_to_segments(tensor)
                if speaker_segments:
                    logger.debug(
                        f"Parsed {len(speaker_segments)} segments "
                        f"from T×S tensor ({tensor.shape})"
                    )
                    return speaker_segments
        except ImportError:
            pass
        except Exception as e:
            logger.warning(f"Failed to parse tensor output: {e}")

        # ── Format 4: objects with start/end attributes ─────────
        # Fallback for any NeMo internal types
        items = item if isinstance(item, list) else [item]
        for entry in items:
            if hasattr(entry, "start") and hasattr(entry, "end"):
                speaker_segments.append({
                    "speaker": getattr(entry, "speaker", "SPEAKER_00"),
                    "start": float(entry.start),
                    "end": float(entry.end),
                })

        if speaker_segments:
            logger.debug(
                f"Parsed {len(speaker_segments)} segments from attribute objects"
            )

        return speaker_segments

    def _parse_speaker_line(self, line: str) -> dict | None:
        """
        Parse a speaker diarization line.

        Handles two formats:
          - Sortformer simple: "0.240 3.120 speaker_0"
          - Full RTTM: "SPEAKER <file> <chan> <start> <dur> <NA> <NA> <speaker> <NA> <NA>"
        """
        try:
            parts = line.strip().split()

            # Format: "start end speaker" (Sortformer simple output)
            if len(parts) == 3:
                start = float(parts[0])
                end = float(parts[1])
                speaker = parts[2].upper()
                # Normalize speaker labels to SPEAKER_XX format
                if not speaker.startswith("SPEAKER_"):
                    speaker = speaker.replace("SPEAKER", "SPEAKER_").replace("_", "_", 1)
                    if not speaker.startswith("SPEAKER_"):
                        speaker = f"SPEAKER_{speaker}"
                return {
                    "speaker": speaker,
                    "start": start,
                    "end": end,
                }

            # Format: Full RTTM
            if len(parts) >= 8 and parts[0] == "SPEAKER":
                start = float(parts[3])
                duration = float(parts[4])
                speaker = parts[7]
                return {
                    "speaker": speaker,
                    "start": start,
                    "end": start + duration,
                }
        except (ValueError, IndexError):
            pass
        return None

    def _tensor_to_segments(self, tensor) -> list[dict]:
        """
        Convert T×S probability tensor to speaker segments.

        Each row = one frame (~0.08s), each column = one speaker.
        Values are probabilities [0,1] of that speaker being active.
        """
        import torch

        # Threshold probabilities
        threshold = 0.5
        active = (tensor > threshold).int()  # [T, S]

        num_frames, num_speakers = active.shape
        segments = []

        for spk_idx in range(num_speakers):
            spk_active = active[:, spk_idx]

            # Find contiguous active regions
            in_segment = False
            seg_start = 0

            for frame_idx in range(num_frames):
                if spk_active[frame_idx] and not in_segment:
                    seg_start = frame_idx
                    in_segment = True
                elif not spk_active[frame_idx] and in_segment:
                    segments.append({
                        "speaker": f"SPEAKER_{spk_idx:02d}",
                        "start": round(seg_start * _FRAME_DUR, 3),
                        "end": round(frame_idx * _FRAME_DUR, 3),
                    })
                    in_segment = False

            # Close final segment
            if in_segment:
                segments.append({
                    "speaker": f"SPEAKER_{spk_idx:02d}",
                    "start": round(seg_start * _FRAME_DUR, 3),
                    "end": round(num_frames * _FRAME_DUR, 3),
                })

        # Sort by start time
        segments.sort(key=lambda s: s["start"])
        return segments

    # ── VAD filtering ───────────────────────────────────────────

    def _filter_by_vad(
        self, speaker_segments: list[dict], speech_regions: list[dict]
    ) -> list[dict]:
        """
        Keep only speaker segments that overlap with VAD speech regions.

        This removes diarization results that fall on music, silence, etc.
        """
        filtered = []
        for seg in speaker_segments:
            # Check if this speaker segment overlaps any speech region
            for region in speech_regions:
                overlap_start = max(seg["start"], region["start"])
                overlap_end = min(seg["end"], region["end"])
                overlap = overlap_end - overlap_start

                if overlap > 0:
                    # Clip segment to speech region boundaries
                    filtered.append({
                        "speaker": seg["speaker"],
                        "start": max(seg["start"], region["start"]),
                        "end": min(seg["end"], region["end"]),
                    })
                    break  # matched at least one region

        return filtered

    # ── Segment splitting ───────────────────────────────────────

    def _split_segments_at_speaker_changes(
        self, transcript: dict, speaker_segments: list[dict]
    ) -> dict:
        """
        Split ASR segments at speaker change boundaries.

        If a single ASR segment spans multiple speakers, break it into
        sub-segments at the speaker change point, using word timestamps
        to find the cleanest split.
        """
        words = transcript.get("words", [])
        original_segments = transcript.get("segments", [])
        new_segments = []

        for seg in original_segments:
            seg_start = seg.get("start", 0.0)
            seg_end = seg.get("end", 0.0)

            # Find all speaker changes within this segment's time range
            speakers_in_range = self._speakers_in_range(
                seg_start, seg_end, speaker_segments
            )

            if len(speakers_in_range) <= 1:
                # Single speaker — just assign the label
                speaker = (
                    speakers_in_range[0]["speaker"]
                    if speakers_in_range
                    else "SPEAKER_00"
                )
                seg["speaker"] = speaker
                new_segments.append(seg)
                continue

            # Multiple speakers — split the segment
            # Get words that belong to this segment
            seg_words = [
                w for w in words
                if w.get("start", 0) >= seg_start and w.get("end", 0) <= seg_end
            ]

            if not seg_words:
                # No words to split on — assign dominant speaker
                seg["speaker"] = self._dominant_speaker(
                    seg_start, seg_end, speaker_segments
                )
                new_segments.append(seg)
                continue

            # Split words by speaker and build sub-segments
            sub_segs = self._build_sub_segments(
                seg_words, speaker_segments, seg
            )
            new_segments.extend(sub_segs)

        transcript["segments"] = new_segments
        return transcript

    def _speakers_in_range(
        self, start: float, end: float, speaker_segments: list[dict]
    ) -> list[dict]:
        """Find all speaker segments that overlap with [start, end]."""
        result = []
        for spk_seg in speaker_segments:
            overlap_start = max(start, spk_seg["start"])
            overlap_end = min(end, spk_seg["end"])
            if overlap_end > overlap_start:
                result.append(spk_seg)
        return result

    def _dominant_speaker(
        self, start: float, end: float, speaker_segments: list[dict]
    ) -> str:
        """Find the speaker with the most overlap in [start, end]."""
        best_speaker = None
        best_overlap = 0.0

        for spk_seg in speaker_segments:
            overlap = max(
                0, min(end, spk_seg["end"]) - max(start, spk_seg["start"])
            )
            if overlap > best_overlap:
                best_overlap = overlap
                best_speaker = spk_seg["speaker"]

        # If no overlap found, find nearest speaker segment by midpoint
        if best_speaker is None and speaker_segments:
            mid = (start + end) / 2
            best_dist = float("inf")
            for spk_seg in speaker_segments:
                seg_mid = (spk_seg["start"] + spk_seg["end"]) / 2
                dist = abs(mid - seg_mid)
                if dist < best_dist:
                    best_dist = dist
                    best_speaker = spk_seg["speaker"]

        return best_speaker or "SPEAKER_0"

    def _build_sub_segments(
        self,
        words: list[dict],
        speaker_segments: list[dict],
        original_seg: dict,
    ) -> list[dict]:
        """
        Build sub-segments from words by grouping consecutive words
        with the same speaker.
        """
        if not words:
            return [original_seg]

        sub_segs = []
        current_speaker = None
        current_words = []

        for word in words:
            word_mid = (word.get("start", 0) + word.get("end", 0)) / 2
            speaker = self._dominant_speaker(
                word.get("start", 0), word.get("end", 0), speaker_segments
            )

            if speaker != current_speaker and current_words:
                # Speaker changed — flush current sub-segment
                sub_segs.append(self._words_to_segment(
                    current_words, current_speaker
                ))
                current_words = []

            current_speaker = speaker
            current_words.append(word)

        # Flush final group
        if current_words:
            sub_segs.append(self._words_to_segment(
                current_words, current_speaker
            ))

        return sub_segs

    def _words_to_segment(
        self, words: list[dict], speaker: str
    ) -> dict:
        """Build a segment dict from a list of words."""
        text = " ".join(w.get("word", "") for w in words).strip()
        return {
            "speaker": speaker,
            "start": words[0].get("start", 0.0),
            "end": words[-1].get("end", 0.0),
            "text": text,
        }

    # ── Word-level speaker attribution ──────────────────────────

    def _assign_word_speakers(
        self, transcript: dict, speaker_segments: list[dict]
    ) -> dict:
        """
        Assign a speaker label to each word based on timestamp overlap.

        Uses the parent segment's speaker as a tiebreaker to keep
        word-level and segment-level speaker sets consistent.
        """
        words = transcript.get("words", [])
        segments = transcript.get("segments", [])

        for word in words:
            w_start = word.get("start", 0.0)
            w_end = word.get("end", 0.0)

            # Prefer the parent segment's speaker if the word falls
            # entirely within a segment that already has a label.
            parent_speaker = None
            for seg in segments:
                if seg.get("start", 0) <= w_start and w_end <= seg.get("end", 0):
                    parent_speaker = seg.get("speaker")
                    break

            if parent_speaker:
                word["speaker"] = parent_speaker
            else:
                word["speaker"] = self._dominant_speaker(
                    w_start, w_end, speaker_segments
                )

        transcript["words"] = words
        return transcript

    # ── Same-speaker merging ────────────────────────────────────

    def _merge_same_speaker_segments(self, transcript: dict) -> dict:
        """
        Merge consecutive segments that share the same speaker.

        This produces cleaner output by combining split fragments
        back into larger blocks when the speaker hasn't changed.
        """
        segments = transcript.get("segments", [])
        if len(segments) <= 1:
            return transcript

        merged = [segments[0].copy()]

        for seg in segments[1:]:
            prev = merged[-1]

            # Merge if same speaker and gap is small (< 2s)
            if (
                seg.get("speaker") == prev.get("speaker")
                and (seg.get("start", 0) - prev.get("end", 0)) < 2.0
            ):
                prev["end"] = seg.get("end", prev["end"])
                prev_text = prev.get("text", "")
                seg_text = seg.get("text", "")
                prev["text"] = (prev_text + " " + seg_text).strip()
            else:
                merged.append(seg.copy())

        transcript["segments"] = merged
        return transcript

    # ── Speaker summary ─────────────────────────────────────────

    def _build_speaker_summary(self, transcript: dict) -> dict:
        """
        Build a per-speaker summary with segment counts, word counts,
        and total speaking duration.

        Returns:
            dict: e.g. {
                "SPEAKER_0": {"segment_count": 3, "word_count": 650,
                              "total_duration_s": 35.2},
                ...
            }
        """
        summary: dict[str, dict] = {}

        for seg in transcript.get("segments", []):
            spk = seg.get("speaker", "unknown")
            if spk not in summary:
                summary[spk] = {
                    "segment_count": 0,
                    "word_count": 0,
                    "total_duration_s": 0.0,
                }
            summary[spk]["segment_count"] += 1
            summary[spk]["total_duration_s"] += (
                seg.get("end", 0) - seg.get("start", 0)
            )

        for word in transcript.get("words", []):
            spk = word.get("speaker", "unknown")
            if spk in summary:
                summary[spk]["word_count"] += 1

        # Round durations for cleaner output
        for spk in summary:
            summary[spk]["total_duration_s"] = round(
                summary[spk]["total_duration_s"], 3
            )

        return summary
