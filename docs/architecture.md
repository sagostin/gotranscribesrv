# Architecture

## Design Philosophy

GoTranscribeSrv follows a **simple hybrid** architecture:

1. **Go (Fiber)** handles everything HTTP — routing, auth, WebSocket management, usage tracking
2. **Python (FastAPI)** handles everything ML — model loading, inference, audio processing
3. They communicate over **localhost HTTP/WebSocket** — no gRPC, no message queues
4. **PostgreSQL** is the only shared service across nodes
5. **Each node is stateless and identical** — horizontal scaling is just "add another Mac Mini"

This keeps each component in the language where it's strongest, with the simplest possible integration.

---

## System Topology

### Single Node

```
┌─────────────────────────────────────────────────────────────┐
│  Mac Mini (M4, 24 GB)                                       │
│                                                             │
│  ┌──────────────────────────┐  ┌──────────────────────────┐ │
│  │  Go — Fiber (:3000)      │  │  Python — FastAPI (:8100)│ │
│  │                          │  │                          │ │
│  │  • JWT Auth Middleware    │  │  • Parakeet TDT (MLX)    │ │
│  │  • Usage Tracking MW     │  │  • Sortformer Diarizer   │ │
│  │  • Rate Limiting (mem)   │  │  • Silero VAD            │ │
│  │  • REST Handlers         │  │  • LuxTTS (48kHz)        │ │
│  │  • Whisper-compat API    │  │  • POST /transcribe      │ │
│  │  • WebSocket ASR Proxy   │  │  • WS /stream            │ │
│  │  • Sidecar HTTP Client   │──│  • POST /synthesize      │ │
│  └──────────┬───────────────┘  └──────────────────────────┘ │
│             │                                               │
└─────────────┼───────────────────────────────────────────────┘
              ▼
       ┌──────────────┐
       │  PostgreSQL   │
       └──────────────┘
```

### Multi-Node (Production)

```
      Clients (Web, Mobile, SDK)
              │
              ▼
      ┌───────────────┐
      │ Load Balancer  │  Caddy / Nginx / HAProxy
      │ (TLS termination, round-robin)
      └───────┬───────┘
        ┌─────┼─────┐
        ▼     ▼     ▼
      Node1 Node2 Node3   ← Identical Mac Minis
        │     │     │
        └─────┼─────┘
              ▼
      ┌───────────────┐
      │  PostgreSQL    │  ← Single shared instance
      │  (dedicated    │     (or on Node 1 with 32GB)
      │   host or RDS) │
      └───────────────┘
```

**Why this works:**
- JWT tokens are stateless — any node can validate them (same signing secret)
- Usage logs write to shared PostgreSQL — no coordination needed
- WebSocket streams are connection-sticky by nature — no session affinity config required
- Rate limiting is per-node, which is fine when each node handles ≤8 concurrent streams

---

## Data Flow

### File Upload ASR

```
Client                    Go (Fiber)                Python (FastAPI)
  │                          │                           │
  │  POST /api/v1/asr       │                           │
  │  [multipart: audio.wav]  │                           │
  │─────────────────────────►│                           │
  │                          │  POST /transcribe         │
  │                          │  [audio bytes + config]   │
  │                          │──────────────────────────►│
  │                          │                           │ Load audio
  │                          │                           │ Resample → 16kHz
  │                          │                           │ Parakeet TDT inference
  │                          │                           │ (optional) Diarize
  │                          │  JSON transcript          │
  │                          │◄──────────────────────────│
  │  JSON response           │                           │
  │◄─────────────────────────│                           │
  │                          │  [async] Log usage        │
  │                          │─────► PostgreSQL          │
```

### Streaming ASR (Real-Time)

```
Client                    Go (Fiber)                Python (FastAPI)
  │                          │                           │
  │  WS /ws/asr              │                           │
  │  [upgrade]               │                           │
  │◄────────────────────────►│                           │
  │                          │  WS /stream               │
  │                          │  [upgrade]                │
  │                          │◄─────────────────────────►│
  │                          │                           │
  │  binary: audio chunk 1   │                           │
  │─────────────────────────►│  forward chunk 1          │
  │                          │──────────────────────────►│
  │                          │                           │ VAD + buffer
  │                          │  partial transcript       │
  │                          │◄──────────────────────────│
  │  text: partial result    │                           │
  │◄─────────────────────────│                           │
  │                          │                           │
  │  binary: audio chunk N   │                           │
  │─────────────────────────►│  forward chunk N          │
  │                          │──────────────────────────►│
  │                          │  final transcript         │
  │                          │◄──────────────────────────│
  │  text: final result      │                           │
  │◄─────────────────────────│                           │
  │                          │                           │
  │  close                   │  close                    │
  │─────────────────────────►│──────────────────────────►│
```

---

## Model Pipeline

### ASR Pipeline

```
Audio Input (any format)
    │
    ▼
Resample → 16kHz mono PCM
    │
    ▼
[Streaming only] Silero VAD
    │  Detects speech segments
    │  Runs on ANE (CoreML) — no GPU contention
    ▼
Parakeet TDT 1.1B (MLX on GPU)
    │  FastConformer encoder
    │  TDT decoder
    │  → text + word-level timestamps
    ▼
[Optional] Speaker Diarization
    │  TitaNet speaker embeddings
    │  Sortformer end-to-end clustering
    │  → speaker labels per segment
    ▼
JSON Response
    {
      "text": "full transcript",
      "segments": [
        {"speaker": "A", "start": 0.0, "end": 2.1, "text": "Hello..."},
        {"speaker": "B", "start": 2.3, "end": 4.5, "text": "Hi there..."}
      ],
      "duration": 60.0,
      "processing_time": 0.55
    }
```

### Model Memory Layout (24 GB M4 Mac Mini)

```
┌───────────────────────────────────────────────┐
│  Unified Memory — 24 GB                       │
│                                               │
│  ┌────────────┐  macOS + system    ~3.5 GB    │
│  ├────────────┤  Parakeet TDT 1.1B ~2.2 GB    │
│  ├────────────┤  LuxTTS            ~1.0 GB    │
│  ├────────────┤  Sortformer        ~0.2 GB    │
│  ├────────────┤  TitaNet + VAD     ~0.05 GB   │
│  ├────────────┤  Python runtime    ~0.8 GB    │
│  ├────────────┤  Go runtime        ~0.1 GB    │
│  ├────────────┤  Audio buffers     ~0.3 GB    │
│  ├────────────┤  Voice presets     ~0.05 GB   │
│  ├────────────┤  PostgreSQL*       ~1.0 GB    │
│  ├────────────┤  ─── Free ───      ~14.8 GB   │
│  └────────────┘                               │
│  * Only if DB is colocated on this node       │
└───────────────────────────────────────────────┘
```

---

## Authentication & Authorization

### JWT Flow

```
Register → bcrypt hash → store in PostgreSQL
                              │
Login → verify hash → issue JWT pair
                              │
         ┌────────────────────┼────────────────────┐
         ▼                                         ▼
   Access Token (15 min)                   Refresh Token (7 days)
   - Used for all API calls                - Used only at POST /refresh
   - Stored in Authorization header        - Stored in httpOnly cookie
   - Contains: user_id, email, tier        - Contains: user_id, token_id
```

### API Key Auth (Alternative)

For programmatic/SDK access:
- User generates API key from dashboard
- Key sent as `X-API-Key` header
- Server validates key hash against `api_keys` table
- Same middleware pipeline (usage tracking, rate limiting)

---

## Database Schema

> Schema is auto-managed by [GORM AutoMigrate](https://gorm.io/docs/migration.html) — no manual SQL migrations needed. The models in `internal/models/` define the tables below.

```sql
-- Users
CREATE TABLE users (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT UNIQUE NOT NULL,
    password    TEXT NOT NULL,  -- bcrypt hash
    tier        TEXT NOT NULL DEFAULT 'free',
    created_at  TIMESTAMPTZ DEFAULT now(),
    updated_at  TIMESTAMPTZ DEFAULT now()
);

-- API Keys
CREATE TABLE api_keys (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID REFERENCES users(id) ON DELETE CASCADE,
    key_hash    TEXT NOT NULL,  -- sha256 of the key
    label       TEXT,
    scopes      TEXT[] DEFAULT '{}',
    active      BOOLEAN DEFAULT true,
    created_at  TIMESTAMPTZ DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

-- Usage Log
CREATE TABLE usage_log (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID REFERENCES users(id),
    endpoint        TEXT NOT NULL,        -- 'asr', 'asr_stream', 'tts'
    audio_duration  INTEGER NOT NULL,     -- milliseconds of audio processed
    process_time    INTEGER NOT NULL,     -- milliseconds of processing time
    diarized        BOOLEAN DEFAULT false,
    metadata        JSONB DEFAULT '{}',
    created_at      TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX idx_usage_user_created ON usage_log(user_id, created_at DESC);
```

---

## Scaling Strategy

| Stage | Nodes | Infra Cost | Handles |
|-------|-------|-----------|---------|
| **Dev** | 1× M4 16GB | $500 | 3–5 streams, 0.6B model |
| **Launch** | 1× M4 24GB | $700 | 5–8 streams, 1.1B model |
| **Growth** | 3× M4 24GB + LB | $2,300 | 15–24 streams |
| **Scale** | 5–10× M4 24GB + LB + dedicated PG | $4,200–$8,000 | 25–80 streams |

**When to add nodes:** Monitor `process_time / audio_duration` ratio. If it exceeds 0.5 (model taking >50% of real-time to process), the node is saturated.

**When to upgrade nodes:** If you need per-node throughput (not just total), move to M4 Pro (48GB, faster GPU/ANE). This gives ~50% more throughput per node vs base M4.
