# GoTranscribeSrv

**On-device speech services powered by NVIDIA Parakeet TDT on Apple Silicon.**

A Go/Fiber backend with a Python inference sidecar providing ASR (speech-to-text), TTS (text-to-speech), and speaker diarization — all running locally on Mac Mini hardware. No cloud APIs, no GPU rental, full data privacy.

---

## Features

| Feature | Details |
|---------|---------|
| **Real-Time Streaming ASR** | WebSocket endpoint for live transcription with partial results |
| **File Upload ASR** | Single file or chunked upload; returns full transcript with timestamps |
| **Whisper-Compatible API** | Drop-in replacement for OpenAI's `/v1/audio/transcriptions` endpoint |
| **Speaker Diarization** | Optional per-request; identifies and labels speakers (NeMo Sortformer) |
| **Text-to-Speech** | LuxTTS with zero-shot voice cloning, 48 kHz output, pre-built voice presets |
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
         │ Go+Py    │  │ Go+Py    │  │ Go+Py    │
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
- **Python (FastAPI)** — ML inference (Parakeet TDT via MLX, Sortformer diarization, LuxTTS)
- Communication: HTTP/WebSocket on localhost (no gRPC overhead needed)

See [docs/architecture.md](docs/architecture.md) for detailed design.

---

## Quick Start

### Option A: Docker (Recommended)

```bash
git clone https://github.com/yourorg/gotranscribesrv.git
cd gotranscribesrv
cp .env.example .env
# Edit .env: set JWT_SECRET and POSTGRES_PASSWORD

docker compose up -d
docker compose logs -f server   # Watch for admin credentials
```

On first boot, an admin user is automatically created with a **random password** and API key printed to the console:
```
  ✅ Admin user created
  │ Email:    admin@gotranscribesrv.local
  │ Password: aX9#kL2mP...  (random, shown once)
  │ API Key:  gtx_live_a4b8...
```

### Option B: Manual Setup

**Prerequisites:** macOS 14+ (Apple Silicon M1/M2/M4), Go 1.22+, Python 3.11+, PostgreSQL 15+

```bash
git clone https://github.com/yourorg/gotranscribesrv.git
cd gotranscribesrv
cp .env.example .env     # Edit DB credentials, JWT secret

# Pre-download ML models + voice presets
make setup               # Runs setup-models + setup-voices

# Start Python sidecar
make sidecar &           # Loads models, serves on :8100

# Start Go backend
make dev                 # Runs migrations, seeds admin, serves on :3000
```

### 4. Test It

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
  -d '{"text": "Hello from GoTranscribeSrv", "voice": "professional"}' \
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
| `POST` | `/api/v1/tts` | Synthesize speech from text |
| `GET`  | `/api/v1/usage/summary` | Usage stats for current user |
| `GET`  | `/api/v1/usage/history` | Detailed usage history |

Full reference: [docs/api.md](docs/api.md)

---

## Pricing & Infrastructure

### Hardware Costs (One-Time)

| Config | Chip | RAM | Price | Concurrent Streams | Use Case |
|--------|------|-----|-------|--------------------|----------|
| **Dev/Staging** | M4 | 16 GB | ~$500 | 3–5 | Development, 0.6B model |
| **Production Node** | M4 | 24 GB | ~$700 | 5–8 | 1.1B model + diarization |
| **Power Node** | M4 Pro | 48 GB | ~$1,400 | 8–12 | Heavy workloads, all features |

### Cluster Scaling

| Nodes | Est. Hardware | Concurrent Streams | File Transcriptions/sec | Monthly Power* |
|-------|-------------|-------------------|------------------------|----------------|
| 1× M4 24GB | $700 | 5–8 | ~5–6 | ~$5 |
| 3× M4 24GB | $2,100 | 15–24 | ~15–18 | ~$15 |
| 5× M4 24GB | $3,500 | 25–40 | ~25–30 | ~$25 |
| 10× M4 24GB | $7,000 | 50–80 | ~50–60 | ~$50 |

*Mac Mini power consumption: ~15–30W under ML load

### Cost Comparison vs Cloud ASR

| Provider | Per Audio Hour | 1,000 hrs/month | 10,000 hrs/month |
|----------|---------------|-----------------|------------------|
| Google Speech-to-Text | $1.44 | $1,440 | $14,400 |
| AWS Transcribe | $1.44 | $1,440 | $14,400 |
| Azure Speech | $1.00 | $1,000 | $10,000 |
| Deepgram | $0.75 | $750 | $7,500 |
| **GoTranscribeSrv (3-node)** | **$0** | **$2,100 one-time + ~$15/mo** | **same** |

A 3-node cluster pays for itself in **under 2 months** vs cloud pricing at 1,000 hrs/month.

### Throughput Reference

| Model | Speed (vs real-time) | 1 hr audio transcribed in |
|-------|---------------------|--------------------------|
| Parakeet TDT 0.6B (CoreML, M4 Pro) | ~110x | ~33 seconds |
| Parakeet TDT 1.1B (CoreML, M4) | ~60–80x | ~45–60 seconds |
| Parakeet TDT 1.1B (MLX, M4) | ~40–60x | ~60–90 seconds |

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
│   └── sidecar/                # HTTP/WS client for Python
├── sidecar/
│   ├── main.py                 # FastAPI inference server
│   ├── routers/                # ASR, TTS endpoints
│   ├── engines/                # Model wrappers
│   └── voices/                 # Pre-built voice presets (LibriTTS-R)
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
| Inference Server | Python + [FastAPI](https://fastapi.tiangolo.com) |
| ASR Model | [Parakeet TDT 0.6B](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2) via MLX |
| Diarization | NeMo Sortformer + TitaNet |
| VAD | Silero VAD |
| TTS | [LuxTTS](https://github.com/ysharma3501/LuxTTS) — 48 kHz, zero-shot voice cloning |
| Load Balancer | Caddy / Nginx (multi-node) |

---

## License

MIT
