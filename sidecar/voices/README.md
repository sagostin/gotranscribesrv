# Voice Presets

This directory previously contained pre-built voice reference clips for LuxTTS.

**LuxTTS has been removed** — TTS is now handled exclusively by the Swift sidecar
using **PocketTTS** (via FluidAudio, CoreML/ANE).

## System Voices

PocketTTS includes 17+ built-in voices (Jane, Charles, Alba, etc.) that are
available through the `GET /api/v1/voices` endpoint with `"type": "system"`.

## Custom Voices

Users can create custom cloned voices via `POST /api/v1/voices/clone`.
Voice embeddings are stored under the Go backend's `data/voices/` directory.
