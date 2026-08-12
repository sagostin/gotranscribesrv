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
    "tts": "pockettts"
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

Lists the models this server advertises, following the OpenAI `/v1/models` schema. Includes both OpenAI-branded mock IDs (so unmodified OpenAI SDKs can look up a known model) and the real on-device engines actually doing the work.

**Request:** No body. Auth required (Bearer token or API key).

| Query | Type | Description |
|-------|------|-------------|
| `owned_by` | string | Optional. Return only entries whose `owned_by` matches (e.g. `openai`, `nvidia`, `kyutai`, `meta`). |

**Response — `200 OK`:**

```json
{
  "object": "list",
  "data": [
    { "id": "whisper-1", "object": "model", "created": 1677649200, "owned_by": "openai" },
    { "id": "gpt-4o-transcribe", "object": "model", "created": 1742000000, "owned_by": "openai" },
    { "id": "gpt-4o-mini-transcribe", "object": "model", "created": 1742000000, "owned_by": "openai" },
    { "id": "gpt-4o-transcribe-diarize", "object": "model", "created": 1742000000, "owned_by": "openai" },
    { "id": "parakeet-tdt-v3-coreml", "object": "model", "created": 1735689600, "owned_by": "nvidia" },
    { "id": "tts-1", "object": "model", "created": 1696280400, "owned_by": "openai" },
    { "id": "tts-1-hd", "object": "model", "created": 1696280400, "owned_by": "openai" },
    { "id": "gpt-4o-mini-tts", "object": "model", "created": 1736380800, "owned_by": "openai" },
    { "id": "pocket-tts-1", "object": "model", "created": 1735603200, "owned_by": "kyutai" }
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

## Text-to-Speech (TTS) — PocketTTS

### POST `/api/v1/tts`

Synthesize speech using PocketTTS with voice cloning support. 24 kHz output.

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

Diarization is handled by the Swift sidecar using the Sortformer model (end-to-end neural, up to 4 speakers) running on CoreML/ANE.

> **Note:** Standalone speaker detection (without transcription) is not supported.
> The Sortformer diarizer requires transcript word/segment timestamps to produce
> meaningful per-speaker results.

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
    "tts": {"requests": 47, "audio_duration_sec": 1220}
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

