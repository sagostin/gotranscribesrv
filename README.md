# GoTranscribeSrv

**On-device speech services powered by CoreML/Apple Neural Engine on Apple Silicon.**

A Go/Fiber backend with a Swift inference sidecar (FluidAudio) providing ASR (speech-to-text — batch + true real-time streaming), TTS (PocketTTS + Kokoro), speaker diarization, and VAD — all running natively on Mac Mini hardware via CoreML and the Apple Neural Engine. No cloud APIs, no GPU rental, full data privacy.

---

## Features

| Feature | Details |
|---------|---------|
| **Real-Time Streaming ASR** | Two layers: (1) buffered pseudo-streaming on `/ws/asr`, `/v1/listen`, `/v1/recognize` (full-buffer re-transcribe every ~2 s); (2) **true streaming** with cache-aware encoder states and turn-taking events on `/v2/listen` (Deepgram-compat), `/v1/realtime` (OpenAI-compat), and `/stream/realtime` (native). Eight streaming engines: Parakeet EOU 120M (English + built-in EOU), Nemotron 0.6B (English), Parakeet Unified 0.6B (multilingual, 25 EU languages). |
| **Realtime Speech-to-Speech** | Full OpenAI Realtime S2S on `/v1/realtime` (opt-in: `REALTIME_S2S_ENABLED=true`, connect with `?model=gpt-realtime`): ASR → LLM → streaming TTS orchestrated by the Go server, EOU turn-taking, barge-in, client-side tool calling. See [docs/realtime.md](docs/realtime.md). |
| **File Upload ASR** | Single file or chunked upload; returns full transcript with timestamps |
| **Whisper-Compatible API** | Drop-in replacement for OpenAI's `/v1/audio/transcriptions` endpoint |
| **Deepgram-Compatible API** | Drop-in replacement for Deepgram's `/v1/listen` WebSocket endpoint |
| **Watson-Compatible API** | Drop-in replacement for IBM Watson's `/v1/recognize` endpoint (HTTP + WebSocket) |
| **Speaker Diarization** | Optional per-request; identifies and labels speakers (Sortformer, up to 4 speakers) |
| **Text-to-Speech** | PocketTTS (with per-user stored voice cloning + 17 built-in system voices) and Kokoro (multilingual EN/Mandarin/JP, higher quality), 24 kHz output. Default backend selectable per-endpoint; streaming TTS is PocketTTS-only. |
| **LLM Gateway** | OpenAI-compatible (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/images/generations`) and Anthropic-compatible (`/v1/messages`) endpoints proxied to the LLM sidecar (CoreML/ANE) with auth, rate limiting, and per-model token usage tracking. SSE streaming passthrough. |
| **User Authentication** | JWT access/refresh tokens + API key support |
| **Usage Tracking** | Per-user metering: audio duration, processing time, endpoint, per-model LLM token totals |
| **Rate Limiting** | Per-user, in-memory sliding window |
| **Inverse Text Normalization (ITN)** | Optional spoken→written form (e.g. "five dollars" → "$5.00"); `ENABLE_ITN=true`; per-request override via `?itn=false` |
| **Admin / Enterprise API** | User management, customer API key issuance, global usage rollup (`/api/v1/admin/*`, enterprise tier) |
| **Prometheus Metrics** | `/metrics` endpoint with HTTP, ASR, TTS, sidecar, auth, and rate-limit collectors |
| **PII Redaction in Logs** | Replaces PII entities in `transcript` / `result` / `prompt` log fields (Loki + stdout) via [Microsoft Presidio](https://microsoft.github.io/presidio/). Response bodies are never modified. Fail-closed. |
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
- **Swift (Vapor + FluidAudio)** — Audio AI inference: ASR (Parakeet TDT v3 batch + Parakeet EOU / Nemotron / Parakeet Unified true streaming), VAD (Silero, includes streaming turn events), speaker diarization (Sortformer), TTS (PocketTTS + Kokoro) — all via CoreML/ANE
- **Presidio Analyzer** *(optional, on by default)* — PII detection for log redaction (mcr.microsoft.com/presidio-analyzer). See "PII Redaction" below.
- Communication: HTTP/WebSocket on localhost between all components

**Split deployment (recommended for production):** The Go API server can run on standard server infrastructure (Docker, K8s, VPS) while the Macs serve as dedicated inference nodes behind a Caddy reverse proxy. Sidecar URLs are fully configurable via `AUDIO_SIDECAR_URL` / `AUDIO_SIDECAR_WS_URL` env vars — no code changes needed. (Legacy `SWIFT_SIDECAR_URL` / `SWIFT_SIDECAR_WS_URL` still work as fallbacks.)

**Multi-node production (Mac mini fleet + separate DB/Caddy VM):** Ready-made compose files — `docker-compose.node.yml` (server + Presidio per mini), `docker-compose.db.yml` (Postgres + Caddy load balancer), a `Caddyfile` with health-checked least-connection balancing, and `deploy/macos/` for headless auto-boot after power outages. See [docs/production.md](docs/production.md).

See [docs/architecture.md](docs/architecture.md) for detailed design.

---

## PII Redaction in Logs

The `transcript` field on `ASR_COMPLETED` / `WHISPER_COMPLETED` / `WATSON_RECOGNIZE_COMPLETED` and the `prompt` attribute on the Whisper verbose-request log are all run through a Microsoft Presidio analyzer before being placed into the structured log pipeline. PII entities (names, emails, phone numbers, credit cards, SSNs, IPs, IBANs, URLs, dates, locations) are replaced with `<TYPE>` placeholders. The HTTP response body is NEVER modified — only what shows up in Loki / stdout.

**Default behavior:** ON. Set `ENABLE_PII=false` in `.env` to disable.

**Fail-closed:** if the Presidio analyzer is unreachable or returns an error, the affected log field is replaced with the literal string `<REDACTED-ERROR>` and a `PII_REDACTOR_ERROR` warning event is emitted. The error is also exposed via Prometheus (`gotranscribesrv_pii_errors_total{reason="analyzer_error"}`) so operators see the degraded mode immediately.

**Deployment topology:**
- **Default** — Presidio runs as a sidecar container in the same `docker-compose.yml` (the `presidio-analyzer` service). Network call is intra-host, no auth required, and PII text never leaves the cluster. Recommended for most deployments.
- **Centralized** — set `PRESIDIO_ANALYZER_URL=https://presidio.internal.company.com` and remove the `presidio-analyzer` service from your compose file. Saves ~700 MB RAM per node by sharing one Presidio deployment. Tradeoff: PII-bearing transcript text now crosses the network to a shared service. Use only when your trust boundary includes that endpoint.

**Entities:** the default set is `PERSON, EMAIL_ADDRESS, PHONE_NUMBER, CREDIT_CARD, US_SSN, IP_ADDRESS, IBAN_CODE, URL, DATE_TIME, LOCATION`. Override via `PII_ENTITIES=PERSON,EMAIL_ADDRESS,...`.

See [docs/api.md → PII Redaction](docs/api.md#pii-redaction) for the full config reference and sample log lines.

---

## Quick Start

### Option A: Docker + Native Sidecar (Recommended)

Docker runs PostgreSQL and the Go API server. The audio sidecar runs natively on the Mac for CoreML/ANE access.

```bash
git clone https://github.com/sagostin/gotranscribesrv.git
cd gotranscribesrv
cp .env.example .env
# Edit .env: set JWT_SECRET and POSTGRES_PASSWORD

# Terminal 1 — Postgres + Go API
make up

# Terminal 2 — Audio sidecar (ASR, VAD, diarization, TTS) — required
make audio-sidecar       # Builds & serves on :8101
```

> **Want ITN (spoken→written form conversion, on by default)?** You must build the Rust static lib and rebuild the audio sidecar **before** `make audio-sidecar`. See [Optional Components → ITN](#inverse-text-normalization-itn--on-by-default) for the 3-step sequence.

On first boot, an admin user is automatically created with a **random password** and API key printed to the console:
```
  ✅ Admin user created
  │ Email:    admin@gotranscribesrv.local
  │ Password: aX9#kL2mP...  (random, shown once)
  │ API Key:  gtx_live_a4b8...
```

### Option B: Fully Manual Setup

**Prerequisites:** macOS 14+ (Apple Silicon M1/M2/M4), Go 1.22+, Swift 6.0+ (Xcode 16+), PostgreSQL 15+

```bash
git clone https://github.com/sagostin/gotranscribesrv.git
cd gotranscribesrv
cp .env.example .env     # Edit DB credentials, JWT secret

# Start audio sidecar (downloads models on first run)
make audio-sidecar &     # Loads CoreML models, serves on :8101

# Start Go backend
make run                 # Starts Go backend, runs migrations, seeds admin (:3000)
```

> **For ITN (on by default):** Run `make itn-vendor && make itn-build && make audio-build` **before** `make audio-sidecar`. See [Optional Components → ITN](#inverse-text-normalization-itn--on-by-default).

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

The core stack — ASR, VAD, diarization, TTS, auth, usage tracking — runs out of the box. One optional component adds capability at the cost of extra build steps:

- **ITN** — spoken→written form conversion. *On by default* in `.env`, but requires a one-time Rust build before the audio sidecar will link it. Skip the build and the sidecar still runs; ITN is just a no-op.

### Inverse Text Normalization (ITN) — *on by default*

The audio sidecar post-processes ASR output to convert spoken-form into written form:

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

**Build prerequisite — must run *before* `make audio-sidecar`:**

ITN links a Rust static library (`libtext_processing_rs.a`) into the audio sidecar at compile time. If you skip this and start the sidecar anyway, ITN is a no-op passthrough and ASR will return spoken-form output.

```bash
# 1. Vendor text-processing-rs (clones from GitHub if not present; no-op if already vendored)
make itn-vendor

# 2. Build the Rust static lib (requires Rust toolchain — `brew install rust` on Apple Silicon)
make itn-build

# 3. Rebuild the audio sidecar to link the static lib
make audio-build

# 4. (Optional) Verify the ITN tests pass
make audio-test

# 5. Now start the sidecar with ITN linked
make audio-sidecar
```

> **Why this is a separate step:** The Rust toolchain (`cargo`) is a hard build-time dependency for ITN. By making it opt-in, users who only need raw ASR don't need to install Rust. The Makefile targets auto-detect Apple Silicon vs Intel (`RUST_TARGET`) and clone the correct repo (`v0.2.2`).

> **If `make itn-build` fails or you skip it:** The audio sidecar still builds and runs, but ITN is silently disabled — `ENABLE_ITN=true` becomes a no-op. The sidecar logs a warning at startup.

### LLM Inference (sidecar-llm)

A separate Swift/Vapor sidecar (`sidecar-llm/`, port 8080) serves chat LLMs (Mistral 7B, Gemma 4 via coreml-llm, …), embeddings, and Stable Diffusion image generation on CoreML/ANE, speaking **OpenAI** (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/images/generations`, `/v1/models`) and **Anthropic** (`/v1/messages`) dialects natively. The Go server proxies those same paths with auth, rate limiting, and per-model token usage tracking — point unmodified OpenAI/Anthropic SDKs at the Go server. Management (download/load/unload/status) is proxied under `/api/v1/admin/llm/models/:id/*` (admin only).

**Build prerequisite — run once before `make llm-sidecar` / `make llm-build`:** sidecar-llm depends on a patched copy of [swift-embeddings](https://github.com/jkrukowski/swift-embeddings) (macOS 15 platform bump + `@preconcurrency` imports for Swift 6), vendored into `sidecar-llm/vendor/` like ITN's text-processing-rs:

```bash
make llm-vendor    # Clone swift-embeddings 0.1.0 + apply patches — first time only
make llm-sidecar   # Run on :8080
```

See [sidecar-llm/README.md](sidecar-llm/README.md) for details.

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
| `GET`  | `/v1/models` | OpenAI-compatible model listing (STT, TTS, LLM) |
| `POST` | `/v1/chat/completions` | OpenAI-compatible LLM chat (SSE streaming) |
| `POST` | `/v1/completions` | OpenAI-compatible legacy completions |
| `POST` | `/v1/embeddings` | OpenAI-compatible embeddings |
| `POST` | `/v1/images/generations` | OpenAI-compatible image generation |
| `POST` | `/v1/messages` | Anthropic-compatible messages (SSE streaming) |
| `WS`   | `/ws/asr` | Real-time streaming transcription |
| `WS`   | `/v1/listen` | Deepgram-compatible streaming transcription |
| `POST` | `/v1/recognize` | Watson-compatible file transcription |
| `WS`   | `/v1/recognize` | Watson-compatible streaming transcription |
| `POST` | `/api/v1/tts` | Synthesize speech from text |
| `POST` | `/api/v1/voices/clone` | Upload audio to create a stored cloned voice |
| `GET`  | `/api/v1/voices` | List custom + system voices |
| `GET`  | `/api/v1/voices/:id` | Get custom voice details |
| `DELETE` | `/api/v1/voices/:id` | Delete a custom voice |
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
| `GET`  | `/api/v1/admin/llm/models/:id/status` | LLM model status (admin) |
| `POST` | `/api/v1/admin/llm/models/:id/download` | Download LLM model (admin) |
| `POST` | `/api/v1/admin/llm/models/:id/load` | Load/warm LLM model (admin) |
| `POST` | `/api/v1/admin/llm/models/:id/unload` | Unload LLM model (admin) |

Full reference: [docs/api.md](docs/api.md)

---

## Pricing & Infrastructure

### Hardware Costs (One-Time)

| Config | Chip | RAM | Price (CAD) | Concurrent Streams | Use Case |
|--------|------|-----|-------|--------------------|----------|
| **Dev** | M4 | 16 GB / 256 GB | ~$799 | 3–5 | Development, 0.6B model |
| **Standard** | M4 | 24 GB / 512 GB | $1,399 | 5–8 | ASR + TTS + diarization |
| **Recommended** | M4 | 32 GB / 512 GB | ~$1,599 | 5–8 | Full stack with headroom |
| **Power Node** | M4 Pro | 48 GB | ~$1,899 | 8–12 | Heavy concurrent ASR + TTS (~50% more throughput vs base M4) |

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
│   └── sidecar/                  # HTTP/WS client for the audio sidecar
├── sidecar-audio/                # Audio inference sidecar (CoreML/ANE)
│   ├── Package.swift             # SPM manifest (Vapor + FluidAudio)
│   └── Sources/
│       ├── Server/
│       │   ├── main.swift        # Entry point
│       │   ├── EngineManager.swift   # Model lifecycle (ASR, VAD, diarizer, TTS)
│       │   ├── AudioConverter.swift  # Format detect + resample → 16 kHz mono PCM
│       │   └── Routes/               # Transcribe, Stream, VAD, Diarize, TTS, Health
│       └── ITNHelpers/           # Inverse text normalization (TextNormalizer)
├── sidecar-llm/                  # LLM inference sidecar (CoreML — chat, embeddings, images); vendor/ deps via `make llm-vendor`
│   ├── Package.swift             # SPM manifest (Vapor + swift-transformers + CoreML-LLM)
│   ├── models.json               # Model registry
│   ├── Sources/
│   │   ├── ModelRuntime/         # registry, HF downloader, compile cache, runner
│   │   ├── Tooling/              # Tool-call parser
│   │   ├── ImageRuntime/         # Stable Diffusion pipeline
│   │   ├── EmbeddingRuntime/     # swift-embeddings-backed embeddings
│   │   ├── ExternalRuntime/      # CoreML-LLM backend for bespoke chat repos
│   │   └── App/                  # Vapor routes, OpenAI + Anthropic DTOs, SSE
│   └── docs/                     # Setup, configuration, endpoints, operations
├── deploy/macos/                 # Headless node setup (launchd plists + guides)
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
| Load Balancer | Caddy / Nginx (multi-node) |

---

## Documentation

| Document | Description |
|----------|-------------|
| [docs/api.md](docs/api.md) | Full API reference: auth, ASR, TTS, voices, usage, admin, error format, rate limits |
| [docs/realtime.md](docs/realtime.md) | OpenAI Realtime API: transcription & speech-to-speech modes, latency budget, client-side tool calling |
| [docs/architecture.md](docs/architecture.md) | System design, data flow, model pipeline, memory layout, scaling strategy |
| [docs/setup.md](docs/setup.md) | Detailed setup, environment variables, deployment topologies, 16 GB Mac Mini notes |
| [docs/production.md](docs/production.md) | Multi-node production: Mac mini fleet + separate DB/Caddy VM, headless boot, scaling runbook |
| [docs/pricing.md](docs/pricing.md) | Pricing reference and cost model |
| [docs/cost-benefit-analysis.md](docs/cost-benefit-analysis.md) | ROI analysis: self-hosted vs cloud APIs |
| [docs/nvidia_cost_analysis.md](docs/nvidia_cost_analysis.md) | Mac Mini vs NVIDIA GPU cost & throughput comparison |

---

## License

GNU General Public License v3.0
