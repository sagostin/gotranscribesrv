# Setup Guide

## Quick Start (Docker + Native Sidecar)

The fastest way to get running. Docker handles PostgreSQL and the Go API server. The audio sidecar runs natively on the Mac for CoreML/ANE access.

```bash
cp .env.example .env
# Edit .env: set JWT_SECRET and POSTGRES_PASSWORD

# Terminal 1 — Postgres + Go server
make up
docker compose logs -f server   # Watch for admin credentials

# Terminal 2 — Audio sidecar (ASR, VAD, diarization, TTS)
make audio-sidecar              # Builds & serves on :8101
```

This starts PostgreSQL and the Go backend in Docker, while the audio sidecar runs natively for CoreML/ANE access. On first boot, an admin user and API key are printed to the console.

---

## Manual Setup

### Requirements

| Dependency | Version | Notes |
|-----------|---------|-------|
| macOS | 14 Sonoma+ | Apple Silicon required (M1/M2/M4) |
| Go | 1.22+ | |
| Swift | 6.0+ | Xcode 16+ (for FluidAudio / CoreML) |
| PostgreSQL | 15+ | Local or remote |
| Docker | 24+ | **Required for the Presidio PII analyzer** (runs as a container in `docker-compose.yml`). Skip if you set `ENABLE_PII=false`. |
| RAM | 16 GB min | 24 GB recommended for full stack; see [16GB Mac Mini considerations](#16gb-mac-mini-considerations) |

---

### 1. PostgreSQL Setup

**Install (if needed):**
```bash
brew install postgresql@15
brew services start postgresql@15
```

**Create database and user:**
```bash
psql postgres <<SQL
  CREATE USER transcribesrv WITH PASSWORD 'your_secure_password';
  CREATE DATABASE transcribesrv OWNER transcribesrv;
SQL
```

---

### 2. Environment Configuration

```bash
cp .env.example .env
```

Edit `.env`:
```bash
# Server
PORT=3000
ENVIRONMENT=development

# Database (for manual setup, use localhost)
DATABASE_URL=postgres://transcribesrv:your_secure_password@localhost:5432/transcribesrv?sslmode=disable

# JWT — generate with: openssl rand -hex 32
JWT_SECRET=generate-a-random-64-char-string-here
JWT_ACCESS_TTL=15m
JWT_REFRESH_TTL=168h

# Audio Sidecar (ASR, VAD, Diarization, TTS — CoreML/ANE)
# Backward-compat: SWIFT_SIDECAR_URL / SWIFT_SIDECAR_WS_URL still work.
AUDIO_SIDECAR_URL=http://localhost:8101
AUDIO_SIDECAR_WS_URL=ws://localhost:8101

# Models (audio sidecar auto-downloads on first run)
ENABLE_DIARIZATION=true
ENABLE_TTS=true

# Inverse Text Normalization (ITN) — converts spoken ASR output to
# written form (e.g. "one two five O" -> "1250"). On by default.
# Set to false to disable ITN across ALL ingress paths (REST + WS) for
# every request that doesn't pass an explicit per-request override.
# Per-request opt-out: pass ?itn=false (WS) or form itn=false (REST).
ENABLE_ITN=true

# PII Redaction in Logs (Loki + stdout only — response bodies are untouched).
# Requires the presidio-analyzer container (started automatically by `make up`).
# Adds ~700 MB RAM for spaCy en_core_web_lg. Disable with ENABLE_PII=false.
ENABLE_PII=true
# Default targets the docker-compose service name. For native / non-compose
# setups (running the Go server directly via `go run`), start Presidio with
# `make presidio-up` and change this to:
#   PRESIDIO_ANALYZER_URL=http://localhost:5002
PRESIDIO_ANALYZER_URL=http://presidio-analyzer:3000
PRESIDIO_TIMEOUT_MS=3000
PII_ENTITIES=                                  # empty = use built-in default set
PII_SCORE_THRESHOLD=0.6

# Rate Limits
RATE_LIMIT_FREE=20       # requests/min
RATE_LIMIT_PRO=120
RATE_LIMIT_ENTERPRISE=0  # 0 = unlimited

# Logging
LOG_LEVEL=info

# ASR model (audio sidecar auto-downloads)
ASR_MODEL=mlx-community/parakeet-tdt-0.6b-v3
```

> **Note on `PRESIDIO_ANALYZER_URL`:** When using `make up` (Docker Compose for Postgres + Go), the default `http://presidio-analyzer:3000` works as-is — that's the Docker-internal DNS name for the `presidio-analyzer` service. For manual / native setups, run `make presidio-up` to start the container standalone and override to `http://localhost:5002` (the host-side port mapped to the container's `:3000`).

---

### 3. Start the Audio Sidecar

The audio sidecar handles all audio AI — ASR, VAD, diarization, and TTS — via CoreML and the Apple Neural Engine.

```bash
# Build and run (models auto-download on first launch):
make audio-sidecar
# 🚀 Initializing FluidAudio engines (CoreML/ANE)...
# ✅ ASR engine loaded (Parakeet TDT v3, ANE)
# ✅ VAD engine loaded (Silero, ANE)
# ✅ Diarizer loaded (Sortformer, ANE)
# ✅ TTS engine loaded (PocketTTS, ANE)
# 🎙  Audio sidecar listening on 0.0.0.0:8101
```

Verify it's running:
```bash
curl http://localhost:8101/health
# {"status":"ok","models":{"asr":"loaded","vad":"loaded","diarizer":"loaded","tts":"loaded","kokoro":"loaded"},
#  "config":{"synthesizeBackend":"pocket","streamBackend":"pocket"}}
```

**First build note:** The initial `swift build` may take a few minutes to compile Vapor and FluidAudio dependencies. Subsequent builds are fast (incremental).

**Model auto-download:** FluidAudio downloads CoreML models from HuggingFace on first launch (~2 GB total). Models are cached in `~/.cache/huggingface` and persist across restarts.

#### Optional: enable full NeMo ITN (spoken → written form)

By default, the audio sidecar logs `📝 ITN: Swift fallback (libnemo_text_processing not linked — passthrough)`. Without the optional native library, ITN is a no-op — spoken numbers like "twelve dollars" pass through unchanged. To enable real NeMo ITN (98.6% compatibility with NVIDIA's NeMo test suite, 7 languages):

```bash
# One-time setup: builds a ~10 MB static lib from the Rust port of NeMo.
# Requires the Rust toolchain (brew install rust on macOS).
make itn-build    # ~5s incremental, ~30s cold
make audio-build  # relink the sidecar to pick up the lib
make audio-test   # 12 tests, including a real-NeMo smoke test
```

Verify the link took effect:

```bash
make audio-sidecar
# 📝 ITN: NeMo library loaded (version=text-processing-rs-0.2.2)
```

The link is **optional and graceful**: removing the `.a` file reverts the sidecar to passthrough mode without any code changes. Per-request opt-out is also available via `?itn=false` on the WS endpoints or `itn=false` form field on the REST endpoints.

#### Toggling ITN off globally

Set `ENABLE_ITN=false` in `.env` and restart the Go server. This propagates to **all five STT ingress paths** (REST + WS, all protocols) — for any request that doesn't pass an explicit `itn=true` override, ITN is bypassed end-to-end. Per-request `itn=true` still wins if a client wants to force it on for one request.

#### TTS backend selection (Kokoro vs PocketTTS)

The sidecar ships two TTS backends. The default matrix is:

| Endpoint                              | Default backend | Why                                                                                     |
|---------------------------------------|-----------------|-----------------------------------------------------------------------------------------|
| `POST /api/v1/tts` (Go legacy)        | `pocket`        | Back-compat — `voice_id`/`voice_ref` cloning keep working.                               |
| `POST /synthesize` (sidecar)          | `pocket`        | Back-compat — same.                                                                      |
| `POST /synthesize/stream` (sidecar)   | `pocket` (locked) | Kokoro has no streaming API in FluidAudio 0.15.5 — `?backend=kokoro` returns 501.       |
| `POST /v1/audio/speech` (Go OpenAI)   | `kokoro`        | New endpoint; voice-agent clients automatically get higher-quality TTS.                  |

Override on the sidecar:

```bash
# Change the /synthesize default (Go /api/v1/tts is hardcoded to pocket for back-compat)
export SIDECAR_TTS_DEFAULT_BACKEND=kokoro   # or "pocket"

# /synthesize/stream backend (only "pocket" is honored — anything else logs a warning)
export SIDECAR_TTS_STREAM_BACKEND=pocket
```

Override on the Go server:

```bash
# /v1/audio/speech default (when no model is recognized)
export TTS_DEFAULT_BACKEND=kokoro   # or "pocket"
```

Verify the resolved values via the sidecar's `/health` (`config.synthesizeBackend` / `config.streamBackend`). For voice agents, the typical pattern is: **`/synthesize/stream` for snappy conversational replies** (PocketTTS, ~80 ms first chunk) and **`/v1/audio/speech?model=kokoro` for greetings / pre-rendered prompts** (Kokoro, full-utterance quality, multilingual). See `docs/api.md` § Text-to-Speech for the full matrix.

#### Real-time streaming ASR engine

The WS endpoints `/stream/realtime`, `/v2/listen` (Deepgram-compat realtime), and `/v1/realtime` (OpenAI-compat realtime) all use cache-aware streaming engines on the sidecar. Default is `eou-320` (Parakeet EOU 120M — best balance for English live agents). Override per session via `?engine=` query param, or globally via:

```bash
export SIDECAR_REALTIME_ENGINE=eou-320      # default; English + built-in turn-taking
# or
export SIDECAR_REALTIME_ENGINE=unified-320  # Parakeet Unified 0.6B — multilingual (25 EU langs)
export SIDECAR_REALTIME_ENGINE=nemotron-560 # Nemotron 0.6B — higher accuracy, English only
# Also valid: eou-160, eou-1280, nemotron-1120, unified-640, unified-1120
```

The Go proxies (`/v2/listen`, `/v1/realtime`) translate their respective protocol's model field (`model=nova-3`, `model=gpt-4o-realtime-preview`, …) into a sidecar `?engine=` value — see the engine table in `docs/api.md` § Real-Time Streaming ASR for the full mapping.

Verify the resolved value via the sidecar's `/health` (`config.realtimeEngine`). The realtime engines are downloaded lazily on first session connect — the first call to `/stream/realtime`, `/v2/listen`, or `/v1/realtime` with a previously-unused engine triggers a HuggingFace download (~100–800 MB per variant, cached under `~/.cache/fluidaudio/Models/`). If you switch `SIDECAR_REALTIME_ENGINE` after the initial setup, expect the first realtime session to spend a few minutes downloading the new engine before `ready` arrives.

#### Models, formats & sample rates at a glance

| Surface                       | Models / engines                                                                 | Input formats                                   | Output formats            | Sample rates |
|-------------------------------|-----------------------------------------------------------------------------------|-------------------------------------------------|---------------------------|--------------|
| `POST /api/v1/asr`            | Parakeet TDT v3 (batch)                                                           | multipart `audio` upload — mp3/wav/opus/flac/m4a/ogg/… | JSON (text + words + diar) | resampled to 16k internally |
| `POST /v1/audio/transcriptions` (OpenAI) | Parakeet TDT v3 (batch)                                                           | multipart `file` upload                          | JSON (text + words + diar) | resampled to 16k internally |
| `WS /ws/asr`, `/v1/listen`, `/v1/recognize` | Parakeet TDT v3 (buffered; full-buffer re-transcribe every ~2s)                  | PCM16/mulaw/alaw binary frames                   | JSON events (partial/final) | 8k upsampled to 16k, or native 16k |
| `WS /stream/realtime`, `/v2/listen`, `/v1/realtime` | EOU/Nemotron/Unified streaming (8 variants) — see engine table                   | PCM16/mulaw/alaw binary frames                   | JSON events (partial/final/end_of_turn/speech_*); OpenAI/Deepgram on WS proxies | 8k upsampled to 16k, or native 16k |
| `POST /synthesize` (sidecar)  | PocketTTS (default) or Kokoro via `?backend=`                                     | JSON `{text, voice, voice_id?, voice_ref?}`      | WAV (24 kHz mono PCM)      | n/a |
| `POST /synthesize/stream`     | PocketTTS only                                                                     | JSON `{text, voice}`                            | L16 raw (24 kHz mono, 80ms chunks) | n/a |
| `POST /api/v1/tts` (Go)       | PocketTTS only (back-compat)                                                       | JSON `{text, voice, voice_id?, voice_ref?}`      | WAV (24 kHz mono PCM)      | n/a |
| `POST /v1/audio/speech` (Go OpenAI) | PocketTTS or Kokoro via `model=` (default: Kokoro via `TTS_DEFAULT_BACKEND`)     | JSON `{model, voice, input, response_format, speed}` | WAV or raw L16 (24 kHz mono) | n/a |
| `POST /api/v1/voices/clone`   | PocketTTS embedding extraction                                                     | multipart `audio` upload                         | binary embedding           | resampled to 16k |
| `POST /vad`                   | Silero VAD v6.2.1 (streaming-capable)                                              | multipart `audio` upload                         | JSON segments              | resampled to 16k |
| `POST /diarize`               | Sortformer v2.1 (4 speakers max)                                                   | multipart `audio` upload                         | JSON segments (speaker_idx, start, end) | resampled to 16k |

**Format gotchas:**
- ASR input: any audio format that `SidecarAudioConverter.toPCM16kMono()` handles (decodes to 16 kHz mono internally). Container is irrelevant — only the audio codec matters.
- ASR output: always 16 kHz mono; word timings are relative to that.
- TTS output: always 24 kHz mono, 16-bit PCM. WAV is full header; `pcm` strips it.
- Streaming TTS (`/synthesize/stream`): raw 16-bit little-endian LE Int16 frames at 24 kHz mono, 80 ms each (1920 samples per frame). Wrap in a WAV header yourself if you need one.
- TTS cloning (`voice_id` / `voice_ref`): **pocket only**. `?backend=kokoro` + cloning → 422.

#### Debug logs

Every transcription (REST + WS final) logs the original ASR text and the ITN-converted text side by side, so you can verify what's happening:

```
ITN [ne] text: "one two five O" -> "1250"
ITN [ne] words (2): 'one'->'1', 'O'->'0'
ITN [ne] final: "I paid five dollars for twenty three items" -> "I paid $5 for 23 items"
```

The tag in brackets shows which engine ran:
- `ne` — NeMo via libnemo_text_processing (after `make itn-build`)
- `swift-passthrough` — Swift fallback (lib not linked; output == input)
- `ITN: disabled for this request/session` — ITN was turned off via env or per-request override

WS partials log at debug level (they fire every ~3s); REST + WS final events log at info level. Set `LOG_LEVEL=debug` in `.env` to see the partials.

---

### 4. Start the Go Backend

```bash
make run        # Starts Go backend (cmd/server/main.go)
# or manually:
go run cmd/server/main.go
```

On first boot, the server creates an admin user and prints credentials:
```
══════════════════════════════════════════════════
  ✅ Admin user created
  │ Email:    admin@gotranscribesrv.local
  │ Password: aX9#kL2mP4qR5sT7uV8w   (random)
  │ Tier:     enterprise
  │
  │ API Key:  gtx_live_a4b8c2d1e5f6...
══════════════════════════════════════════════════
  ⚠️  Save these credentials now — they are shown only once!
```

Verify:
```bash
curl http://localhost:3000/health
# {"status":"ok","models":{"asr":"loaded","vad":"loaded","diarizer":"loaded","tts":"loaded"}}
```

---

### 5. Presidio (PII Redaction) {#presidio-setup}

The PII redactor runs as a separate container. There are two setup paths; pick the one that matches how you're running the Go backend.

#### Path A — Docker Compose (`make up`) — recommended

`docker-compose.yml` already declares the `presidio-analyzer` service. No extra steps:

```bash
docker compose up -d --build
docker compose ps                # Both 'server' and 'presidio-analyzer' should be running
docker compose logs -f presidio-analyzer
# ...wait for "Application startup complete."
```

Confirm the analyzer is healthy:

```bash
curl http://localhost:5002/health
# {"status":"ok"}

# Smoke-test the /analyze endpoint with a PII-bearing string:
curl -X POST http://localhost:5002/analyze \
  -H "Content-Type: application/json" \
  -d '{"text": "Call John Smith at 212-555-1234 or email john@example.com", "language": "en"}'
# Should return a JSON array with PERSON, PHONE_NUMBER, EMAIL_ADDRESS spans.
```

> **Why `localhost:5002` works:** The host port is bound to `127.0.0.1` only in `docker-compose.yml` — see the comment there. The Go server in `server` reaches the analyzer via the Docker-internal DNS name `http://presidio-analyzer:3000` (NOT the host port). The host-published port exists only for ad-hoc debugging from the host.

#### Path B — Native (Go server running directly, not in compose)

Run Presidio as a standalone container, then point the Go server at it:

```bash
make presidio-up       # Pulls and starts the container on 127.0.0.1:5002
docker ps --filter name=gotranscribesrv-presidio
curl http://localhost:5002/health
```

Override the URL in `.env`:

```bash
PRESIDIO_ANALYZER_URL=http://localhost:5002
```

Then start (or restart) the Go server as in section 5.

#### Disabling PII redaction

Set `ENABLE_PII=false` in `.env` and restart the server. Useful for local debugging when you want to see raw transcript text in the logs. Response bodies are unchanged either way.

#### Disabling PII redaction when Presidio is unreachable (fail-closed behavior)

The redactor is **fail-closed by design**: if Presidio is unreachable, returns an error, or times out (default 3s), the affected log field is replaced with the literal string `<REDACTED-ERROR>` and a separate `PII_REDACTOR_ERROR` warning event is emitted. The HTTP response body is **never** affected — clients always get the raw transcript. You'll see this reflected in Prometheus as `gotranscribesrv_pii_errors_total{reason="analyzer_error"}` incrementing.

**Practical implications:**
- A down Presidio adds ~3s of latency per ASR request (the timeout). Consider raising `PRESIDIO_TIMEOUT_MS` only if your deployment tolerates this.
- No request is rejected — the only externally visible effect is slower responses and a sentinel in the log.
- Search Loki for `transcript="<REDACTED-ERROR>"` to find requests whose logs were affected.

#### Multi-node / centralized Presidio

For HA or to share one Presidio across multiple Go servers, override `PRESIDIO_ANALYZER_URL` to point at an external endpoint:

```bash
PRESIDIO_ANALYZER_URL=https://presidio.internal.company.com
```

Then remove the local `presidio-analyzer` service from your compose file (or simply don't `make presidio-up` on the nodes). See [docs/api.md → PII Redaction](api.md#pii-redaction) for the privacy tradeoff of centralization.

---

### 6. Quick Test

```bash
# Transcribe using the admin API key
curl -X POST http://localhost:3000/api/v1/asr \
  -H "X-API-Key: gtx_live_..." \
  -F "audio=@test.wav" | jq

# Confirm the log field was PII-redacted (transcript contains <PERSON>, <PHONE_NUMBER>, etc.)
#   tail /var/log/gotranscribesrv/server.log | grep ASR_COMPLETED | jq '.additional_data.transcript'
# Should NOT contain raw names, phone numbers, emails, etc.

# Create a customer (admin only)
curl -X POST http://localhost:3000/api/v1/admin/users \
  -H "X-API-Key: gtx_live_..." \
  -H "Content-Type: application/json" \
  -d '{"email": "customer@co.com", "password": "securepass", "tier": "pro"}' | jq

# Check usage
curl -s http://localhost:3000/api/v1/usage/summary \
  -H "X-API-Key: gtx_live_..." | jq
```

---

## Docker Compose Deployment

### Services

Docker Compose runs PostgreSQL, the Go API server, and the Presidio PII analyzer. The audio sidecar runs natively on the host for CoreML/ANE access.

| Service | Image | Port | Notes |
|---------|-------|------|-------|
| `db` | postgres:16-alpine | 5432 | Persistent volume `pgdata` |
| `server` | Custom (Go, alpine) | 3000 | Waits for healthy db + presidio-analyzer |
| `presidio-analyzer` | mcr.microsoft.com/presidio-analyzer:latest | 5002→3000 | spaCy `en_core_web_lg` + REST `/analyze`. Bundled with the analyzer; no model download on first start. `start_period: 60s` because spaCy cold-loads. |

The Go server connects to the native audio sidecar via `host.docker.internal`:
- Audio sidecar: `http://host.docker.internal:8101`

The Presidio container is on the same docker network as `server`, so it uses `http://presidio-analyzer:3000` internally (no `host.docker.internal` needed). The `5002` host port is exposed only so you can `curl http://localhost:5002/health` for sanity-checking.

### Commands

```bash
# Start Postgres + Go server (run the sidecar separately)
make up

# Start the audio sidecar in a separate terminal:
make audio-sidecar       # Terminal 2 — ASR, VAD, diarization, TTS

docker compose logs -f server   # Watch Go server logs (admin creds here)
docker compose down              # Stop Docker services
docker compose down -v           # Stop + delete volumes (⚠️ data loss)
```

---

## Multi-Node Deployment

There are two approaches for multi-node deployments:
- **Co-located** — Go + audio sidecar on every Mac Mini (simpler, all-in-one)
- **Split** — Go API on normal server infra, Macs as pure inference nodes (recommended)

For a complete production walkthrough of the co-located approach (compose files, headless boot, shared DB VM with Caddy), see [docs/production.md](production.md).

### Option A: Co-Located (All-in-One on Every Mac)

**On each Mac Mini:**

1. Clone the repo and follow steps 2–5 above
2. Point `DATABASE_URL` to the shared PostgreSQL host
3. Use the **same `JWT_SECRET`** across all nodes

**Load Balancer (Caddy):**

```
transcribe.yourcompany.com {
    reverse_proxy {
        to mini1.local:3000 mini2.local:3000 mini3.local:3000
        lb_policy least_conn
        health_uri /health
        health_interval 10s
    }
}
```

---

### Option B: Split Deployment (Recommended) {#split-deployment}

Run the Go API gateway on your normal server infrastructure and keep Macs as dedicated inference nodes. No code changes — just config.

#### 1. Mac Minis (Inference Only)

Each Mac runs the audio sidecar:

```bash
git clone https://github.com/yourorg/gotranscribesrv.git
cd gotranscribesrv

# Start audio sidecar (models auto-download on first run)
make audio-sidecar       # ASR, VAD, diarization, TTS on :8101
```

Verify each Mac is serving:
```bash
curl http://mini1.local:8101/health
# {"status":"ok","models":{"asr":"loaded","vad":"loaded","diarizer":"loaded","tts":"loaded"}}
```

#### 2. Caddy Reverse Proxy (Inference Load Balancer)

Install Caddy on a machine reachable by both the Go server and the Macs:

```bash
brew install caddy   # or apt install caddy
```

Create `Caddyfile`:

```
inference.internal {
    reverse_proxy mini1.local:8101 mini2.local:8101 mini3.local:8101 {
        lb_policy round_robin
        health_uri /health
        health_interval 10s
    }
}
```

Start Caddy:
```bash
caddy run
```

Caddy automatically handles:
- Load balancing across Mac pool
- Health checks — removes unhealthy Macs from the pool
- WebSocket proxying (for streaming ASR)
- TLS (optional, automatic with a domain)

#### 3. Go API Server (Normal Server Infra)

Run the Go server anywhere — Docker, K8s, VPS, bare metal:

```bash
# .env on the Go server
AUDIO_SIDECAR_URL=http://inference.internal:80       # Caddy proxy to Mac pool
AUDIO_SIDECAR_WS_URL=ws://inference.internal:80      # WebSocket via Caddy
# Or with TLS:
# AUDIO_SIDECAR_URL=https://inference.internal:443
# AUDIO_SIDECAR_WS_URL=wss://inference.internal:443

DATABASE_URL=postgres://user:pass@db-host:5432/transcribesrv?sslmode=disable
JWT_SECRET=your-shared-secret
```

Start the server:
```bash
make run   # or: docker compose up server db
```

#### Architecture Diagram

```
  Clients → Go API (:3000)  → Caddy → Mac Mini 1 (Audio :8101)
              on VPS/K8s         ↗   → Mac Mini 2 (Audio :8101)
                           LB  ↗    → Mac Mini 3 (Audio :8101)
                    ↓
              PostgreSQL
```

WebSocket connections are automatically proxied and remain sticky to the connected node.

---

## Makefile Reference

```makefile
# Go backend
run:              # Start Go backend
build:            # Build binary to bin/server
test:             # Run Go tests
migrate:          # Run GORM migrations only
lint:             # golangci-lint
tidy:             # go mod tidy

# Audio sidecar (ASR, VAD, Diarization, TTS — CoreML/ANE)
audio-sidecar:    # Build & run audio sidecar on :8101
audio-build:      # Build audio sidecar in release mode
audio-test:       # Sidecar tests (ITN)
sidecar-install:  # Install audio launchd agent (auto-start at login — prod nodes)
sidecar-restart:  # Restart the audio launchd agent
sidecar-uninstall:# Remove the audio launchd agents
sidecar-status:   # launchd state + :8101 health check

# LLM sidecar (chat, embeddings, image generation — CoreML)
llm-sidecar:      # Build & run LLM sidecar on :8080
llm-build:        # Build LLM sidecar in release mode
llm-install:      # Install LLM launchd agent (auto-start at login — prod nodes)
llm-restart:      # Restart the LLM launchd agent
llm-uninstall:    # Remove the LLM launchd agent
llm-status:       # launchd state + :8080 health check

# ITN (optional Rust build — run BEFORE audio-build)
itn-vendor:       # Clone text-processing-rs
itn-build:        # Build libtext_processing_rs.a
itn-clean:        # Remove Rust build artifacts

# Dev Docker (Postgres + Go server + Presidio)
up:               # docker compose up -d --build
down:             # docker compose down
logs:             # docker compose logs -f

# Production (multi-node — see docs/production.md)
node-up:          # Mac mini node: server + Presidio (docker-compose.node.yml)
node-down:        # Stop node stack
node-logs:        # Tail node logs
node-migrate:     # One-shot DB migration from a node
db-up:            # DB VM: Postgres + Caddy (docker-compose.db.yml)
db-down:          # Stop DB VM stack
db-logs:          # Tail DB VM logs
db-backup:        # pg_dump (compressed) into ./backups/
db-restore:       # Restore: make db-restore FILE=backups/....dump
caddy-reload:     # Zero-downtime Caddy reload after Caddyfile edits

# Utilities
clean:            # Remove bin/, sidecar-audio/.build, sidecar-llm/.build
help:             # List all targets (also the default: bare `make`)
```

---

## 16GB Mac Mini M4 Considerations

A single 16 GB Mac Mini M4 can run GoTranscribeSrv, but with important constraints. This section outlines the trade-offs, pitfalls, and acceptable criteria for deploying on 16 GB.

### Memory Layout (16 GB M4)

```
┌─────────────────────────────────────────────────┐
│  Unified Memory — 16 GB                         │
│                                                 │
│  macOS + system              ~3.5 GB           │
│  Parakeet TDT v3 (ASR)       ~1.2 GB           │
│  PocketTTS (TTS)              ~0.5 GB           │
│  Sortformer (diarization)     ~0.2 GB           │
│  Silero VAD                   ~0.05 GB          │
│  Swift runtime               ~0.2 GB           │
│  Go runtime                   ~0.1 GB           │
│  Audio buffers               ~0.3 GB           │
│  ─────────────────────────────────              │
│  Free (16GB)                  ~9.95 GB         │
└─────────────────────────────────────────────────┘
```

### What Works on 16 GB

| Configuration | Status | Notes |
|---------------|--------|-------|
| ASR only (0.6B Parakeet) | ✅ Works | 3–5 concurrent streams |
| ASR + VAD + Diarization | ✅ Works | 3–5 concurrent streams |
| ASR + TTS + Diarization | ✅ Works | Performance may degrade with TTS under load |
| PostgreSQL co-located | ✅ Works | |

### Critical Constraints

1. **No co-located PostgreSQL in production** — offload the database to an external PostgreSQL host (RDS, Supabase, or a dedicated $5/mo VPS) to conserve ~1 GB.
2. **Reduce concurrent streams** — expect 3–5 concurrent streams instead of 5–8. Monitor the node; if `process_time / audio_duration` exceeds 0.5, the node is saturated.
3. **No headroom for spikes** — 16 GB has minimal free memory (~10 GB). A memory spike (e.g., multiple large audio files queued) can trigger OOM kills. Add a swap file as a safety net:

   ```bash
   # Create a 4 GB swap file (macOS)
   sudo hdiutil attach -shadow /private/var/vm/swapfile -stdinpass 4096
   ```

### Acceptable Criteria for 16 GB Deployment

A 16 GB Mac Mini M4 is acceptable if:

- [ ] PostgreSQL is hosted externally (not co-located)
- [ ] Expected concurrent load is ≤5 streams
- [ ] Audio file sizes are modest (<30 min per file; streaming is fine)
- [ ] You accept that TTS under concurrent load may degrade performance
- [ ] A swap file is configured as a safety net for memory spikes

### Recommended `.env` for 16 GB

```bash
# External PostgreSQL (not on this node)
DATABASE_URL=postgres://user:pass@your-external-pg-host:5432/transcribesrv?sslmode=disable

# Optional: Reduce model memory footprint
ASR_MODEL=mlx-community/parakeet-tdt-0.6b-v3  # default, already smallest
ENABLE_TTS=true   # TTS works but monitor under concurrent load
ENABLE_DIARIZATION=true
```

### When to Choose 16 GB vs 24 GB

| Scenario | Recommended Config |
|----------|-------------------|
| Dev/staging, solo | 16 GB — cost-effective |
| Production, ASR + TTS only | 16 GB — acceptable with external DB |
| Production, full stack | 24–32 GB |
| 5+ concurrent streams expected | 24 GB minimum |

> **TL;DR:** A single 16 GB Mac Mini M4 works for ASR + TTS + diarization with an external database.

---

## Troubleshooting

| Issue | Solution |
|-------|---------|
| Swift build fails | Ensure Xcode 16+ and Swift 6.0+ (`swift --version`) |
| `CUDA not available` | Expected — we use CoreML/ANE, not CUDA |
| Audio model download slow | Models download from HuggingFace on first run (~2 GB). Set `HF_HUB_CACHE` to a fast SSD |
| `ASR engine not loaded` | Check audio sidecar logs — CoreML model may have failed to download |
| Port 8101 in use | Change `AUDIO_SIDECAR_PORT` env var for the audio sidecar |
| Port 8080 in use | Change `PORT` env var for the LLM sidecar (or override `LLM_SIDECAR_PORT` in the Makefile) |
| Port 3000 in use | Change `PORT` in `.env` |
| PostgreSQL connection refused | Check `brew services list` for postgres status |
| Sidecar health check fails | Ensure audio sidecar is running on port 8101 |
| Out of memory on 16 GB | Move PostgreSQL to an external host, reduce concurrent streams |
| Admin credentials lost | Delete all users from DB and restart — seed runs again |
| `PocketTTS model not initialized` | Check audio sidecar logs — TTS model may have failed to download |
