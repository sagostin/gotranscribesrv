# API Reference

Base URL: `http://localhost:3000`

All endpoints except `/auth/*` require authentication via either:
- **JWT**: `Authorization: Bearer <access_token>`
- **API Key**: `X-API-Key: <key>`
- **Basic Auth**: `Authorization: Basic base64(apikey:<key>)` (Watson-compatible)
- **Deepgram Token**: `Authorization: Token <key>` (Deepgram-compatible)

---

## Health

### GET `/health`

Check server and sidecar connectivity. **No authentication required.**

**Response (200):**
```json
{
  "status": "ok",
  "sidecar": "connected",
  "models": {
    "asr": "parakeet-tdt-v3",
    "vad": "silero-vad",
    "diarizer": "sortformer",
    "tts": "pockettts",
    "kokoro": "kokoro-ane"
  },
  "config": {
    "synthesizeBackend": "pocket",
    "streamBackend": "pocket",
    "realtimeEngine": "eou-320"
  }
}
```

> **Note:** If a sidecar is unreachable, its models show as `"disconnected"`. The overall `status` is always `"ok"` as long as the Go server is running.

---

## Authentication

### POST `/api/v1/auth/register`

Create a new user account.

**Request:**
```json
{
  "email": "user@example.com",
  "password": "min8chars"
}
```

**Response (201):**
```json
{
  "id": "uuid",
  "email": "user@example.com",
  "tier": "free",
  "created_at": "2026-03-17T00:00:00Z"
}
```

**Errors:** `403` registration disabled (`REGISTRATION_ENABLED=false`), `409` email exists, `422` validation failed

---

### POST `/api/v1/auth/login`

Authenticate and receive tokens.

**Request:**
```json
{
  "email": "user@example.com",
  "password": "min8chars"
}
```

**Response (200):**
```json
{
  "access_token": "eyJ...",
  "refresh_token": "eyJ...",
  "expires_in": 900,
  "token_type": "Bearer"
}
```

The `refresh_token` is also set as an httpOnly cookie.

**Errors:** `401` invalid credentials

---

### POST `/api/v1/auth/refresh`

Exchange a refresh token for a new access token.

**Request:** Refresh token from cookie or body:
```json
{
  "refresh_token": "eyJ..."
}
```

**Response (200):**
```json
{
  "access_token": "eyJ...",
  "expires_in": 900
}
```

---

### POST `/api/v1/auth/logout`

Invalidate the current refresh token.

**Response (200):**
```json
{
  "message": "logged out"
}
```

---

## Speech-to-Text (ASR)

### POST `/api/v1/asr`

Transcribe an uploaded audio file.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `audio` | file | yes | Audio file (WAV, MP3, FLAC, OGG, M4A), max 100 MB |
| `diarize` | string | no | `"true"` to enable speaker diarization |
| `language` | string | no | Language code (default: `"en"`) |

**Response (200):**
```json
{
  "text": "Hello, how are you doing today?",
  "segments": [
    {
      "speaker": "A",
      "start": 0.0,
      "end": 1.8,
      "text": "Hello, how are you doing today?"
    }
  ],
  "words": [
    {"word": "Hello", "start": 0.0, "end": 0.4},
    {"word": "how", "start": 0.5, "end": 0.7},
    {"word": "are", "start": 0.7, "end": 0.85},
    {"word": "you", "start": 0.85, "end": 1.0},
    {"word": "doing", "start": 1.0, "end": 1.3},
    {"word": "today", "start": 1.4, "end": 1.8}
  ],
  "duration": 1.8,
  "processing_time_ms": 52,
  "model": "parakeet-tdt-0.6b-v3",
  "diarized": false
}
```

**With `diarize=true`, segments include speaker labels:**
```json
{
  "segments": [
    {"speaker": "SPEAKER_00", "start": 0.0, "end": 2.1, "text": "Hello, how are you?"},
    {"speaker": "SPEAKER_01", "start": 2.3, "end": 4.5, "text": "I'm doing well, thanks."}
  ]
}
```

**Errors:** `413` file too large (>100 MB), `415` unsupported format, `422` invalid params

---

### GET `/v1/models` (OpenAI-Compatible)

Lists the models this server advertises, following the OpenAI `/v1/models` schema. The list merges the static STT/TTS catalog (OpenAI-branded mock IDs so unmodified SDKs find a known model, plus the real on-device audio engines) with the **live LLM sidecar registry** — chat, embedding, and image entries carry extra fields (`kind`, `runtime`, `status`, `repo`). If the LLM sidecar is unreachable, the list degrades to the static audio catalog.

**Request:** No body. Auth required (Bearer token or API key).

| Query | Type | Description |
|-------|------|-------------|
| `owned_by` | string | Optional. Return only entries whose `owned_by` matches (e.g. `openai`, `nvidia`, `kyutai`, `local`). |

**Response — `200 OK`:**

```json
{
  "object": "list",
  "data": [
    { "id": "whisper-1", "object": "model", "created": 1677649200, "owned_by": "openai" },
    { "id": "parakeet-tdt-v3-coreml", "object": "model", "created": 1735689600, "owned_by": "nvidia" },
    { "id": "pocket-tts-1", "object": "model", "created": 1735603200, "owned_by": "kyutai" },
    {
      "id": "mistral-7b-int4", "object": "model", "created": 0, "owned_by": "local",
      "kind": "chat", "runtime": "standard", "status": "ready", "repo": "apple/mistral-coreml"
    },
    {
      "id": "all-minilm-l6-v2", "object": "model", "created": 0, "owned_by": "local",
      "kind": "embedding", "runtime": "standard", "status": "ready"
    }
  ]
}
```

> **Note:** The `model` field on `/v1/audio/transcriptions` is not validated against this list. Any value (including unknown IDs) is accepted; the real engine (Parakeet TDT v3 / CoreML) is always used for STT.

---

### POST `/v1/audio/transcriptions` (Whisper-Compatible)

Drop-in replacement for the OpenAI Whisper API. Allows existing tools and SDKs that target the OpenAI transcription endpoint to work with GoTranscribeSrv without code changes.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `file` | file | yes | Audio file (mp3, mp4, mpeg, mpga, m4a, wav, webm) |
| `model` | string | yes | Model name — accepted for compatibility. If name contains `"diarize"` (e.g. `"gpt-4o-transcribe-diarize"`), speaker diarization is enabled automatically. Always uses Parakeet TDT v3 via CoreML. |
| `language` | string | no | ISO-639-1 language code (default: `"en"`) |
| `prompt` | string | no | Hint text (accepted for compatibility, best-effort) |
| `response_format` | string | no | `"json"`, `"text"`, `"srt"`, `"vtt"`, `"verbose_json"`, `"diarized_json"` (default: `"json"`) |
| `stream` | string | no | `"true"` to enable SSE streaming (see below) |
| `temperature` | number | no | Accepted for compatibility (ignored) |
| `timestamp_granularities[]` | array | no | `"word"` and/or `"segment"` — controls which timestamp fields appear in `verbose_json`. Defaults to both if omitted. |

> **Verbose logging:** All request parameters (`model`, `language`, `temperature`, `prompt`, `stream`, `response_format`, `timestamp_granularities[]`, `diarize`) are logged at INFO level for debugging and auditing.

> **Diarization:** Enabled when `response_format=diarized_json` or when the `model` field contains `"diarize"`. Uses Sortformer (CoreML/ANE) for neural speaker detection.

**Response — `json` (default):**
```json
{
  "text": "Hello, how are you doing today?",
  "usage": {"type": "duration", "seconds": 2}
}
```

**Response — `diarized_json`:**

Speaker-labeled segments per the OpenAI `TranscriptionDiarized` schema:

```json
{
  "task": "transcribe",
  "duration": 6.5,
  "text": "Thanks for calling support.\nHi, I need help with my account.",
  "segments": [
    {
      "type": "transcript.text.segment",
      "id": "seg_001",
      "start": 0.0,
      "end": 2.8,
      "text": "Thanks for calling support.",
      "speaker": "A"
    },
    {
      "type": "transcript.text.segment",
      "id": "seg_002",
      "start": 3.1,
      "end": 6.5,
      "text": "Hi, I need help with my account.",
      "speaker": "B"
    }
  ],
  "usage": {"type": "duration", "seconds": 7}
}
```

**Response — `verbose_json`:**

Enriched segments with Whisper-spec fields, optional word-level timestamps, and **VAD speech detection segments** from Silero VAD:

```json
{
  "task": "transcribe",
  "language": "en",
  "duration": 1.8,
  "text": "Hello, how are you doing today?",
  "segments": [
    {
      "id": 0,
      "seek": 0,
      "start": 0.0,
      "end": 1.8,
      "text": "Hello, how are you doing today?",
      "tokens": [],
      "temperature": 0.0,
      "avg_logprob": -0.15,
      "compression_ratio": 1.2,
      "no_speech_prob": 0.02
    }
  ],
  "words": [
    {"word": "Hello", "start": 0.0, "end": 0.4},
    {"word": "how", "start": 0.5, "end": 0.7},
    {"word": "are", "start": 0.7, "end": 0.85},
    {"word": "you", "start": 0.85, "end": 1.0},
    {"word": "doing", "start": 1.0, "end": 1.3},
    {"word": "today", "start": 1.4, "end": 1.8}
  ],
  "vad_segments": [
    {"start": 0.0, "end": 1.85}
  ],
  "vad_processing_time_ms": 12,
  "usage": {"type": "duration", "seconds": 2}
}
```

| Field | Description |
|-------|-------------|
| `segments[].seek` | Seek offset (centiseconds from start) |
| `segments[].tokens` | Token IDs (empty — Parakeet TDT doesn't produce these) |
| `segments[].no_speech_prob` | Estimated probability of non-speech, derived from VAD overlap (0.0 = speech, 1.0 = silence) |
| `vad_segments` | Array of `{start, end}` from Silero VAD — detected speech regions |
| `vad_processing_time_ms` | VAD inference time in milliseconds |
| `usage` | Duration-based billing: `{type: "duration", seconds: N}` |

> **`timestamp_granularities[]` behavior:**
> - `["word"]` → response includes `words` only (no `segments`)
> - `["segment"]` → response includes `segments` only (no `words`)
> - `["word", "segment"]` or omitted → both included

> **Note:** `vad_segments` are included in `verbose_json` when the VAD sidecar is available. If unavailable, they are omitted gracefully.

**Response — `text`:**
```
Hello, how are you doing today?
```

**Response — `srt`:**
```
1
00:00:00,000 --> 00:00:01,800
Hello, how are you doing today?
```

**Response — `vtt`:**
```
WEBVTT

00:00:00.000 --> 00:00:01.800
Hello, how are you doing today?
```

#### Streaming Mode (`stream=true`)

When `stream=true` is set, the response uses **Server-Sent Events (SSE)** instead of returning a single JSON body. This is compatible with the OpenAI `gpt-4o-transcribe` streaming API.

**Headers:**
```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

**Events emitted:**

1. **`transcript.text.delta`** — one per transcript segment, sent incrementally:
```
event: transcript.text.delta
data: {"type":"transcript.text.delta","delta":"Hello, how are you doing today?","logprobs":null}
```

2. **`transcript.text.done`** — final event with the complete transcript and usage:
```
event: transcript.text.done
data: {"type":"transcript.text.done","text":"Hello, how are you doing today?","logprobs":null,"usage":{"type":"duration","seconds":2}}
```

3. **Terminal sentinel:**
```
data: [DONE]
```

**Example (curl):**
```bash
curl --no-buffer -X POST http://localhost:3000/v1/audio/transcriptions \
  -H "X-API-Key: YOUR_KEY" \
  -F "file=@audio.mp3" \
  -F "model=whisper-1" \
  -F "stream=true"
```

> **Compatibility note:** The `model` field is required by the OpenAI spec, so clients will send it (e.g., `"whisper-1"`). GoTranscribeSrv accepts any value but always uses Parakeet TDT v3 (CoreML/ANE). Fields like `temperature` are accepted without error but have no effect.

**Errors:** `413` file too large (>25 MB), `415` unsupported format

---

### WS `/ws/asr`

Real-time streaming transcription over WebSocket.

**Connection:** Upgrade with auth token or API key as query param:
```
ws://localhost:3000/ws/asr?token=<access_token>&language=en&diarize=false
ws://localhost:3000/ws/asr?token=gtx_live_abc123&language=en
```

**Client → Server:** Binary frames containing raw audio
- Format: PCM 16-bit signed, 16kHz, mono
- Recommended chunk size: 40ms–500ms of audio (1,280–16,000 bytes per frame)
- Send at real-time rate (1:1)

**Server → Client:** JSON text frames

Partial result (interim, may change):
```json
{
  "type": "partial",
  "text": "hello how are",
  "is_final": false
}
```

Final result (stable, utterance complete):
```json
{
  "type": "final",
  "text": "Hello, how are you doing today?",
  "start": 0.0,
  "end": 1.8,
  "words": [
    {"word": "Hello", "start": 0.0, "end": 0.4},
    {"word": "how", "start": 0.5, "end": 0.7}
  ],
  "is_final": true
}
```

Control messages:
```json
{"type": "ready"}          // Server is ready for audio
{"type": "error", "message": "..."}
{"type": "done"}           // Stream complete, closing
```

**Client control:** Send a JSON text frame to end the stream:
```json
{"action": "stop"}
```

---

### WS `/v1/listen` (Deepgram-Compatible)

Drop-in replacement for the Deepgram Live Transcription API. Allows existing Deepgram SDKs and tools to stream audio to GoTranscribeSrv without code changes.

**Connection:**
```
ws://localhost:3000/v1/listen?token=gtx_live_abc123&language=en
ws://localhost:3000/v1/listen?encoding=linear16&sample_rate=16000
```

**Authentication:**
- `Authorization: Token <api_key>` (Deepgram format)
- `Authorization: Bearer <api_key>` (OpenAI format)
- `?token=<api_key>` query param (browser/WebSocket clients)

**Query parameters:**

| Param | Type | Description |
|-------|------|-------------|
| `language` | string | BCP-47 language code (default: `"en"`) |
| `diarize` | string | `"true"` to enable speaker diarization |
| `interim_results` | string | `"true"` to receive partial transcripts (default: `"true"`) |
| `encoding` | string | Accepted for compatibility (ignored, expects PCM 16-bit) |
| `sample_rate` | string | Accepted for compatibility (ignored, expects 16kHz) |
| `model` | string | Accepted for compatibility (ignored, always uses Parakeet TDT v3 via CoreML) |
| `punctuate` | string | Accepted for compatibility (ignored) |
| `smart_format` | string | Accepted for compatibility (ignored) |
| `utterance_end_ms` | string | Accepted for compatibility (best-effort) |

**Client → Server:** Binary audio frames (PCM 16-bit, 16kHz, mono)

**Client control messages:**
```json
{"type": "KeepAlive"}    // Keep connection alive during silence
{"type": "CloseStream"}  // Gracefully end the session
```

**Server → Client:** JSON events

**`Metadata`** (sent once on connection open):
```json
{
  "type": "Metadata",
  "request_id": "52cc0efe-fa77-4aa7-b79c-0dda09de2f14",
  "created": "2026-03-18T00:00:00Z",
  "duration": 0,
  "channels": 1,
  "model_info": {
    "name": "parakeet-tdt-v3-coreml",
    "version": "2026-03-01",
    "arch": "parakeet-tdt"
  }
}
```

**`Results`** (interim transcript, `is_final: false`):
```json
{
  "type": "Results",
  "channel_index": [0, 1],
  "duration": 0.5,
  "start": 0.0,
  "is_final": false,
  "speech_final": false,
  "channel": {
    "alternatives": [{
      "transcript": "hello how",
      "confidence": 0.99,
      "words": []
    }]
  }
}
```

**`Results`** (final transcript, `is_final: true`):
```json
{
  "type": "Results",
  "channel_index": [0, 1],
  "duration": 1.8,
  "start": 0.0,
  "is_final": true,
  "speech_final": true,
  "channel": {
    "alternatives": [{
      "transcript": "Hello, how are you doing today?",
      "confidence": 0.99,
      "words": [
        {"word": "Hello", "start": 0.0, "end": 0.4, "confidence": 0.99, "punctuated_word": "Hello"},
        {"word": "how", "start": 0.5, "end": 0.7, "confidence": 0.99, "punctuated_word": "how"}
      ]
    }]
  }
}
```

> **Compatibility note:** Fields like `encoding`, `sample_rate`, `model`, `punctuate`, and `smart_format` are accepted without error but ignored. GoTranscribeSrv always expects PCM 16-bit 16kHz mono audio and uses Parakeet TDT v3 (CoreML/ANE).

---

### POST `/v1/recognize` (Watson-Compatible)

Drop-in replacement for the IBM Watson Speech-to-Text API. Allows existing Watson SDKs and tools to transcribe audio via GoTranscribeSrv without code changes.

**Request:** Raw audio in the request body (not multipart).

**Authentication:**
- `Authorization: Basic base64(apikey:<api_key>)` (Watson format)
- `Authorization: Bearer <api_key>` (OpenAI format)
- `X-API-Key: <key>` (GoTranscribeSrv format)

**Headers:**

| Header | Required | Description |
|--------|----------|-------------|
| `Content-Type` | yes | Audio format: `audio/wav`, `audio/flac`, `audio/mp3`, `audio/ogg`, `audio/webm`, `audio/l16`, `application/octet-stream` |

**Query parameters:**

| Param | Type | Description |
|-------|------|-------------|
| `model` | string | Accepted for compatibility (ignored, always uses Parakeet TDT v3) |
| `timestamps` | string | `"true"` to include word-level timestamps |
| `word_confidence` | string | `"true"` to include per-word confidence scores |
| `speaker_labels` | string | `"true"` to enable speaker diarization |
| `max_alternatives` | string | Accepted for compatibility (ignored, always returns 1) |
| `profanity_filter` | string | Accepted for compatibility (ignored) |
| `smart_formatting` | string | Accepted for compatibility (ignored) |
| `inactivity_timeout` | string | Accepted for compatibility (ignored) |
| `language` | string | Language code (default: `"en"`) |

**Response (200):**
```json
{
  "results": [
    {
      "alternatives": [
        {
          "transcript": "Hello, how are you doing today?",
          "confidence": 0.99
        }
      ],
      "final": true
    }
  ],
  "result_index": 0
}
```

**With `timestamps=true`:**
```json
{
  "results": [{
    "alternatives": [{
      "transcript": "Hello, how are you doing today?",
      "confidence": 0.99,
      "timestamps": [
        ["Hello", 0.0, 0.4],
        ["how", 0.5, 0.7],
        ["are", 0.7, 0.85],
        ["you", 0.85, 1.0],
        ["doing", 1.0, 1.3],
        ["today", 1.4, 1.8]
      ]
    }],
    "final": true
  }],
  "result_index": 0
}
```

**With `speaker_labels=true`:**
```json
{
  "results": [...],
  "result_index": 0,
  "speaker_labels": [
    {"from": 0.0, "to": 0.4, "speaker": 0, "confidence": 0.99, "final": true},
    {"from": 0.5, "to": 0.7, "speaker": 0, "confidence": 0.99, "final": true},
    {"from": 2.3, "to": 2.8, "speaker": 1, "confidence": 0.99, "final": true}
  ]
}
```

**Example (curl):**
```bash
curl -X POST "http://localhost:3000/v1/recognize?timestamps=true" \
  -H "X-API-Key: YOUR_KEY" \
  -H "Content-Type: audio/mp3" \
  --data-binary @audio.mp3
```

> **Compatibility note:** The `model` and `max_alternatives` parameters are accepted without error but ignored. GoTranscribeSrv always uses Parakeet TDT v3 (CoreML/ANE) and returns a single alternative.

**Errors:** `400` no audio data, `413` file too large (>100 MB), `503` sidecar unavailable

---

### WS `/v1/recognize` (Watson-Compatible Streaming)

Drop-in replacement for the IBM Watson Speech-to-Text WebSocket API. Allows existing Watson SDKs to stream audio for real-time transcription.

**Connection:**
```
ws://localhost:3000/v1/recognize?token=gtx_live_abc123
ws://localhost:3000/v1/recognize?token=<access_token>&timestamps=true
```

**Authentication:**
- `Authorization: Basic base64(apikey:<api_key>)` (Watson format)
- `Authorization: Bearer <api_key>` header
- `?token=<api_key>` query param

**Query parameters:**

| Param | Type | Description |
|-------|------|-------------|
| `timestamps` | string | `"true"` to include word-level timestamps (default: `"false"`) |
| `word_confidence` | string | `"true"` to include per-word confidence (default: `"false"`) |
| `speaker_labels` | string | `"true"` to enable speaker diarization (default: `"false"`) |
| `interim_results` | string | `"true"` to receive partial transcripts (default: `"true"`) |
| `language` | string | Language code (default: `"en"`) |
| `encoding` | string | Accepted for compatibility (ignored) |
| `sample_rate` | string | Accepted for compatibility (ignored) |

**Server → Client:** JSON events

**`state` message** (sent on connection open and after processing completes):
```json
{"state": "listening"}
```

**Interim result** (`final: false`):
```json
{
  "results": [{
    "alternatives": [{"transcript": "hello how", "confidence": 0}],
    "final": false
  }],
  "result_index": 0
}
```

**Final result** (`final: true`):
```json
{
  "results": [{
    "alternatives": [{
      "transcript": "Hello, how are you doing today?",
      "confidence": 0.99,
      "timestamps": [["Hello", 0.0, 0.4], ["how", 0.5, 0.7]]
    }],
    "final": true
  }],
  "result_index": 0
}
```

**Client → Server:**

Start message (optional, parameters can also be set via query params):
```json
{
  "action": "start",
  "content-type": "audio/l16;rate=16000",
  "interim_results": true,
  "timestamps": true,
  "speaker_labels": true
}
```

Binary audio frames (PCM 16-bit, 16kHz, mono).

Stop message:
```json
{"action": "stop"}
```

> **Compatibility note:** GoTranscribeSrv always expects PCM 16-bit 16kHz mono audio and uses Parakeet TDT v3 (CoreML/ANE). The `content-type` field in the start message is accepted but ignored.

---

## Real-Time Streaming ASR (Voice-Agent Grade)

The endpoints in this section use **true streaming ASR** (cache-aware encoder states, partial hypotheses, turn-taking events) — distinct from the buffered `/ws/asr`, `/v1/listen`, and `/v1/recognize` WS routes above, which re-transcribe the full audio buffer every ~2 seconds with the offline TDT model and are intended for batch-style streaming.

Three things change when you switch to true streaming:

1. **Incremental partials** — the server emits `partial` events as the user speaks (typically every 160–320 ms), each containing the accumulated-so-far hypothesis, instead of periodic re-transcriptions of the entire buffer.
2. **Built-in turn-taking** — the Parakeet EOU engine emits an `end_of_turn` event (with the finalized transcript) when it detects end-of-utterance; a streaming Silero VAD also emits `speech_started` / `speech_stopped` events for client-side turn-taking.
3. **O(n) compute** — inference cost is linear in audio duration rather than quadratic in session length, so longer agent calls don't exponentially slow down.

### Streaming engines (`?engine=`)

All ten variants are wired in the sidecar's `StreamingModelVariant`. Pass via `?engine=<id>` on any of the realtime endpoints (`/stream/realtime`, `/v2/listen`, `/v1/realtime`).

| Engine               | Architecture                 | Latency  | Multilingual | Notes |
|----------------------|------------------------------|----------|--------------|-------|
| `eou-160`            | Parakeet EOU 120M, 160ms chunk | ~160 ms  | English only | Lowest-latency variant. Less accurate per chunk but snappy. |
| `eou-320` (default)  | Parakeet EOU 120M, 320ms chunk | ~320 ms  | English only | Best for live voice agents — low latency + **built-in end-of-utterance detection** (no client-side turn-taking logic needed). |
| `eou-1280`           | Parakeet EOU 120M, 1.28s chunk | ~1.28 s  | English only | Highest throughput; weak for low-latency agents. |
| `nemotron-560`       | Nemotron Speech Stream. 0.6B, 560ms  | ~560 ms  | English      | Higher accuracy than EOU; **no built-in EOU** — uses streaming VAD for turn boundaries. |
| `nemotron-1120`      | Nemotron 0.6B, 1.12s chunk          | ~1.12 s  | English      | Balanced Nemotron. |
| `nemotron-2240`      | Nemotron 0.6B, 2.24s chunk          | ~2.24 s  | English      | Highest Nemotron accuracy tier. |
| `unified-320`        | Parakeet Unified 0.6B, 320ms chunk   | ~320 ms  | Multilingual (25 EU languages) | TDT v3 quality in streaming form. **Best pick for multilingual live agents.** |
| `unified-640`        | Parakeet Unified 0.6B, 640ms chunk   | ~640 ms  | Multilingual | Balanced multilingual. |
| `unified-1120`       | Parakeet Unified 0.6B, 1.12s chunk   | ~1.12 s  | Multilingual | Best accuracy among streaming unified variants. |
| `unified-2080`       | Parakeet Unified 0.6B, 2.08s chunk   | ~2.08 s  | Multilingual | Largest chunk — weakest for low-latency agents. |

> **Default:** the sidecar reads `SIDECAR_REALTIME_ENGINE` at startup; absent that, `eou-320` is used. Override per-session with `?engine=…`. Pass-through the prefix: any unknown `eou-*` / `nemotron-*` / `unified-*` is forwarded verbatim to the sidecar.

#### Model name → engine mapping (Go realtime proxies)

The Go proxies (`/v2/listen` Deepgram-compat, `/v1/realtime` OpenAI-compat) translate their protocol's `model` field into a sidecar `?engine=` value. Unknown explicit engine IDs (`eou-160`, `nemotron-2240`, `unified-2080`, …) pass through unchanged.

| OpenAI Realtime `model`                  | Sidecar engine     | Deepgram `model`         | Sidecar engine   |
|------------------------------------------|--------------------|---------------------------|------------------|
| `gpt-4o-realtime-preview`, `gpt-4o-realtime` | `eou-320`          | `nova-3`, `nova-2`, `nova-2-eou` | `eou-320`        |
| `gpt-4o-mini-realtime-preview`           | `nemotron-560`     | `nova-3-unified`, `nova-2-unified` | `unified-320`    |
| `nova-3`, `parakeet-unified-320`         | `unified-320`      | `2-nova`, `nova-2-nemotron`      | `nemotron-560`   |
| anything containing `unified`            | `unified-320`      | `eou-*`, `nemotron-*`, `unified-*` (pass-through) | (same id)        |
| anything containing `nemotron`           | `nemotron-560`     | anything else              | `eou-320`        |
| anything containing `eou-`               | (pass-through)     |                           |                  |
| empty / unknown                          | server default (`eou-320`) | empty / unknown        | `eou-320`        |

### WS `/v1/realtime` (OpenAI Realtime-Compatible)

OpenAI Realtime-style WebSocket proxy. This endpoint implements the **input-transcription half** of the Realtime API — LLM/TTS stays in your existing service; we only emit user-side transcription events. Wire-compatible with the OpenAI Python SDK's `RealtimeClient`.

> **Speech-to-speech + tools:** connect with `?model=gpt-realtime` (any
> `gpt-realtime*` name) and `REALTIME_S2S_ENABLED=true` to get a full
> speech-to-speech session — ASR → LLM → TTS with barge-in and client-side
> function calling. Spec: [docs/realtime.md](realtime.md).

**Connect:** `WS /v1/realtime?encoding=linear16&sample_rate=16000`

**Client → server events handled:**

| Event                          | Purpose |
|--------------------------------|---------|
| `session.update`               | Configure session. `session.model` selects engine (e.g. `"gpt-4o-realtime-preview"` → `eou-320`, `"nova-3"` → `unified-320`). |
| `input_audio_buffer.append`    | Base64-encoded PCM16 frame. Forwarded verbatim to the streaming engine. |
| `input_audio_buffer.commit`    | Ack-only — the streaming engine is auto-incremental. |
| `input_audio_buffer.clear`     | No-op (logged only). |

**Server → client events emitted:**

| Event                                                    | When |
|----------------------------------------------------------|------|
| `session.created`                                        | On connect. Echoes resolved model. |
| `session.updated`                                        | After each `session.update`. |
| `input_audio_buffer.speech_started`                      | Streaming VAD detects voice onset. |
| `input_audio_buffer.speech_stopped`                      | Streaming VAD detects voice offset. |
| `conversation.item.input_audio_transcription.delta`      | Each incremental partial hypothesis. |
| `conversation.item.input_audio_transcription.completed`  | End-of-turn final transcript (EOU / VAD speech_end). |
| `input_audio_buffer.committed`                           | Turn-end marker. New `item_id` follows. |
| `error`                                                  | Sidecar / proxy error. |

### WS `/v2/listen` (Deepgram-Compatible, Real-Time)

Deepgram Nova-compatible WebSocket proxy using the real-time engine. Distinct from `/v1/listen` (which proxies the buffered `/stream` route), so existing Deepgram clients on `/v1/listen` are untouched.

**Connect:** `WS /v2/listen?encoding=linear16&sample_rate=16000&model=nova-3&interim_results=true`

| Query param       | Default | Description |
|-------------------|---------|-------------|
| `model`           | `nova-3` | Deepgram model name — mapped to a streaming engine: `nova-3`/`nova-2` → `eou-320`, `nova-3-unified` → `unified-320`, `2-nova` → `nemotron-560`. Pass an explicit engine ID (`eou-160`, `nemotron-1120`, …) to bypass the mapping. |
| `interim_results` | `true`  | Emit interim `Results` events with `is_final=false`. Set `false` to receive only finals. |
| `encoding`        | `linear16` | `linear16` \| `mulaw` \| `alaw`. |
| `sample_rate`     | `16000` | 8000 is upsampled 2×. |
| `itn`             | `true`  | Apply inverse text normalization. |
| `vad`             | `true`  | Emit `SpeechStarted` events from streaming VAD. |

**Server → client events emitted** follow the Deepgram Nova wire protocol: `Metadata` on connect, `SpeechStarted`, `Results` (interim / final / `speech_final`), `UtteranceEnd`, `Error`.

### Sidecar endpoints (direct, no Go proxy)

For clients that don't need JWT auth or quota tracking, the audio sidecar exposes the underlying streaming endpoints directly:

- `WS /stream/realtime` — native JSON+PCM protocol (see below). Engine selectable via `?engine=`.
- `POST /synthesize?backend=kokoro` — Kokoro TTS, no LLM.
- `POST /synthesize/stream` — PocketTTS chunked streaming TTS (raw Int16 L16 frames, 24 kHz mono, transfer-encoding chunked).

#### Native `/stream/realtime` event protocol

**Server → client JSON events:**

| Event            | Payload                                              |
|------------------|------------------------------------------------------|
| `ready`          | `{"engine":"eou-320","display_name":"Parakeet EOU 120M (320ms)"}` |
| `speech_started` | `{"time":3.42}`                                       |
| `speech_stopped` | `{"time":7.07}`                                       |
| `partial`        | `{"text":"welcome if you","is_final":false}`         |
| `end_of_turn`    | Marker for turn boundary.                             |
| `final`          | `{"text":"welcome if you are calling…","is_final":true,"speech_final":true,"itn_applied":true}` |
| `done`           | Stream closing.                                       |
| `error`          | `{"message":"…"}`                                     |

**Client → server:** binary PCM frames (encoding determined by query params: `linear16`/`mulaw`/`alaw`, `sample_rate=16000` or `8000`), plus JSON control: `{"action":"stop"}` or `{"type":"CloseStream"}`.

---

## Text-to-Speech (TTS) — PocketTTS & Kokoro

Two TTS backends ship with the sidecar. Pick the right one per call site; the table below summarizes what changes:

| Backend   | Quality              | Time-to-first-audio         | Voices                                                | Streaming | Voice cloning | Multilingual |
|-----------|----------------------|------------------------------|-------------------------------------------------------|-----------|---------------|--------------|
| `pocket`  | Good                 | **~80 ms first chunk** (streaming) | 17 built-in (alba, jane, charles, …) + custom clones | **Yes** (`/synthesize/stream`) | **Yes** (`voice_id` / `voice_ref`) | English      |
| `kokoro`  | Best quality-per-watt | Full utterance (no streaming API in 0.15.5) | 13 (`af_heart`, `bf_emma`, `zf_001`, … — EN/Mandarin/JP) | **No** (returns 501 on `/synthesize/stream`) | **No** (returns 422 if cloning requested) | **Yes** |

**Default-backend matrix:**

| Endpoint                          | Default backend | Configurable via                          | Notes |
|-----------------------------------|-----------------|--------------------------------------------|-------|
| `POST /api/v1/tts`                | `pocket`        | Sidecar env `SIDECAR_TTS_DEFAULT_BACKEND`  | Existing behavior preserved. `voice_id`/`voice_ref` keep working. |
| `POST /synthesize?backend=…`      | `pocket`        | `?backend=` query or `SIDECAR_TTS_DEFAULT_BACKEND` | Voice cloning → 422 unless `?backend=pocket`. |
| `POST /synthesize/stream`         | `pocket` (locked) | `?backend=` query (only `pocket` accepted) | Kokoro has no streaming API in FluidAudio 0.15.5. |
| `POST /v1/audio/speech` (Go)      | `kokoro`        | Go env `TTS_DEFAULT_BACKEND` (default `kokoro`); explicit `model=tts-1` overrides | New endpoint — no back-compat risk. |

For voice agents: **`/synthesize/stream`** (PocketTTS) is the fast first-chunk path; **`/v1/audio/speech?model=kokoro`** (or `/synthesize?backend=kokoro`) is the higher-quality full-utterance path. Many production voice agents use both — PocketTTS for snappy conversational replies, Kokoro for greetings / pre-recorded-style prompts.

### POST `/api/v1/tts`

Synthesize speech using PocketTTS with voice cloning support. 24 kHz output. **Back-compat preserved**: defaults to `pocket` regardless of `SIDECAR_TTS_DEFAULT_BACKEND`, so existing callers keep working.

**Request:**
```json
{
  "text": "Hello, welcome to GoTranscribeSrv.",
  "voice": "jane",
  "speed": 1.0,
  "format": "wav"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `text` | string | yes | Text to synthesize (max 5,000 chars) |
| `voice` | string | no | System voice name — e.g. `"jane"`, `"charles"` (default: `"default"`) |
| `voice_id` | string | no | UUID of a stored custom voice (from `/api/v1/voices/clone`) |
| `voice_ref` | string | no | Base64-encoded audio reference for one-shot voice cloning (5–15 sec) |
| `speed` | number | no | Playback speed 0.5–2.0 (default: 1.0) |
| `format` | string | no | `"wav"`, `"opus"`, `"mp3"` (default: `"wav"`) |

> **Priority:** `voice_id` > `voice_ref` > `voice`. If `voice_id` is provided, the stored voice embedding is used (fastest, no re-cloning). If `voice_ref` is provided, one-shot cloning is performed. Otherwise the named system voice is used.

**Response (200):** Audio binary stream at 24 kHz.

```
Content-Type: audio/wav
X-Audio-Sample-Rate: 24000
```

### POST `/v1/audio/speech` (OpenAI-Compatible)

OpenAI TTS-compatible endpoint. Maps OpenAI's `model` / `voice` names onto our backends. **Defaults to Kokoro** (via Go `TTS_DEFAULT_BACKEND` env, default `kokoro`) so voice-agent clients that send OpenAI-shaped requests get the higher-quality path automatically.

**Request:**
```json
{
  "model": "tts-1",
  "voice": "alloy",
  "input": "Hello, welcome to GoTranscribeSrv.",
  "response_format": "wav",
  "speed": 1.0
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | no | `tts-1` / `tts-1-hd` / `gpt-4o-mini-tts` → PocketTTS; `kokoro` / `kokoro-82m` → Kokoro. Unrecognized / omitted models fall back to Go `TTS_DEFAULT_BACKEND` (default `kokoro`). (default: empty → server default) |
| `voice` | string | no | OpenAI voice name (`alloy`, `ash`, `coral`, …) — mapped to PocketTTS when backend is pocket; pass Kokoro-native IDs (`af_heart`, `zf_001`, …) when backend is kokoro (default: `"alloy"`) |
| `input` | string | yes | Text to synthesize (max 5,000 chars) |
| `response_format` | string | no | `"wav"` (24 kHz mono PCM) or `"pcm"` (raw 24 kHz Int16 LE, WAV header stripped). `mp3`/`opus`/`flac` return 501 — not yet wired up. (default: `"wav"`) |
| `speed` | number | no | Playback speed 0.25–4.0 (default: 1.0) |

**OpenAI voice → PocketTTS mapping:** alloy→jane, ash→charles, ballad→mary, coral→eve, echo→alba, sage→george, shimmer→anna, verse→michael, onyx→paul, nova→vera, fable→jean. **Kokoro voices pass through** unchanged (`af_heart`, `bf_emma`, `zf_001`, …).

**Response (200):** Audio binary stream.

```
Content-Type: audio/wav                          # or audio/L16 for pcm
X-Audio-Sample-Rate: 24000
X-TTS-Backend: pocket|kokoro                     # resolved backend
X-TTS-Model: <model from request>                # echoed
```

> The `/api/v1/tts` endpoint above is unchanged in behavior — this is purely additive. Use `/v1/audio/speech` for OpenAI client compatibility; it defaults to Kokoro (Go `TTS_DEFAULT_BACKEND=kokoro`).

### POST `/synthesize/stream` (sidecar, chunked audio)

Server-Sent chunked L16 audio, 80 ms frames @ 24 kHz mono, streamed from PocketTTS. The only sidecar endpoint with low-latency first-chunk delivery. **Kokoro not supported** — `?backend=kokoro` returns 501.

```
POST /synthesize/stream?backend=pocket     # backend param is optional, only "pocket" is accepted
Content-Type: application/json

{ "text": "streaming test", "voice": "alba" }
```

```
Content-Type: audio/L16; rate=24000; channels=1
Transfer-Encoding: chunked
X-TTS-Backend: pocket
```

### Configuration env vars

| Env var                          | Default | Where read    | Effect |
|----------------------------------|---------|----------------|--------|
| `SIDECAR_TTS_DEFAULT_BACKEND`    | `pocket` | Sidecar        | `/synthesize` fallback when `?backend=` isn't sent. |
| `SIDECAR_TTS_STREAM_BACKEND`     | `pocket` | Sidecar        | `/synthesize/stream` fallback. Anything other than `pocket` is rejected with a startup warning (Kokoro streaming is unsupported upstream). |
| `TTS_DEFAULT_BACKEND` (Go)       | `kokoro` | Go server      | `/v1/audio/speech` fallback when `model` is omitted or unrecognized. Set to `pocket` to preserve legacy behavior on this endpoint. |

`GET /health` on the sidecar surfaces the resolved values under `config`:

```json
{
  "status": "ok",
  "models": { "asr": "loaded", "vad": "loaded", "diarizer": "loaded", "tts": "loaded", "kokoro": "loaded" },
  "config": { "synthesizeBackend": "pocket", "streamBackend": "pocket", "realtimeEngine": "eou-320" }
}
```

---

### Voice Management

#### POST `/api/v1/voices/clone`

Upload an audio recording to create a stored cloned voice. The sidecar extracts a voice embedding that can be reused in TTS synthesis via `voice_id`.

**Request:** Multipart form data

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `audio` | file | yes | Audio file (WAV, MP3, etc.) — max 10 MB. 5–15 seconds of clear, single-speaker speech recommended. |
| `name` | string | yes | Display name for the voice (must be unique per user) |
| `description` | string | no | Optional description |

**Response (201):**
```json
{
  "id": "uuid",
  "name": "my-voice",
  "description": "My custom voice",
  "type": "custom",
  "size_bytes": 48000,
  "created_at": "2026-03-20T14:00:00Z"
}
```

**Errors:** `409` voice name already exists, `422` missing name/audio or file too large, `502` sidecar unavailable

---

#### GET `/api/v1/voices`

List the current user's custom (cloned) voices and all system (PocketTTS built-in) voices.

**Response (200):**
```json
{
  "custom": [
    {
      "id": "uuid",
      "name": "my-voice",
      "description": "My custom voice",
      "type": "custom",
      "size_bytes": 48000,
      "created_at": "2026-03-20T14:00:00Z"
    }
  ],
  "system": [
    {"name": "default", "description": "PocketTTS default voice", "type": "system"},
    {"name": "Jane", "description": "Female, conversational", "type": "system"},
    {"name": "Alba", "description": "Male, reading & conversational", "type": "system"},
    {"name": "Charles", "description": "Male, conversational", "type": "system"},
    {"name": "Anna", "description": "Female, conversational", "type": "system"},
    {"name": "Eve", "description": "Female, conversational", "type": "system"},
    {"name": "George", "description": "Male, conversational", "type": "system"},
    {"name": "Paul", "description": "Male, conversational", "type": "system"},
    {"name": "Mary", "description": "Female, conversational", "type": "system"},
    {"name": "Michael", "description": "Male, conversational", "type": "system"},
    {"name": "Vera", "description": "Female, conversational", "type": "system"},
    {"name": "Jean", "description": "Male, conversational", "type": "system"},
    {"name": "Eponine", "description": "Female, reading", "type": "system"},
    {"name": "Fantine", "description": "Female, reading", "type": "system"},
    {"name": "Marius", "description": "Male", "type": "system"},
    {"name": "Cosette", "description": "Female", "type": "system"},
    {"name": "Azelma", "description": "Female, reading", "type": "system"}
  ]
}
```

---

#### GET `/api/v1/voices/:id`

Get details for a specific custom voice.

**Response (200):**
```json
{
  "id": "uuid",
  "name": "my-voice",
  "description": "My custom voice",
  "type": "custom",
  "size_bytes": 48000,
  "created_at": "2026-03-20T14:00:00Z"
}
```

**Errors:** `400` invalid ID, `404` voice not found

---

#### DELETE `/api/v1/voices/:id`

Delete a custom voice and remove the stored embedding.

**Response (200):**
```json
{"message": "Voice deleted"}
```

**Errors:** `400` invalid ID, `404` voice not found
```

---

## Speaker Diarization

Speaker diarization is available as part of the ASR endpoint by setting `diarize=true`.
See [`POST /api/v1/asr`](#post-apiv1asr) above.

Diarization is handled by the audio sidecar using the Sortformer model (end-to-end neural, up to 4 speakers) running on CoreML/ANE.

> **Note:** Standalone speaker detection (without transcription) is not supported.
> The Sortformer diarizer requires transcript word/segment timestamps to produce
> meaningful per-speaker results.

---

## LLM Gateway (OpenAI + Anthropic-Compatible)

The Go server proxies the LLM sidecar's native API surface, adding auth (JWT or API key), per-tier rate limiting, and **per-model token usage tracking**. Request and response bodies pass through verbatim — point unmodified OpenAI or Anthropic SDKs at this server. Enabled via `ENABLE_LLM=true` + `LLM_SIDECAR_URL` (default `http://127.0.0.1:8080`).

| Route | Dialect | Streaming |
|-------|---------|-----------|
| `POST /v1/chat/completions` | OpenAI chat | SSE (`stream: true`) |
| `POST /v1/completions` | OpenAI legacy | SSE |
| `POST /v1/embeddings` | OpenAI | — |
| `POST /v1/images/generations` | OpenAI | — |
| `POST /v1/messages` | Anthropic Messages | SSE (`stream: true`) |

**Auth:** standard gateway auth — `Authorization: Bearer <jwt-or-gtx_key>` or `X-API-Key: gtx_...`. Anthropic SDK users pass their `gtx_...` key as `api_key` (the SDK's `x-api-key` header is accepted).

**Usage tracking:** every request writes a `usage_logs` row whose `metadata` JSONB carries `{"model", "prompt_tokens", "completion_tokens", "total_tokens", "stream"}`. Token counts are extracted from the response (non-streaming) or tee'd from the terminal SSE frames (streaming — OpenAI finish-chunk `usage`; Anthropic `message_start`/`message_delta`). Aggregates appear in `/api/v1/usage/summary` under `by_endpoint` (`llm_chat`, `llm_completion`, `llm_embeddings`, `llm_images`, `llm_messages`) and per-model under `by_model`.

**Examples:**

```bash
# OpenAI SDK-compatible chat (streaming)
curl -N http://localhost:3000/v1/chat/completions \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"model":"mistral-7b-int4","stream":true,"messages":[{"role":"user","content":"Hi"}]}'

# Anthropic SDK-compatible messages
curl http://localhost:3000/v1/messages \
  -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01" -H "Content-Type: application/json" \
  -d '{"model":"mistral-7b-int4","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:3000/v1", api_key="gtx_live_...")
print(client.chat.completions.create(model="mistral-7b-int4",
      messages=[{"role": "user", "content": "Hi"}]).choices[0].message.content)

import anthropic
acl = anthropic.Anthropic(base_url="http://localhost:3000", api_key="gtx_live_...")
print(acl.messages.create(model="mistral-7b-int4", max_tokens=50,
      messages=[{"role": "user", "content": "Hi"}]).content[0].text)
```

**Error passthrough:** upstream errors keep the sidecar's OpenAI-style envelope (`{"error": {"message", "type", "code"}}`) and status code. Gateway-side failures use the same shape (e.g. `502 {"error": {"type": "server_error", "message": "LLM service unavailable"}}`).

**Admin model management** (admin users only):

| Route | Proxies to sidecar |
|-------|--------------------|
| `GET /api/v1/admin/llm/models/:id/status` | `GET /models/:id/status` |
| `POST /api/v1/admin/llm/models/:id/download` | `POST /models/:id/download` |
| `POST /api/v1/admin/llm/models/:id/load` | `POST /models/:id/load` |
| `POST /api/v1/admin/llm/models/:id/unload` | `POST /models/:id/unload` |

> **Known gaps:** legacy `/v1/completions` *streaming* and `coreml-llm` runtime responses don't carry usage on the wire — those rows log zero tokens.

---

## Usage Tracking

### GET `/api/v1/usage/summary`

Aggregated usage stats for the authenticated user.

**Query params:**

| Param | Type | Description |
|-------|------|-------------|
| `period` | string | `"day"`, `"week"`, `"month"` (default: `"month"`) |

**Response (200):**
```json
{
  "period": "month",
  "total_requests": 1847,
  "total_audio_duration_sec": 36420,
  "total_processing_time_sec": 485,
  "by_endpoint": {
    "asr": {"requests": 1200, "audio_duration_sec": 28000},
    "asr_stream": {"requests": 600, "audio_duration_sec": 7200},
    "tts": {"requests": 47, "audio_duration_sec": 1220},
    "llm_chat": {"requests": 91, "prompt_tokens": 40211, "completion_tokens": 8830}
  },
  "by_model": {
    "mistral-7b-int4": {"requests": 80, "prompt_tokens": 35800, "completion_tokens": 7900, "total_tokens": 43700},
    "all-minilm-l6-v2": {"requests": 11, "prompt_tokens": 4411, "completion_tokens": 0, "total_tokens": 4411}
  },
  "by_key": [
    {
      "key_id": "uuid",
      "label": "production-app",
      "total_requests": 1500,
      "total_audio_duration_sec": 30000,
      "total_processing_time_sec": 400,
      "by_endpoint": {
        "asr": {"requests": 1000, "audio_duration_sec": 24000},
        "asr_stream": {"requests": 500, "audio_duration_sec": 6000}
      }
    },
    {
      "key_id": "uuid",
      "label": "dev-testing",
      "total_requests": 247,
      "total_audio_duration_sec": 5200,
      "total_processing_time_sec": 70,
      "by_endpoint": {
        "asr": {"requests": 200, "audio_duration_sec": 4000}
      }
    }
  ]
}
```

> **Note:** Requests made via JWT (session auth) have no `api_key_id` and are excluded from `by_key` but included in the totals.

---

### GET `/api/v1/usage/history`

Paginated usage log entries.

**Query params:**

| Param | Type | Description |
|-------|------|-------------|
| `page` | int | Page number (default: 1) |
| `limit` | int | Items per page, max 100 (default: 20) |
| `endpoint` | string | Filter by endpoint |
| `key_id` | uuid | Filter by API key ID |

**Response (200):**
```json
{
  "items": [
    {
      "id": "uuid",
      "api_key_id": "uuid",
      "endpoint": "asr",
      "audio_duration_ms": 60000,
      "processing_time_ms": 550,
      "diarized": true,
      "created_at": "2026-03-17T12:00:00Z"
    }
  ],
  "total": 1847,
  "page": 1,
  "pages": 93
}
```

---

### GET `/api/v1/usage/keys/:id`

Usage summary for a specific API key owned by the authenticated user.

**Query params:**

| Param | Type | Description |
|-------|------|-------------|
| `period` | string | `"day"`, `"week"`, `"month"` (default: `"month"`) |

**Response (200):**
```json
{
  "period": "month",
  "key": {
    "key_id": "uuid",
    "label": "production-app",
    "total_requests": 1500,
    "total_audio_duration_sec": 30000,
    "total_processing_time_sec": 400,
    "by_endpoint": {
      "asr": {"requests": 1000, "audio_duration_sec": 24000}
    }
  }
}
```

**Errors:** `400` invalid key ID, `404` key not found or not owned by user

---

### GET `/api/v1/usage/me`

Usage stats for the API key used to authenticate the current request. This allows API key holders to check their own usage without knowing the key UUID. **Requires API key authentication** — returns `400` if authenticated via JWT.

**Query params:**

| Param | Type | Description |
|-------|------|-------------|
| `period` | string | `"day"`, `"week"`, `"month"` (default: `"month"`) |

**Example:**
```bash
curl -H "X-API-Key: gtx_live_abc123..." http://localhost:3000/api/v1/usage/me
```

**Response (200):**
```json
{
  "period": "month",
  "key": {
    "key_id": "uuid",
    "label": "production-app",
    "total_requests": 1500,
    "total_audio_duration_sec": 30000,
    "total_processing_time_sec": 400,
    "by_endpoint": {
      "asr": {"requests": 1000, "audio_duration_sec": 24000}
    }
  }
}
```

**Errors:** `400` not authenticated via API key

---

## API Keys

### POST `/api/v1/keys`

Generate a new API key.

**Request:**
```json
{
  "label": "production-app",
  "scopes": ["asr", "tts"]
}
```

**Response (201):**
```json
{
  "id": "uuid",
  "key": "gtx_live_abc123...",
  "label": "production-app",
  "scopes": ["asr", "tts"],
  "created_at": "2026-03-17T00:00:00Z"
}
```

> **Note:** The full key is only shown once at creation.

---

### GET `/api/v1/keys`

List all API keys for the current user.

---

### DELETE `/api/v1/keys/:id`

Revoke an API key.

---

## Admin API (Enterprise Tier Only)

> All `/api/v1/admin/*` endpoints require **enterprise tier** authentication.
> Returns `403 FORBIDDEN` for non-enterprise users.

### User Management

#### GET `/api/v1/admin/users`

List all users (paginated).

**Query params:** `page` (default: 1), `limit` (default: 20, max: 100)

**Response (200):**
```json
{
  "items": [
    {"id": "uuid", "email": "customer@co.com", "tier": "pro", "created_at": "..."}
  ],
  "total": 42,
  "page": 1,
  "pages": 3
}
```

#### POST `/api/v1/admin/users`

Create a new user (customer).

**Request:**
```json
{
  "email": "customer@company.com",
  "password": "securepass123",
  "tier": "pro"
}
```

**Response (201):**
```json
{"id": "uuid", "email": "customer@company.com", "tier": "pro", "created_at": "..."}
```

#### GET `/api/v1/admin/users/:id`

Get user details including API keys and usage count.

**Response (200):**
```json
{
  "user": {"id": "uuid", "email": "customer@co.com", "tier": "pro"},
  "api_keys": [{"id": "uuid", "label": "production-key", "active": true}],
  "total_requests": 1250
}
```

#### PUT `/api/v1/admin/users/:id`

Update user (email, tier, password). Only include fields to change.

**Request:**
```json
{"tier": "enterprise"}
```

#### DELETE `/api/v1/admin/users/:id`

Soft-delete a user.

---

### Customer API Key Management

#### POST `/api/v1/admin/users/:id/keys`

Create an API key for a specific user/customer.

**Request:**
```json
{
  "label": "Acme Corp — Production",
  "scopes": ["asr", "tts"]
}
```

**Response (201):**
```json
{
  "id": "uuid",
  "key": "gtx_live_a4b8c2d1...",
  "label": "Acme Corp — Production",
  "scopes": ["asr", "tts"],
  "user_id": "uuid",
  "user_email": "acme@company.com",
  "created_at": "..."
}
```

> **Note:** The `key` value is shown **only once**. Store it securely.

#### GET `/api/v1/admin/users/:id/keys`

List all API keys for a user.

#### DELETE `/api/v1/admin/users/:id/keys/:keyId`

Revoke a specific API key.

---

### Global Usage

#### GET `/api/v1/admin/usage`

Aggregated usage across all users.

**Query params:** `period` — `day`, `week`, `month` (default)

**Response (200):**
```json
{
  "period": "month",
  "total_requests": 12500,
  "total_users": 42,
  "top_users": [
    {"user_id": "uuid", "email": "heavy@user.com", "request_count": 3200, "audio_hours": 45.2}
  ]
}
```

---

## Error Format

All errors return a consistent JSON structure:

```json
{
  "error": {
    "code": "INVALID_AUDIO",
    "message": "Unsupported audio format: .aac",
    "status": 415
  }
}
```

## Rate Limits

Default limits (per user, sliding window):

| Tier | Requests/min | Concurrent Streams |
|------|-------------|-------------------|
| `free` | 20 | 1 |
| `pro` | 120 | 5 |
| `enterprise` | unlimited | unlimited |

Rate limit headers included on every response:
```
X-RateLimit-Limit: 120
X-RateLimit-Remaining: 118
X-RateLimit-Reset: 1710720000
```

---

## PII Redaction

PII entities in the `transcript` / `prompt` log fields are replaced with `<TYPE>` placeholders before the structured log payload is emitted to stdout or shipped to Loki. The HTTP response body is **never** modified — only what engineers see in Grafana / `kubectl logs`.

### What's redacted

| Endpoint | Log event | Field redacted |
|---|---|---|
| `POST /api/v1/asr` | `ASR_COMPLETED` | `transcript` |
| `POST /v1/audio/transcriptions` | `WHISPER_COMPLETED` | `transcript` |
| `POST /v1/audio/transcriptions` | verbose request log | `prompt` |
| `POST /v1/recognize` | `WATSON_RECOGNIZE_COMPLETED` | `transcript` |

Streaming WebSocket endpoints (`/ws/asr`, `/v1/listen`, `/v1/recognize` WS) currently log only audio bytes / duration / process time at session end — no transcript text — so redaction is a no-op there. If transcript logging is added to those paths in the future, it must go through the same redactor.

### Default entities

`PERSON`, `EMAIL_ADDRESS`, `PHONE_NUMBER`, `CREDIT_CARD`, `US_SSN`, `IP_ADDRESS`, `IBAN_CODE`, `URL`, `DATE_TIME`, `LOCATION`. Override via `PII_ENTITIES=PERSON,EMAIL_ADDRESS,...`.

### Configuration

| Env var | Default | Description |
|---|---|---|
| `ENABLE_PII` | `true` | Master switch. When `false`, transcripts are logged verbatim. |
| `PRESIDIO_ANALYZER_URL` | `http://presidio-analyzer:3000` | URL of the Presidio analyzer service. Override to point at an external/centralized deployment. |
| `PRESIDIO_TIMEOUT_MS` | `3000` | Per-call HTTP timeout to the analyzer. |
| `PII_ENTITIES` | _(empty)_ | Comma-separated entity list. Empty = use built-in default set. |
| `PII_SCORE_THRESHOLD` | `0.6` | Minimum Presidio confidence to consider a detection (0.0–1.0). |

### Fail-closed semantics

When the analyzer is unreachable, times out, or returns an invalid response, the affected log field is replaced with the literal string `<REDACTED-ERROR>` and a separate `PII_REDACTOR_ERROR` warning event is emitted. Operators see degraded mode via the `gotranscribesrv_pii_errors_total{reason="analyzer_error"}` Prometheus counter.

Example degraded log line:
```json
{"type":"ASR_COMPLETED","additional_data":{"transcript":"<REDACTED-ERROR>","pii_redacted":0,...}}
```

### Deployment topology

- **Default (recommended)** — the `presidio-analyzer` container runs in the same `docker-compose.yml` as the Go server. Network call is intra-host, no auth required, and PII-bearing text never leaves the cluster.
- **Centralized** — set `PRESIDIO_ANALYZER_URL=https://presidio.internal.company.com` and remove the `presidio-analyzer` service from your compose file. Saves ~700 MB RAM per node by sharing one Presidio deployment across many gotranscribesrv nodes. Tradeoff: PII text now crosses the network to a shared service. Use only when your trust boundary includes that endpoint.

### Sample log line, before and after

**Before** (a Whisper-compat request whose audio contained personally-identifiable content):
```json
{
  "type": "WHISPER_COMPLETED",
  "message": "Whisper-compat transcription completed",
  "request_id": "8a4f...",
  "additional_data": {
    "transcript": "Hi, my name is John Smith and my phone number is 212-555-1234. Email me at john@example.com.",
    "word_count": 22,
    "segment_count": 1
  }
}
```

**After** (with `ENABLE_PII=true` and the default entity set):
```json
{
  "type": "WHISPER_COMPLETED",
  "message": "Whisper-compat transcription completed",
  "request_id": "8a4f...",
  "additional_data": {
    "transcript": "Hi, my name is <PERSON> and my phone number is <PHONE_NUMBER>. Email me at <EMAIL_ADDRESS>.",
    "word_count": 22,
    "segment_count": 1,
    "pii_redacted": 3,
    "pii_entity_types": ["PERSON", "PHONE_NUMBER", "EMAIL_ADDRESS"]
  }
}
```

### Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `gotranscribesrv_pii_redactions_total` | Counter | `entity_type` | Total PII entities replaced, by detected type. |
| `gotranscribesrv_pii_duration_seconds` | Histogram | `result` (`success`/`error`) | Wall-clock latency of `POST /analyze` calls. |
| `gotranscribesrv_pii_errors_total` | Counter | `reason` | Redactor failures by reason. |

---

## Audit Logging

Every request that hits the API server produces structured log events. The full pipeline is `slog → stdout → optional Loki`, with mirrored rows to PostgreSQL for successful requests (`usage_logs`) and failed requests (`request_logs`).

### What gets logged

| Event | Type | Source | When |
|---|---|---|---|
| `REQUEST_RECEIVED` | `*_REQUEST_RECEIVED` | handlers | Every speech/voice/process request arrives |
| `REQUEST_COMPLETED` | `*_COMPLETED` | handlers | Every successful ASR / TTS / voice response |
| `REQUEST_FAILED` | `REQUEST_FAILED` | middleware | Every 4xx/5xx response across all authed endpoints |
| `AUTH_FAILED` | `AUTH_FAILED` | middleware | Every failed authentication attempt (401) |
| `PII_REDACTOR_ERROR` | `PIIRedactorError` | handlers | PII analyzer unreachable / errored (fail-closed trigger) |
| `RATE_LIMITED` | (HTTP 429) | middleware | Rate limit rejections, also via `gotranscribesrv_rate_limit_rejections_total` |

### Where to find things

| Question | Loki query |
|---|---|
| Who failed auth in the last hour? | `{type="AUTH_FAILED"} \| json \| reason="bad_signature"` |
| What's the PII error rate? | `sum(rate(gotranscribesrv_pii_errors_total[5m]))` |
| What endpoints 5xx'd today? | `{type="REQUEST_FAILED"} \| json \| status>=500 \| endpoint` |
| What's the slow ASR latency distribution? | `histogram_quantile(0.99, sum(rate(gotranscribesrv_asr_processing_duration_seconds_bucket[5m])) by (le, endpoint))` |
| Token blacklist hits? | `{type="AUTH_FAILED"} \| json \| reason="blacklisted"` |

### Failed auth — what's recorded

Every `AUTH_FAILED` event carries:
- `auth_method`: `jwt`, `api_key`, `basic`, `jwt_query`
- `reason`: `missing_token`, `expired`, `bad_signature`, `malformed`, `wrong_algorithm`, `blacklisted`, `unknown_or_revoked`, `invalid`, `malformed_header`
- `endpoint`, `method`, `ip`, `user_agent`, `request_id`

**Security: the raw token, API key, or password is NEVER logged — only metadata about the failure.** Operators correlate failed-auth patterns via the `reason` and `auth_method` labels.

### Failed requests — what's recorded

Every `REQUEST_FAILED` event (via the `UsageTracker` middleware) carries:
- `endpoint`, `status`, `error_code` (extracted from JSON error body), `method`, `path`, `ip`, `user_agent`, `content_type`, `process_ms`, `user_id` (when authenticated), `api_key_id`

A `request_logs` row is also written to PostgreSQL on every 4xx/5xx (skipping anonymous failures, since there's no `user_id` to attach).

### What is NOT logged

- Raw request bodies (audio bytes, JSON payloads) — only metadata (file_size, content_type, word_count).
- Raw response bodies — only metadata.
- Raw tokens, API keys, or passwords — never, in any log event.
- WebSocket streaming session-end events currently log only audio bytes / duration / process time — no transcript text. If transcript text is added to those paths, it will go through the PII redactor before logging.

### Prometheus metrics for audit

| Metric | Type | Labels |
|---|---|---|
| `gotranscribesrv_http_requests_total` | Counter | `method`, `path`, `status` |
| `gotranscribesrv_auth_attempts_total` | Counter | `method` (`jwt`/`api_key`), `result` (`success`/`failure`) |
| `gotranscribesrv_rate_limit_rejections_total` | Counter | `tier` |
| `gotranscribesrv_sidecar_errors_total` | Counter | `sidecar`, `operation` |
| `gotranscribesrv_pii_errors_total` | Counter | `reason` |




---

## Appendix: Models, formats & sample rates

A flat lookup for callers — which models / engines exist, what audio formats each endpoint accepts and emits, and at what sample rates.

### ASR surfaces

| Surface                                  | Engine                              | Input formats                                   | Output formats            | Sample rates |
|------------------------------------------|-------------------------------------|-------------------------------------------------|---------------------------|--------------|
| `POST /api/v1/asr`                       | Parakeet TDT v3 0.6B (batch)        | Multipart `audio` upload — mp3/wav/opus/flac/m4a/ogg/aac/caf (any codec `SidecarAudioConverter` decodes) | JSON `{text, words, segments, duration, processing_time_ms}` | resampled to 16 kHz mono internally |
| `POST /v1/audio/transcriptions` (OpenAI) | Parakeet TDT v3 0.6B (batch)        | Multipart `file` upload                          | JSON (OpenAI-compatible: `text`/`verbose_json`/`srt`/`vtt`) | resampled to 16 kHz mono internally |
| `POST /v1/recognize` (Watson REST)       | Parakeet TDT v3 0.6B (batch)        | multipart `audio` upload                         | JSON Watson-shaped response | resampled to 16 kHz mono internally |
| `WS /ws/asr`                             | Parakeet TDT v3 0.6B (buffered pseudo-streaming — full-buffer re-transcribe every ~2 s) | Binary PCM16 frames (JSON control: `start`/`stop`) | JSON events: `ready`/`partial`/`final`/`done` | 8 kHz upsampled to 16 kHz, or native 16 kHz |
| `WS /v1/listen` (Deepgram-compat, legacy) | Parakeet TDT v3 0.6B (buffered)     | Binary PCM16/μ-law/A-law frames (JSON `start`/`stop`) | JSON Deepgram-shaped events | 8 kHz upsampled to 16 kHz, or native 16 kHz |
| `WS /v1/recognize` (Watson WS)           | Parakeet TDT v3 0.6B (buffered)      | Binary PCM16 frames (JSON Watson start)          | JSON Watson-shaped events | 8 kHz upsampled to 16 kHz, or native 16 kHz |
| `WS /stream/realtime` (sidecar, native)  | EOU / Nemotron / Unified streaming — see [Streaming engines](#streaming-engines-engine) | Binary PCM16/μ-law/A-law frames (JSON `stop`/`CloseStream`) | JSON events: `ready`/`speech_started`/`speech_stopped`/`partial`/`end_of_turn`/`final`/`done`/`error` | 8 kHz upsampled to 16 kHz, or native 16 kHz |
| `WS /v2/listen` (Deepgram-compat, realtime) | EOU / Nemotron / Unified streaming | Binary PCM16/μ-law/A-law frames | JSON Deepgram-shaped events (`Metadata`/`SpeechStarted`/`Results`/`UtteranceEnd`/`Error`) | 8 kHz upsampled to 16 kHz, or native 16 kHz |
| `WS /v1/realtime` (OpenAI-compat, realtime) | EOU / Nemotron / Unified streaming | JSON `input_audio_buffer.append` (base64 PCM16) + `session.update` | JSON OpenAI Realtime events: `session.created`/`updated`/`input_audio_buffer.speech_started`/`speech_stopped`/`committed`/`conversation.item.input_audio_transcription.delta`/`.completed`/`error` | 8 kHz upsampled to 16 kHz, or native 16 kHz |
| `POST /vad`                              | Silero VAD v6.2.1 (streaming-capable) | Multipart `audio` upload                         | JSON `{speech_segments: [{start, end}], duration, processing_time_ms}` | resampled to 16 kHz mono internally |
| `POST /diarize`                           | Sortformer v2.1 (4 speakers max)     | Multipart `audio` upload                         | JSON `{segments: [{speaker, start, end}], duration, processing_time_ms}` | resampled to 16 kHz mono internally |
| `POST /api/v1/voices/clone`              | PocketTTS voice-embedding extractor   | Multipart `audio` upload (5–15 sec clear speech) | Binary embedding bytes (`application/octet-stream`) | resampled to 16 kHz mono internally |

### TTS surfaces

| Surface                                  | Backends available                  | Input                                | Output format                                | Output sample rate | Notes |
|------------------------------------------|-------------------------------------|--------------------------------------|----------------------------------------------|--------------------|-------|
| `POST /synthesize` (sidecar)             | PocketTTS (default) or Kokoro via `?backend=` | JSON `{text, voice, voice_id?, voice_ref?, speed, format}` | WAV (full 44-byte header, 16-bit PCM mono)   | 24 kHz             | Voice cloning (PocketTTS) — `voice_id` / `voice_ref` rejected when `?backend=kokoro`. |
| `POST /synthesize/stream` (sidecar)      | PocketTTS only (locked)             | JSON `{text, voice}`                 | **Streaming** raw Int16 little-endian L16 frames, 80 ms each (1920 samples/frame), `Transfer-Encoding: chunked` | 24 kHz             | `Content-Type: audio/L16; rate=24000; channels=1`. Wrap with your own WAV header if you need a `.wav`. `?backend=kokoro` → 501 (Kokoro has no streaming API in FluidAudio 0.15.5). |
| `POST /api/v1/tts` (Go, legacy)          | PocketTTS only                      | JSON `{text, voice, voice_id?, voice_ref?, speed, format}` | WAV (full header)                            | 24 kHz             | Back-compat — always PocketTTS, voice cloning supported. |
| `POST /v1/audio/speech` (Go, OpenAI)     | PocketTTS or Kokoro via `model=`     | JSON `{model, voice, input, response_format, speed}` | WAV (full header) **or** raw L16 (`response_format=pcm`) | 24 kHz             | Default backend is `kokoro` (Go env `TTS_DEFAULT_BACKEND=kokoro`). Explicit `model=tts-1`/`tts-1-hd`/`gpt-4o-mini-tts` pins to PocketTTS. `response_format=mp3`/`opus`/`flac` → 501. |

### Voice IDs / names — quick lookup

| Backend   | Voice names                                                                                  | Multilingual? |
|-----------|----------------------------------------------------------------------------------------------|---------------|
| PocketTTS | `default`, `jane`, `alba`, `charles`, `anna`, `eve`, `george`, `paul`, `mary`, `michael`, `vera`, `jean`, `eponine`, `fantine`, `marius`, `cosette`, `azelma` + any filesystem `.wav` in `voices/` + user-cloned `voice_id` (UUID) | English       |
| Kokoro    | `af_heart`, `af_bella`, `af_sky`, `af_nicole`, `am_adam`, `am_michael`, `bf_emma`, `bf_isabella`, `bm_george`, `zf_001`, `zf_002`, `zm_001`, `jf_alpha` | English + Mandarin + Japanese |
| OpenAI (via `/v1/audio/speech`) | `alloy`→jane, `ash`→charles, `ballad`→mary, `coral`→eve, `echo`→alba, `sage`→george, `shimmer`→anna, `verse`→michael, `onyx`→paul, `nova`→vera, `fable`→jean — Kokoro IDs pass through unchanged. | mirrors backend |

### Sample rates & container formats

- **ASR**: always 16 kHz mono internally — the sidecar resamples on input and emits 16 kHz-relative timings on output. Input container/codec is irrelevant (decoded via `SidecarAudioConverter.toPCM16kMono`); opus, mp3, m4a/aac, ogg/vorbis, flac, wav, caf all work.
- **TTS**: always 24 kHz mono, 16-bit signed little-endian PCM. WAV = full 44-byte header + samples; raw PCM (used by `/synthesize/stream` and OpenAI `response_format=pcm`) = samples only.
- **WS streaming inputs**: PCM16 (signed 16-bit little-endian) at 8 kHz (auto-upsampled 2×) or 16 kHz, or μ-law/A-law at 8 kHz.
- **VAD / diarization**: 16 kHz mono internally.

### Configuration env vars

| Env var                          | Default | Where read   | Effect                                                                 |
|----------------------------------|---------|--------------|------------------------------------------------------------------------|
| `AUDIO_SIDECAR_URL`              | `http://127.0.0.1:8101` | Go       | Audio sidecar REST base URL. (`SWIFT_SIDECAR_URL` still works as a fallback.) |
| `AUDIO_SIDECAR_WS_URL`           | `ws://127.0.0.1:8101`  | Go       | Audio sidecar WebSocket base URL. (`SWIFT_SIDECAR_WS_URL` still works as a fallback.) |
| `AUDIO_SIDECAR_PORT`             | `8101`  | Sidecar      | HTTP listen port.                                                      |
| `AUDIO_SIDECAR_HOST`             | `0.0.0.0` | Sidecar    | HTTP listen host.                                                      |
| `SIDECAR_REALTIME_ENGINE`        | `eou-320` | Sidecar     | Default streaming engine for `/stream/realtime`, `/v2/listen`, `/v1/realtime`. |
| `SIDECAR_TTS_DEFAULT_BACKEND`    | `pocket` | Sidecar      | Default backend for `/synthesize` when `?backend=` isn't sent. Back-compat for Go `/api/v1/tts`. |
| `SIDECAR_TTS_STREAM_BACKEND`     | `pocket` | Sidecar      | Default backend for `/synthesize/stream`. Anything other than `pocket` is rejected with a startup warning (Kokoro streaming unsupported in FluidAudio 0.15.5). |
| `TTS_DEFAULT_BACKEND` (Go)       | `kokoro` | Go           | Default backend for `/v1/audio/speech` when `model` is omitted or unrecognized. Set to `pocket` to preserve legacy behavior. |
| `ENABLE_ITN`                     | `true`  | Go + Sidecar | Spoken-form → written-form normalization (numbers, dates, etc.).       |
| `ENABLE_DIARIZATION`             | `true`  | Go + Sidecar | Sortformer diarization in `/api/v1/asr` (inline).                      |
| `ENABLE_TTS`                     | `true`  | Go + Sidecar | TTS feature flag (server-wide).                                        |
| `REALTIME_S2S_ENABLED`           | `false` | Go           | Speech-to-speech mode on `WS /v1/realtime` (`?model=gpt-realtime*`). Requires the LLM sidecar. See [docs/realtime.md](realtime.md). |
| `REALTIME_S2S_MODEL`             | `mistral-7b-int4` | Go | LLM model (sidecar-llm registry id) for S2S turns.           |
| `REALTIME_S2S_VOICE`             | `default` | Go        | PocketTTS voice for S2S spoken responses.                              |
| `REALTIME_S2S_MAX_TOKENS`        | `300`   | Go           | Per-turn response token cap.                                           |
| `REALTIME_S2S_TEMPERATURE`       | `0.7`   | Go           | LLM sampling temperature.                                              |
| `REALTIME_S2S_INTERRUPTIONS`     | `true`  | Go           | Barge-in: user speech cancels the in-flight response.                  |

Verify sidecar env values via `/health.config`:

```json
{
  "config": {
    "synthesizeBackend": "pocket",
    "streamBackend": "pocket",
    "realtimeEngine": "eou-320"
  }
}
```

(`SIDECAR_REALTIME_ENGINE` is logged on startup; it isn't surfaced in `/health` today — see `docs/setup.md` for the runtime env reference.)
