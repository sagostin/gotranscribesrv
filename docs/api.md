# API Reference

Base URL: `http://localhost:3000`

All endpoints except `/auth/*` require authentication via either:
- **JWT**: `Authorization: Bearer <access_token>`
- **API Key**: `X-API-Key: <key>`

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
| `audio` | file | yes | Audio file (WAV, MP3, FLAC, OGG, M4A) |
| `diarize` | string | no | `"true"` to enable speaker diarization |
| `timestamps` | string | no | `"true"` for word-level timestamps (default: true) |
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

**Errors:** `413` file too large, `415` unsupported format, `422` invalid params

---

### POST `/v1/audio/transcriptions` (Whisper-Compatible)

Drop-in replacement for the OpenAI Whisper API. Allows existing tools and SDKs that target the OpenAI transcription endpoint to work with GoTranscribeSrv without code changes.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `file` | file | yes | Audio file (mp3, mp4, mpeg, mpga, m4a, wav, webm) |
| `model` | string | yes | Model name (accepted but ignored; always uses Parakeet TDT) |
| `language` | string | no | ISO-639-1 language code (default: `"en"`) |
| `prompt` | string | no | Hint text (accepted for compatibility, best-effort) |
| `response_format` | string | no | `"json"`, `"text"`, `"srt"`, `"vtt"`, `"verbose_json"` (default: `"json"`) |
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

> **Compatibility note:** The `model` field is required by the OpenAI spec, so clients will send it (e.g., `"whisper-1"`). GoTranscribeSrv accepts any value but always uses Parakeet TDT. Fields like `temperature` are accepted without error but have no effect.

**Errors:** `413` file too large (>25 MB), `415` unsupported format

---

### WS `/ws/asr`

Real-time streaming transcription over WebSocket.

**Connection:** Upgrade with auth token as query param:
```
ws://localhost:3000/ws/asr?token=<access_token>&language=en&diarize=false
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

## Text-to-Speech (TTS) — LuxTTS

### POST `/api/v1/tts`

Synthesize speech using LuxTTS with zero-shot voice cloning. 48 kHz output.

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

> **Note:** If both `voice` and `voice_ref` are provided, `voice_ref` takes priority.

**Built-in voice presets** (curated from LibriTTS-R, CC BY 4.0):

| Voice ID | Description |
|----------|-------------|
| `default` | Neutral, clear American English |
| `professional` | Formal, confident tone |
| `friendly` | Warm, conversational |
| `narrator` | Deep, documentary style |
| `bright` | Energetic, upbeat |

**Response (200):** Audio binary stream at 48 kHz.

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

## Speaker Diarization

### POST `/api/v1/diarize`

Standalone speaker detection without transcription. Returns speaker segments.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `audio` | file | yes | Audio file (WAV, MP3, FLAC, OGG, M4A) |

**Response (200):**
```json
{
  "speakers": {
    "SPEAKER_00": [
      {"start": 0.0, "end": 2.1},
      {"start": 4.5, "end": 8.3}
    ],
    "SPEAKER_01": [
      {"start": 2.3, "end": 4.4}
    ]
  },
  "num_speakers": 2,
  "duration": 8.3,
  "processing_time_ms": 1250
}
```

**Errors:** `413` file too large, `502` sidecar unavailable

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

