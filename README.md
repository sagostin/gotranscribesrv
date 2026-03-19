# GoTranscribeSrv

**On-device speech services powered by CoreML/Apple Neural Engine on Apple Silicon.**

A Go/Fiber backend with a Swift inference sidecar (FluidAudio) providing ASR (speech-to-text), TTS (text-to-speech), speaker diarization, and VAD — all running natively on Mac Mini hardware via CoreML and the Apple Neural Engine. An optional Python sidecar handles on-device LLM processing. No cloud APIs, no GPU rental, full data privacy.

---

## Features

| Feature | Details |
|---------|---------|
| **Real-Time Streaming ASR** | WebSocket endpoint for live transcription with partial results |
| **File Upload ASR** | Single file or chunked upload; returns full transcript with timestamps |
| **Whisper-Compatible API** | Drop-in replacement for OpenAI's `/v1/audio/transcriptions` endpoint |
| **Deepgram-Compatible API** | Drop-in replacement for Deepgram's `/v1/listen` WebSocket endpoint |
| **Speaker Diarization** | Optional per-request; identifies and labels speakers (Sortformer, up to 4 speakers) |
| **Text-to-Speech** | PocketTTS with voice cloning support, 24 kHz output |
| **LLM Transcript Processing** | On-device summarization, action items, translation, Q&A via Llama 3.1 8B (optional) |
| **User Authentication** | JWT access/refresh tokens + API key support |
| **Usage Tracking** | Per-user metering: audio duration, processing time, endpoint |
| **Rate Limiting** | Per-user, in-memory sliding window |
| **Horizontal Scaling** | Add Mac Minis behind a load balancer; stateless nodes, shared PostgreSQL |

---

## Architecture

```
                     ┌──────────────────┐
                     │  Load Balancer   │
                     │  (Caddy/Nginx)   │
                     └────────┬─────────┘
               ┌──────────────┼──────────────┐
               ▼              ▼              ▼
         ┌──────────┐  ┌──────────┐  ┌──────────┐
         │Mac Mini 1│  │Mac Mini 2│  │Mac Mini 3│
         │Go+Swift  │  │Go+Swift  │  │Go+Swift  │
         │  (+Py)   │  │  (+Py)   │  │  (+Py)   │
         └────┬─────┘  └────┬─────┘  └────┬─────┘
              └──────────────┼──────────────┘
                             ▼
                     ┌──────────────────┐
                     │   PostgreSQL     │
                     │   (shared)       │
                     └──────────────────┘
```

Each node runs:
- **Go (Fiber)** — API gateway, auth, WebSocket handling, usage tracking
- **Swift (Vapor + FluidAudio)** — Audio AI inference: ASR (Parakeet TDT v3), VAD (Silero), speaker diarization (Sortformer), TTS (PocketTTS) — all via CoreML/ANE
- **Python (FastAPI)** *(optional)* — LLM transcript processing (Llama 3.1 8B via mlx-lm)
- Communication: HTTP/WebSocket on localhost between all components

**Split deployment (recommended for production):** The Go API server can run on standard server infrastructure (Docker, K8s, VPS) while the Macs serve as dedicated inference nodes behind a Caddy reverse proxy. Sidecar URLs are fully configurable via `SWIFT_SIDECAR_URL` / `SWIFT_SIDECAR_WS_URL` / `LLM_SIDECAR_URL` env vars — no code changes needed.

See [docs/architecture.md](docs/architecture.md) for detailed design.

---

## Quick Start

### Option A: Docker + Native Sidecars (Recommended)

Docker runs PostgreSQL and the Go API server. The Swift and Python sidecars run natively on the Mac for CoreML/ANE access.

```bash
git clone https://github.com/yourorg/gotranscribesrv.git
cd gotranscribesrv
cp .env.example .env
# Edit .env: set JWT_SECRET and POSTGRES_PASSWORD

# Terminal 1 — Postgres + Go API
make up

# Terminal 2 — Swift sidecar (ASR, VAD, diarization, TTS)
make swift-sidecar       # Builds & serves on :8101

# Terminal 3 — Python sidecar (LLM only, optional)
make sidecar             # Serves on :8100
```

On first boot, an admin user is automatically created with a **random password** and API key printed to the console:
```
  ✅ Admin user created
  │ Email:    admin@gotranscribesrv.local
  │ Password: aX9#kL2mP...  (random, shown once)
  │ API Key:  gtx_live_a4b8...
```

### Option B: Fully Manual Setup

**Prerequisites:** macOS 14+ (Apple Silicon M1/M2/M4), Go 1.22+, Swift 6.0+ (Xcode 16+), Python 3.11+ (for LLM only), PostgreSQL 15+

```bash
git clone https://github.com/yourorg/gotranscribesrv.git
cd gotranscribesrv
cp .env.example .env     # Edit DB credentials, JWT secret

# Start Swift sidecar (downloads models on first run)
make swift-sidecar &     # Loads CoreML models, serves on :8101

# Optional: Start Python sidecar for LLM processing
make setup               # Downloads LLM models
make sidecar &           # Serves on :8100

# Start Go backend
make dev                 # Runs migrations, seeds admin, serves on :3000
```

### Test It

```bash
# Use the admin API key from the console output:
curl -X POST http://localhost:3000/api/v1/asr \
  -H "X-API-Key: gtx_live_..." \
  -F "audio=@sample.wav" \
  -F "diarize=true"

# Or use the Whisper-compatible endpoint:
curl -X POST http://localhost:3000/v1/audio/transcriptions \
  -H "X-API-Key: gtx_live_..." \
  -F "file=@sample.wav" \
  -F "response_format=verbose_json"

# Synthesize speech:
curl -X POST http://localhost:3000/api/v1/tts \
  -H "X-API-Key: gtx_live_..." \
  -H "Content-Type: application/json" \
  -d '{"text": "Hello from GoTranscribeSrv", "voice": "default"}' \
  --output speech.wav
```

---

## API Overview

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/auth/register` | Create account |
| `POST` | `/api/v1/auth/login` | Login → JWT tokens |
| `POST` | `/api/v1/auth/refresh` | Refresh access token |
| `POST` | `/api/v1/asr` | Transcribe uploaded audio file |
| `POST` | `/v1/audio/transcriptions` | OpenAI Whisper-compatible endpoint |
| `WS`   | `/ws/asr` | Real-time streaming transcription |
| `WS`   | `/v1/listen` | Deepgram-compatible streaming transcription |
| `POST` | `/api/v1/tts` | Synthesize speech from text |
| `GET`  | `/api/v1/voices` | List available TTS voice presets |
| `POST` | `/api/v1/process` | LLM transcript processing (summarize, action items, etc.) |
| `GET`  | `/api/v1/process/tasks` | List available LLM processing tasks |
| `GET`  | `/api/v1/usage/summary` | Usage stats for current user |
| `GET`  | `/api/v1/usage/history` | Detailed usage history |

Full reference: [docs/api.md](docs/api.md)

---

## Pricing & Infrastructure

### Hardware Costs (One-Time)

| Config | Chip | RAM | Price (CAD) | Concurrent Streams | Use Case |
|--------|------|-----|-------|--------------------|----------|
| **Dev/Staging** | M4 | 16 GB | ~$700 | 3–5 | Development, 0.6B model |
| **Standard** | M4 | 24 GB | ~$950 | 5–8 | ASR + TTS + diarization |
| **Recommended** | M4 | 32 GB | ~$1,150 | 5–8 | Full stack incl. LLM processing |
| **Power Node** | M4 Pro | 48 GB | ~$1,900 | 8–12 | Heavy concurrent LLM + ASR |

### Cluster Scaling

| Nodes | Est. Hardware (CAD) | Concurrent Streams | File Transcriptions/sec | Monthly Power* |
|-------|-------------|-------------------|------------------------|----------------|
| 1× M4 24GB | $950 | 5–8 | ~5–6 | ~$7 |
| 3× M4 24GB | $2,850 | 15–24 | ~15–18 | ~$20 |
| 5× M4 24GB | $4,750 | 25–40 | ~25–30 | ~$35 |
| 10× M4 24GB | $9,500 | 50–80 | ~50–60 | ~$70 |

*Mac Mini power consumption: ~15–30W under ML load

### Cost Comparison vs Cloud ASR

| Provider | Per Audio Hour (CAD) | 1,000 hrs/month | 10,000 hrs/month |
|----------|---------------|-----------------|------------------|
| Google Speech-to-Text | $2.00 | $2,000 | $20,000 |
| AWS Transcribe | $2.00 | $2,000 | $20,000 |
| Azure Speech | $1.40 | $1,400 | $14,000 |
| Deepgram | $1.05 | $1,050 | $10,500 |
| **GoTranscribeSrv (3-node)** | **$0** | **$2,850 one-time + ~$20/mo** | **same** |

A 3-node cluster pays for itself in **under 2 months** vs cloud pricing at 1,000 hrs/month.

### Throughput Reference

| Model | Speed (vs real-time) | 1 hr audio transcribed in |
|-------|---------------------|-----------------------------|
| Parakeet TDT v3 (CoreML, M4 Pro) | ~110x | ~33 seconds |
| Parakeet TDT v3 (CoreML, M4) | ~60–80x | ~45–60 seconds |

---

## Project Structure

```
gotranscribesrv/
├── cmd/server/main.go          # Entry point
├── internal/
│   ├── config/                 # Environment config
│   ├── database/               # PostgreSQL + migrations
│   ├── models/                 # User, APIKey, UsageLog
│   ├── middleware/             # Auth, usage, rate limit
│   ├── handlers/               # Route handlers
│   └── sidecar/                # HTTP/WS client for sidecars
├── sidecar-swift/              # Swift inference sidecar
│   ├── Package.swift           # SPM manifest (Vapor + FluidAudio)
│   └── Sources/Server/
│       ├── main.swift          # Entry point
│       ├── EngineManager.swift # Model lifecycle (ASR, VAD, diarizer, TTS)
│       ├── AudioConverter.swift
│       └── Routes/             # Transcribe, Stream, VAD, Diarize, TTS, Health
├── sidecar/                    # Python sidecar (LLM only)
│   ├── main.py                 # FastAPI server
│   ├── routers/                # LLM processing endpoints
│   └── engines/                # LLM engine wrapper
├── docs/                       # Architecture, API, setup
├── .env.example
├── Makefile
└── docker-compose.yml
```

---

## Tech Stack

| Layer | Technology |
|-------|-----------|
| API Gateway | Go + [Fiber v2](https://gofiber.io) |
| Auth | JWT (access/refresh) + API keys, bcrypt |
| Database | PostgreSQL 15+ via [GORM](https://gorm.io) |
| Inference Server | Swift + [Vapor](https://vapor.codes) + [FluidAudio](https://github.com/FluidInference/FluidAudio) |
| ASR Model | [Parakeet TDT v3](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2) via CoreML/ANE |
| Diarization | Sortformer (end-to-end neural, up to 4 speakers) |
| VAD | Silero VAD (CoreML/ANE) |
| TTS | PocketTTS — 24 kHz, voice cloning support |
| LLM Processing | Llama 3.1 8B (4-bit) via [mlx-lm](https://github.com/ml-explore/mlx-examples) — opt-in, Python sidecar |
| Load Balancer | Caddy / Nginx (multi-node) |

---

## License

MIT
