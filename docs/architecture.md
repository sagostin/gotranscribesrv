# Architecture

## Design Philosophy

GoTranscribeSrv follows a **modular multi-sidecar** architecture:

1. **Go (Fiber)** handles everything HTTP — routing, auth, WebSocket management, usage tracking
2. **Swift (Vapor + FluidAudio)** handles all audio AI — ASR, VAD, diarization, TTS via CoreML/Apple Neural Engine
3. **Python (FastAPI + mlx-lm)** handles LLM processing only — summarization, action items, translation *(optional)*
4. **PostgreSQL** is the only shared service across nodes
5. **Each node is stateless and identical** — horizontal scaling is just "add another Mac Mini"

This keeps each component in the language where it's strongest: Go for API infrastructure, Swift for native Apple Silicon performance, Python for ML-framework-heavy LLM inference.

---

## System Topology

### Single Node

```
┌─────────────────────────────────────────────────────────────────────┐
│  Mac Mini (M4, 24 GB)                                               │
│                                                                     │
│  ┌──────────────────────────┐  ┌──────────────────────────────────┐ │
│  │  Go — Fiber (:3000)      │  │  Swift — Vapor (:8101)           │ │
│  │                          │  │                                  │ │
│  │  • JWT Auth Middleware    │  │  • Parakeet TDT v3 (CoreML/ANE) │ │
│  │  • Usage Tracking MW     │  │  • Sortformer Diarizer (ANE)    │ │
│  │  • Rate Limiting (mem)   │  │  • Silero VAD (CoreML/ANE)      │ │
│  │  • REST Handlers         │  │  • PocketTTS (CoreML/ANE)       │ │
│  │  • Whisper-compat API    │  │  • POST /transcribe             │ │
│  │  • Deepgram-compat API   │  │  • WS   /stream                │ │
│  │  • Watson-compat API     │  │  • POST /synthesize             │ │
│  │  • WebSocket ASR Proxy   │  │  • POST /diarize                │ │
│  │  • Sidecar HTTP Client   │──│  • POST /vad                     │ │
│  └──────────┬───────────────┘  └──────────────────────────────────┘ │
│             │                                                       │
│             │                  ┌──────────────────────────────────┐ │
│             │                  │  Python — FastAPI (:8100)        │ │
│             │                  │  (optional — LLM only)           │ │
│             │                  │                                  │ │
│             │                  │  • Llama 3.1 8B Q4 (mlx-lm)     │ │
│             └─────────────────→│  • POST /process                │ │
│                                │  • GET  /process/tasks          │ │
│                                └──────────────────────────────────┘ │
│                                                                     │
└─────────────┬───────────────────────────────────────────────────────┘
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

### Split Deployment (Recommended for Production)

The Go API gateway and the inference sidecars don't need to run on the same machine. Since the sidecars are accessed via environment variables (`SWIFT_SIDECAR_URL`, `SWIFT_SIDECAR_WS_URL`, `LLM_SIDECAR_URL`), you can run the Go server on your normal server infrastructure and keep the Macs as dedicated inference nodes:

```
                    ┌──────────────────────────────┐
                    │  Standard Server / VPS / K8s  │
                    │                               │
                    │  Go API (:3000)               │
                    │  Auth, Usage, Rate Limiting   │
                    │  PostgreSQL (or external)     │
                    └──────────┬────────────────────┘
                               │
                    ┌──────────▼────────────────────┐
                    │  Caddy Reverse Proxy           │
                    │  Load-balance + health check   │
                    │  across Mac Mini sidecar pool  │
                    └──────────┬────────────────────┘
                 ┌─────────────┼─────────────┐
                 ▼             ▼             ▼
           ┌──────────┐ ┌──────────┐ ┌──────────┐
           │Mac Mini 1│ │Mac Mini 2│ │Mac Mini 3│
           │Swift     │ │Swift     │ │Swift     │
           │:8101     │ │:8101     │ │:8101     │
           │(+Py 8100)│ │(+Py 8100)│ │(+Py 8100)│
           └──────────┘ └──────────┘ └──────────┘
```

**Why split?**
- Macs become pure inference appliances — only run `make swift-sidecar` (and optionally `make sidecar`), no Go, no Postgres
- API layer runs on standard commodity infra (Docker, K8s, $5/mo VPS, etc.)
- Scale API and inference independently
- Caddy provides automatic health checks and failover across Mac pool

**Config (Go server `.env`):**
```bash
SWIFT_SIDECAR_URL=https://inference.internal:443    # Caddy proxy to Mac pool
SWIFT_SIDECAR_WS_URL=wss://inference.internal:443   # WebSocket via Caddy
LLM_SIDECAR_URL=https://inference.internal:443       # Optional, same proxy
```

**Note:** Both sidecars must stay on Apple Silicon — the Swift sidecar requires CoreML (ASR, VAD, diarization, TTS engines) and the Python sidecar requires MLX (LLM inference).

See the [Setup Guide](setup.md#split-deployment) for detailed configuration steps.

---

## Data Flow

### File Upload ASR

```
Client                    Go (Fiber)                Swift (Vapor/FluidAudio)
  │                          │                           │
  │  POST /api/v1/asr       │                           │
  │  [multipart: audio.wav]  │                           │
  │─────────────────────────►│                           │
  │                          │  POST /transcribe         │
  │                          │  [multipart: audio + cfg] │
  │                          │──────────────────────────►│
  │                          │                           │ AudioConverter
  │                          │                           │ → 16kHz mono PCM
  │                          │                           │ Parakeet TDT v3 (CoreML)
  │                          │                           │ (optional) Sortformer diarize
  │                          │  JSON transcript          │
  │                          │◄──────────────────────────│
  │  JSON response           │                           │
  │◄─────────────────────────│                           │
  │                          │  [async] Log usage        │
  │                          │─────► PostgreSQL          │
```

### Streaming ASR (Real-Time)

```
Client                    Go (Fiber)                Swift (Vapor/FluidAudio)
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
  │                          │                           │ Decode (PCM/μ-law/A-law)
  │                          │                           │ Resample → 16kHz
  │                          │                           │ Buffer + ASR (CoreML)
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

### LLM Processing (Optional)

```
Client                    Go (Fiber)                Python (FastAPI/mlx-lm)
  │                          │                           │
  │  POST /api/v1/process    │                           │
  │  [transcript + task]     │                           │
  │─────────────────────────►│                           │
  │                          │  POST /process            │
  │                          │  [transcript + task]      │
  │                          │──────────────────────────►│
  │                          │                           │ Llama 3.1 8B (MLX)
  │                          │  JSON result              │
  │                          │◄──────────────────────────│
  │  JSON response           │                           │
  │◄─────────────────────────│                           │
```

---

## Model Pipeline

### ASR Pipeline

```
Audio Input (any format)
    │
    ▼
AudioConverter (FluidAudio)
    │  Format detection + resample → 16kHz mono PCM
    ▼
[Streaming only] Silero VAD (CoreML/ANE)
    │  Detects speech segments
    │  Runs on Apple Neural Engine — no GPU contention
    ▼
Parakeet TDT v3 (CoreML/ANE)
    │  FastConformer encoder
    │  TDT decoder
    │  → text + word-level timestamps
    ▼
[Optional] Speaker Diarization
    │  Sortformer (end-to-end neural, up to 4 speakers)
    │  Maps words to speakers via overlap analysis
    │  → speaker labels per segment
    ▼
JSON Response
    {
      "text": "full transcript",
      "segments": [
        {"speaker": "SPEAKER_00", "start": 0.0, "end": 2.1, "text": "Hello..."},
        {"speaker": "SPEAKER_01", "start": 2.3, "end": 4.5, "text": "Hi there..."}
      ],
      "duration": 60.0,
      "processing_time_ms": 52
    }
```

### Model Memory Layout (24 GB M4 Mac Mini)

```
┌───────────────────────────────────────────────┐
│  Unified Memory — 24 GB                       │
│                                               │
│  ┌────────────┐  macOS + system    ~3.5 GB    │
│  ├────────────┤  Parakeet TDT v3   ~1.2 GB    │
│  ├────────────┤  PocketTTS         ~0.5 GB    │
│  ├────────────┤  Sortformer        ~0.2 GB    │
│  ├────────────┤  Silero VAD        ~0.05 GB   │
│  ├────────────┤  LLM (Llama 8B Q4) ~4.5 GB *  │
│  ├────────────┤  Swift runtime     ~0.2 GB    │
│  ├────────────┤  Go runtime        ~0.1 GB    │
│  ├────────────┤  Audio buffers     ~0.3 GB    │
│  ├────────────┤  PostgreSQL**      ~1.0 GB    │
│  ├────────────┤  ── Free (24GB) ── ~12.5 GB   │
│  ├────────────┤  ── Free (32GB) ── ~20.5 GB   │
│  └────────────┘                               │
│  *  Only if ENABLE_LLM=true (Python sidecar)  │
│  ** Only if DB is colocated on this node       │
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

| Stage | Nodes | Infra Cost (CAD) | Handles |
|-------|-------|-----------|---------| 
| **Dev** | 1× M4 16GB | $700 | 3–5 streams, 0.6B model |
| **Launch** | 1× M4 24GB | $950 | 5–8 streams, ASR + TTS + diarization |
| **Recommended** | 1× M4 32GB | $1,150 | Full stack incl. LLM processing |
| **Growth** | 3× M4 32GB + LB | $3,700 | 15–24 streams, all features |
| **Scale** | 5–10× M4 32GB + LB + dedicated PG | $6,700–$13,000 | 25–80 streams |

**When to add nodes:** Monitor `process_time / audio_duration` ratio. If it exceeds 0.5 (model taking >50% of real-time to process), the node is saturated.

**When to upgrade nodes:** If you need per-node throughput (not just total), move to M4 Pro (48GB, faster GPU/ANE). This gives ~50% more throughput per node vs base M4.
