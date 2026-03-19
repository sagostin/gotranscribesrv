# API Reference

Base URL: `http://localhost:3000`

All endpoints except `/auth/*` require authentication via either:
- **JWT**: `Authorization: Bearer <access_token>`
- **API Key**: `X-API-Key: <key>`

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
    "llm": "llama-3.1-8b-q4"
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

**Errors:** `409` email exists, `422` validation failed

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

### POST `/v1/audio/transcriptions` (Whisper-Compatible)

Drop-in replacement for the OpenAI Whisper API. Allows existing tools and SDKs that target the OpenAI transcription endpoint to work with GoTranscribeSrv without code changes.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `file` | file | yes | Audio file (mp3, mp4, mpeg, mpga, m4a, wav, webm) |
| `model` | string | yes | Model name (accepted but ignored; always uses Parakeet TDT v3 via CoreML) |
| `language` | string | no | ISO-639-1 language code (default: `"en"`) |
| `prompt` | string | no | Hint text (accepted for compatibility, best-effort) |
| `response_format` | string | no | `"json"`, `"text"`, `"srt"`, `"vtt"`, `"verbose_json"` (default: `"json"`) |
| `stream` | string | no | `"true"` to enable SSE streaming (see below) |
| `temperature` | number | no | Accepted for compatibility (ignored) |
| `timestamp_granularities[]` | array | no | `"word"` and/or `"segment"` (requires `verbose_json`) |

**Response — `json` (default):**
```json
{
  "text": "Hello, how are you doing today?"
}
```

**Response — `verbose_json`:**
```json
{
  "task": "transcribe",
  "language": "en",
  "duration": 1.8,
  "text": "Hello, how are you doing today?",
  "segments": [
    {
      "id": 0,
      "start": 0.0,
      "end": 1.8,
      "text": "Hello, how are you doing today?",
      "tokens": [15947, 11, 577, 527, 345],
      "temperature": 0.0,
      "avg_logprob": -0.15,
      "compression_ratio": 1.2,
      "no_speech_prob": 0.01
    }
  ],
  "words": [
    {"word": "Hello", "start": 0.0, "end": 0.4},
    {"word": "how", "start": 0.5, "end": 0.7},
    {"word": "are", "start": 0.7, "end": 0.85},
    {"word": "you", "start": 0.85, "end": 1.0},
    {"word": "doing", "start": 1.0, "end": 1.3},
    {"word": "today", "start": 1.4, "end": 1.8}
  ]
}
```

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

2. **`transcript.text.done`** — final event with the complete transcript:
```
event: transcript.text.done
data: {"type":"transcript.text.done","text":"Hello, how are you doing today?","duration":1.8,"words":[{"word":"Hello","start":0.0,"end":0.4}],"logprobs":null}
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

## Text-to-Speech (TTS) — PocketTTS

### POST `/api/v1/tts`

Synthesize speech using PocketTTS with voice cloning support. 24 kHz output.

**Request:**
```json
{
  "text": "Hello, welcome to GoTranscribeSrv.",
  "voice": "professional",
  "speed": 1.0,
  "format": "wav"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `text` | string | yes | Text to synthesize (max 5,000 chars) |
| `voice` | string | no | Preset voice ID (default: `"default"`) |
| `voice_ref` | string | no | Base64-encoded audio reference for custom voice cloning (5–15 sec) |
| `speed` | number | no | Playback speed 0.5–2.0 (default: 1.0) |
| `format` | string | no | `"wav"`, `"opus"`, `"mp3"` (default: `"wav"`) |

> **Note:** If both `voice` and `voice_ref` are provided, `voice_ref` takes priority. TTS runs on the Swift sidecar via PocketTTS (CoreML/ANE).

**Built-in voice presets:**

| Voice ID | Description |
|----------|-------------|
| `default` | PocketTTS default voice |

**Response (200):** Audio binary stream at 24 kHz.

```
Content-Type: audio/wav
Content-Length: 96000
X-Audio-Sample-Rate: 48000
```

---

### GET `/api/v1/voices`

List available TTS voice presets.

**Response (200):**
```json
{
  "voices": [
    {"id": "default", "name": "Default", "description": "Neutral, clear American English"},
    {"id": "professional", "name": "Professional", "description": "Formal, confident tone"},
    {"id": "friendly", "name": "Friendly", "description": "Warm, conversational"},
    {"id": "narrator", "name": "Narrator", "description": "Deep, documentary style"},
    {"id": "bright", "name": "Bright", "description": "Energetic, upbeat"}
  ]
}
```

---

## LLM Transcript Processing

### POST `/api/v1/process`

Run LLM processing on transcript text. Requires the Python sidecar (mlx-lm).

**Request:**
```json
{
  "transcript_text": "Hello, how are you? I'm doing well, thanks for asking...",
  "task": "summarize",
  "max_tokens": 1024,
  "temperature": 0.3
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `transcript_text` | string | yes | The transcript text to process |
| `task` | string | no | Processing task (default: `"summarize"`) |
| `language` | string | conditional | Required when `task` is `"translate"` |
| `prompt` | string | conditional | Required when `task` is `"qa"` or `"custom"` |
| `max_tokens` | int | no | Max tokens to generate (default: 1024) |
| `temperature` | number | no | Sampling temperature (default: 0.3) |

**Available tasks:**

| Task | Description |
|------|-------------|
| `summarize` | Concise summary of the transcript |
| `action_items` | Extract action items and next steps |
| `translate` | Translate to target `language` |
| `qa` | Answer a question about the transcript (`prompt` required) |
| `custom` | Run a custom prompt against the transcript (`prompt` required) |

**Response (200):**
```json
{
  "result": "The discussion covered project timelines and resource allocation...",
  "task": "summarize",
  "model": "mlx-community/Meta-Llama-3.1-8B-Instruct-4bit",
  "processing_time_ms": 2340,
  "tokens_generated": 156
}
```

**Errors:** `422` missing required fields, `502` LLM sidecar unavailable

---

### GET `/api/v1/process/tasks`

List available LLM processing tasks.

**Response (200):**
```json
{
  "tasks": ["summarize", "action_items", "translate", "qa", "custom"],
  "descriptions": {
    "summarize": "Generate a concise summary of the transcript",
    "action_items": "Extract action items and next steps",
    "translate": "Translate the transcript to a target language",
    "qa": "Answer a question about the transcript",
    "custom": "Run a custom prompt against the transcript"
  }
}
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
    "tts": {"requests": 47, "audio_duration_sec": 1220},
    "process": {"requests": 85, "audio_duration_sec": 0}
  }
}
```

---

### GET `/api/v1/usage/history`

Paginated usage log entries.

**Query params:**

| Param | Type | Description |
|-------|------|-------------|
| `page` | int | Page number (default: 1) |
| `limit` | int | Items per page, max 100 (default: 20) |
| `endpoint` | string | Filter by endpoint |

**Response (200):**
```json
{
  "items": [
    {
      "id": "uuid",
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

