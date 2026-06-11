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
| **Watson-Compatible API** | Drop-in replacement for IBM Watson's `/v1/recognize` endpoint (HTTP + WebSocket) |
| **Speaker Diarization** | Optional per-request; identifies and labels speakers (Sortformer, up to 4 speakers) |
| **Text-to-Speech** | PocketTTS with per-user stored voice cloning + 17 built-in system voices, 24 kHz output |
| **LLM Transcript Processing** | *Opt-in, ~4.5 GB RAM overhead* — summarization, action items, translation, Q&A via Llama 3.1 8B. Most deployments leave this off. |
| **User Authentication** | JWT access/refresh tokens + API key support |
| **Usage Tracking** | Per-user metering: audio duration, processing time, endpoint |
| **Rate Limiting** | Per-user, in-memory sliding window |
| **Inverse Text Normalization (ITN)** | Optional spoken→written form (e.g. "five dollars" → "$5.00"); `ENABLE_ITN=true`; per-request override via `?itn=false` |
| **Admin / Enterprise API** | User management, customer API key issuance, global usage rollup (`/api/v1/admin/*`, enterprise tier) |
| **Prometheus Metrics** | `/metrics` endpoint with HTTP, ASR, TTS, LLM, sidecar, auth, and rate-limit collectors |
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
- **Python (FastAPI)** *(optional, off by default)* — LLM transcript processing (Llama 3.1 8B via mlx-lm). Off in most deployments.
- Communication: HTTP/WebSocket on localhost between all components

**Split deployment (recommended for production):** The Go API server can run on standard server infrastructure (Docker, K8s, VPS) while the Macs serve as dedicated inference nodes behind a Caddy reverse proxy. Sidecar URLs are fully configurable via `SWIFT_SIDECAR_URL` / `SWIFT_SIDECAR_WS_URL` / `LLM_SIDECAR_URL` env vars — no code changes needed.

See [docs/architecture.md](docs/architecture.md) for detailed design.

---

## Quick Start

### Option A: Docker + Native Sidecars (Recommended)

Docker runs PostgreSQL and the Go API server. The Swift and Python sidecars run natively on the Mac for CoreML/ANE access.

```bash
git clone https://github.com/sagostin/gotranscribesrv.git
cd gotranscribesrv
cp .env.example .env
# Edit .env: set JWT_SECRET and POSTGRES_PASSWORD

# Terminal 1 — Postgres + Go API
make up

# Terminal 2 — Swift sidecar (ASR, VAD, diarization, TTS) — required
make swift-sidecar       # Builds & serves on :8101

# Terminal 3 — Python sidecar (LLM only) — OPTIONAL, skip for pure ASR/TTS
#   See "Optional Components" below. Adds ~4.5 GB RAM.
make sidecar             # Serves on :8100
```

> **Want ITN (spoken→written form conversion, on by default)?** You must build the Rust static lib and rebuild the Swift sidecar **before** `make swift-sidecar`. See [Optional Components → ITN](#inverse-text-normalization-itn--on-by-default) for the 3-step sequence.

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
git clone https://github.com/sagostin/gotranscribesrv.git
cd gotranscribesrv
cp .env.example .env     # Edit DB credentials, JWT secret

# Start Swift sidecar (downloads models on first run)
make swift-sidecar &     # Loads CoreML models, serves on :8101

# OPTIONAL: Python sidecar for LLM transcript processing (skip for pure ASR/TTS)
make setup               # Downloads LLM models (~5 GB)
make sidecar &           # Serves on :8100

# Start Go backend
make run                 # Starts Go backend, runs migrations, seeds admin (:3000)
```

> **For ITN (on by default):** Run `make itn-vendor && make itn-build && make swift-build` **before** `make swift-sidecar`. See [Optional Components → ITN](#inverse-text-normalization-itn--on-by-default).

### Test It

```bash
# Verify the stack is up (server + sidecars reachable):
curl -s http://localhost:3000/health | jq

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

## Optional Components

The core stack — ASR, VAD, diarization, TTS, auth, usage tracking — runs out of the box. Two optional components add capability at the cost of extra build steps and/or memory:

- **ITN** — spoken→written form conversion. *On by default* in `.env`, but requires a one-time Rust build before the Swift sidecar will link it. Skip the build and the sidecar still runs; ITN is just a no-op.
- **LLM** — transcript post-processing (summarize, action items, Q&A). *Off by default*; opt-in via `ENABLE_LLM=true` + Python sidecar. Adds ~4.5 GB RAM. Most deployments leave this off.

### Inverse Text Normalization (ITN) — *on by default*

The Swift sidecar post-processes ASR output to convert spoken-form into written form:

| Spoken (ASR raw) | Written (ITN output) |
|---|---|
| "one two five O" | `1250` |
| "five dollars and fifty cents" | `$5.50` |
| "january fifth twenty twenty five" | `January 5, 2025` |

**Enable / disable:**
- **Globally** in `.env`: `ENABLE_ITN=true` (default) or `ENABLE_ITN=false`
- **Per request** (overrides global):
  - REST: `itn=false` form field
  - WebSocket: `?itn=false` query param

**Build prerequisite — must run *before* `make swift-sidecar`:**

ITN links a Rust static library (`libtext_processing_rs.a`) into the Swift sidecar at compile time. If you skip this and start the sidecar anyway, ITN is a no-op passthrough and ASR will return spoken-form output.

```bash
# 1. Vendor text-processing-rs (clones from GitHub if not present; no-op if already vendored)
make itn-vendor

# 2. Build the Rust static lib (requires Rust toolchain — `brew install rust` on Apple Silicon)
make itn-build

# 3. Rebuild the Swift sidecar to link the static lib
make swift-build

# 4. (Optional) Verify the ITN tests pass
make swift-test

# 5. Now start the sidecar with ITN linked
make swift-sidecar
```

> **Why this is a separate step:** The Rust toolchain (`cargo`) is a hard build-time dependency for ITN. By making it opt-in, users who only need raw ASR don't need to install Rust. The Makefile targets auto-detect Apple Silicon vs Intel (`RUST_TARGET`) and clone the correct repo (`v0.2.2`).

> **If `make itn-build` fails or you skip it:** The Swift sidecar still builds and runs, but ITN is silently disabled — `ENABLE_ITN=true` becomes a no-op. The sidecar logs a warning at startup.

### LLM Transcript Processing — *opt-in, adds ~4.5 GB RAM*

Powers `POST /api/v1/process` (summarize, action items, translation, Q&A, custom prompts) via Llama 3.1 8B (4-bit) on MLX.

> **Most deployments do not need this.** It exists for transcript post-processing. If your use case is pure ASR/TTS, **leave `ENABLE_LLM=false` and skip the Python sidecar entirely** — that saves ~4.5 GB of unified memory on a 16 GB Mac Mini and avoids the ~5 GB model download.

**To enable:**
```bash
# 1. Edit .env:
ENABLE_LLM=true
LLM_MODEL=mlx-community/Meta-Llama-3.1-8B-Instruct-4bit

# 2. Download the model (~5 GB, one-time):
make setup

# 3. Start the Python sidecar:
make sidecar        # Serves on :8100

# 4. Verify the /api/v1/process/tasks endpoint returns the task list
curl -H "X-API-Key: gtx_live_..." http://localhost:3000/api/v1/process/tasks
```

**Memory impact:**

| Node RAM | With LLM enabled | Recommended config |
|---|---|---|
| 16 GB | Tight (~5 GB free with full stack); OOM risk under load | 16 GB nodes should keep `ENABLE_LLM=false` |
| 24 GB | Comfortable (~12 GB free) | Default for `ENABLE_LLM=true` |
| 32 GB+ | Plenty of headroom | Run LLM + ASR + TTS + diarization concurrently |

**Tasks exposed** (see [docs/api.md](docs/api.md#llm-transcript-processing)):
`summarize` · `action_items` · `translate` · `qa` · `custom`

---

## API Overview

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/health` | Server + sidecar connectivity check (no auth) |
| `GET` | `/metrics` | Prometheus metrics scrape (path configurable via `METRICS_PATH`) |
| `POST` | `/api/v1/auth/register` | Create account |
| `POST` | `/api/v1/auth/login` | Login → JWT tokens |
| `POST` | `/api/v1/auth/refresh` | Refresh access token |
| `POST` | `/api/v1/auth/logout` | Invalidate refresh token |
| `POST` | `/api/v1/asr` | Transcribe uploaded audio file |
| `POST` | `/v1/audio/transcriptions` | OpenAI Whisper-compatible endpoint |
| `WS`   | `/ws/asr` | Real-time streaming transcription |
| `WS`   | `/v1/listen` | Deepgram-compatible streaming transcription |
| `POST` | `/v1/recognize` | Watson-compatible file transcription |
| `WS`   | `/v1/recognize` | Watson-compatible streaming transcription |
| `POST` | `/api/v1/tts` | Synthesize speech from text |
| `POST` | `/api/v1/voices/clone` | Upload audio to create a stored cloned voice |
| `GET`  | `/api/v1/voices` | List custom + system voices |
| `GET`  | `/api/v1/voices/:id` | Get custom voice details |
| `DELETE` | `/api/v1/voices/:id` | Delete a custom voice |
| `POST` | `/api/v1/process` | LLM transcript processing (summarize, action items, etc.) |
| `GET`  | `/api/v1/process/tasks` | List available LLM processing tasks |
| `GET`  | `/api/v1/usage/summary` | Usage stats for current user |
| `GET`  | `/api/v1/usage/history` | Detailed usage history |
| `GET`  | `/api/v1/usage/keys/:id` | Per-key usage summary |
| `GET`  | `/api/v1/usage/me` | Current API key's usage (API-key auth only) |
| `POST` | `/api/v1/keys` | Generate API key |
| `GET`  | `/api/v1/keys` | List API keys |
| `DELETE` | `/api/v1/keys/:id` | Revoke API key |
| `GET`  | `/api/v1/admin/users` | List all users (enterprise tier) |
| `POST` | `/api/v1/admin/users` | Create user/customer (enterprise tier) |
| `GET`  | `/api/v1/admin/users/:id` | Get user details (enterprise tier) |
| `PUT`  | `/api/v1/admin/users/:id` | Update user (enterprise tier) |
| `DELETE` | `/api/v1/admin/users/:id` | Soft-delete user (enterprise tier) |
| `POST` | `/api/v1/admin/users/:id/keys` | Create API key for user (enterprise tier) |
| `GET`  | `/api/v1/admin/users/:id/keys` | List user's API keys (enterprise tier) |
| `DELETE` | `/api/v1/admin/users/:id/keys/:keyId` | Revoke user's API key (enterprise tier) |
| `GET`  | `/api/v1/admin/usage` | Global usage across all users (enterprise tier) |

Full reference: [docs/api.md](docs/api.md)

---

## Pricing & Infrastructure

### Hardware Costs (One-Time)

| Config | Chip | RAM | Price (CAD) | Concurrent Streams | Use Case |
|--------|------|-----|-------|--------------------|----------|
| **Dev** | M4 | 16 GB / 256 GB | ~$799 | 3–5 | Development, 0.6B model |
| **Standard** | M4 | 24 GB / 512 GB | $1,399 | 5–8 | ASR + TTS + diarization |
| **Recommended** | M4 | 32 GB / 512 GB | ~$1,599 | 5–8 | Full stack incl. LLM processing |
| **Power Node** | M4 Pro | 48 GB | ~$1,899 | 8–12 | Heavy concurrent LLM + ASR (~50% more throughput vs base M4) |

### Cluster Scaling

| Nodes | Est. Hardware (CAD) | Concurrent Streams | File Transcriptions/sec | Monthly Power* |
|-------|-------------|-------------------|------------------------|----------------|
| 1× M4 24 GB / 512 GB | $1,399 | 5–8 | ~5–6 | ~$7 |
| 3× M4 24 GB / 512 GB | $4,197 | 15–24 | ~15–18 | ~$20 |
| 5× M4 24 GB / 512 GB | $6,995 | 25–40 | ~25–30 | ~$35 |
| 10× M4 24 GB / 512 GB | $13,990 | 50–80 | ~50–60 | ~$70 |

*Mac Mini power consumption: ~15–30W under ML load

### Cost Comparison vs Cloud ASR

| Provider | Per Audio Hour (CAD) | 1,000 hrs/month | 10,000 hrs/month |
|----------|---------------|-----------------|------------------|
| Google Speech-to-Text | $2.00 | $2,000 | $20,000 |
| AWS Transcribe | $2.00 | $2,000 | $20,000 |
| Azure Speech | $1.40 | $1,400 | $14,000 |
| Deepgram | $1.05 | $1,050 | $10,500 |
| **GoTranscribeSrv (3-node)** | **$0** | **$4,197 one-time + ~$20/mo** | **same** |

A 3-node cluster pays for itself in **3–4 months** vs cloud pricing at 1,000 hrs/month (faster vs AWS/Google, slower vs Deepgram).

### Throughput Reference

| Model | Speed (vs real-time) | 1 hr audio transcribed in |
|-------|---------------------|-----------------------------|
| Parakeet TDT v3 (CoreML, M4 Pro) | ~110x | ~33 seconds |
| Parakeet TDT v3 (CoreML, M4) | ~60–80x | ~45–60 seconds |

---

## Project Structure

```
gotranscribesrv/
├── cmd/server/main.go            # Entry point (route registration, middleware wiring)
├── internal/
│   ├── config/                   # Environment config
│   ├── database/                 # PostgreSQL + GORM setup
│   ├── models/                   # User, APIKey, UsageLog, Voice, RequestLog, TokenBlacklist
│   ├── middleware/               # Auth, usage tracking, rate limiting
│   ├── handlers/                 # Route handlers: asr, auth, whisper, deepgram, watson, tts, voices, process, usage, keys, admin, ws
│   ├── metrics/                  # Prometheus collectors + middleware
│   └── sidecar/                  # HTTP/WS client for Swift + Python sidecars
├── sidecar-swift/                # Swift inference sidecar (CoreML/ANE)
│   ├── Package.swift             # SPM manifest (Vapor + FluidAudio)
│   └── Sources/
│       ├── Server/
│       │   ├── main.swift        # Entry point
│       │   ├── EngineManager.swift   # Model lifecycle (ASR, VAD, diarizer, TTS)
│       │   ├── AudioConverter.swift  # Format detect + resample → 16 kHz mono PCM
│       │   └── Routes/               # Transcribe, Stream, VAD, Diarize, TTS, Health
│       └── ITNHelpers/           # Inverse text normalization (TextNormalizer)
├── sidecar/                      # Python sidecar (LLM, optional)
│   ├── main.py                   # FastAPI server
│   ├── routers/                  # Processing endpoints (process, asr)
│   ├── engines/                  # ML engine wrappers (LLM, diarizer, VAD, ASR)
│   └── inference_pool.py         # Concurrency-managed inference queue
├── sidecar-node/                 # Node.js sidecar (legacy, pre-Swift — ASR + VAD only)
│   ├── server.js                 # Express server
│   └── routes/                   # Transcribe, VAD, Health
├── scripts/                      # Operational scripts
├── docs/                         # Architecture, API, setup, pricing, cost analysis
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
| TTS | PocketTTS — 24 kHz, per-user stored voice cloning, 17+ built-in voices |
| LLM Processing | Llama 3.1 8B (4-bit) via [mlx-lm](https://github.com/ml-explore/mlx-examples) — opt-in, Python sidecar |
| Load Balancer | Caddy / Nginx (multi-node) |

---

## Documentation

| Document | Description |
|----------|-------------|
| [docs/api.md](docs/api.md) | Full API reference: auth, ASR, TTS, voices, usage, admin, error format, rate limits |
| [docs/architecture.md](docs/architecture.md) | System design, data flow, model pipeline, memory layout, scaling strategy |
| [docs/setup.md](docs/setup.md) | Detailed setup, environment variables, deployment topologies, 16 GB Mac Mini notes |
| [docs/pricing.md](docs/pricing.md) | Pricing reference and cost model |
| [docs/cost-benefit-analysis.md](docs/cost-benefit-analysis.md) | ROI analysis: self-hosted vs cloud APIs |
| [docs/nvidia_cost_analysis.md](docs/nvidia_cost_analysis.md) | Mac Mini vs NVIDIA GPU cost & throughput comparison |

---

## License

GNU General Public License v3.0
