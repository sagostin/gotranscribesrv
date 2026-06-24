# Setup Guide

## Quick Start (Docker + Native Sidecars)

The fastest way to get running. Docker handles PostgreSQL and the Go API server. The Swift and Python sidecars run natively on the Mac for CoreML/ANE access.

```bash
cp .env.example .env
# Edit .env: set JWT_SECRET and POSTGRES_PASSWORD

# Terminal 1 — Postgres + Go server
make up
docker compose logs -f server   # Watch for admin credentials

# Terminal 2 — Swift sidecar (ASR, VAD, diarization, TTS)
make swift-sidecar              # Builds & serves on :8101

# Terminal 3 — Python sidecar (LLM only, optional)
make setup                      # Download LLM models
make sidecar                    # Serves on :8100
```

This starts PostgreSQL and the Go backend in Docker, while the Swift sidecar runs natively for CoreML/ANE access. The Python sidecar is optional — only needed for LLM transcript processing. On first boot, an admin user and API key are printed to the console.

---

## Manual Setup

### Requirements

| Dependency | Version | Notes |
|-----------|---------|-------|
| macOS | 14 Sonoma+ | Apple Silicon required (M1/M2/M4) |
| Go | 1.22+ | |
| Swift | 6.0+ | Xcode 16+ (for FluidAudio / CoreML) |
| Python | 3.11+ | Only needed for LLM processing |
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

# Swift Sidecar (ASR, VAD, Diarization, TTS — CoreML/ANE)
SWIFT_SIDECAR_URL=http://localhost:8101
SWIFT_SIDECAR_WS_URL=ws://localhost:8101

# Python Sidecar (LLM only — MLX, optional)
LLM_SIDECAR_URL=http://localhost:8100

# Models (Swift sidecar auto-downloads on first run)
ENABLE_DIARIZATION=true
ENABLE_TTS=true

# Inverse Text Normalization (ITN) — converts spoken ASR output to
# written form (e.g. "one two five O" -> "1250"). On by default.
# Set to false to disable ITN across ALL ingress paths (REST + WS) for
# every request that doesn't pass an explicit per-request override.
# Per-request opt-out: pass ?itn=false (WS) or form itn=false (REST).
ENABLE_ITN=true

# LLM Processing (opt-in, requires ~4.5 GB extra RAM + Python sidecar)
ENABLE_LLM=false
LLM_MODEL=mlx-community/Meta-Llama-3.1-8B-Instruct-4bit

# PII Redaction in Logs (Loki + stdout only — response bodies are untouched).
# Requires the presidio-analyzer container (started automatically by `make up`).
# Adds ~700 MB RAM for spaCy en_core_web_lg. Disable with ENABLE_PII=false.
ENABLE_PII=true
PRESIDIO_ANALYZER_URL=http://localhost:5002    # If using docker-compose; see below for native setup
PRESIDIO_TIMEOUT_MS=3000
PII_ENTITIES=                                  # empty = use built-in default set
PII_SCORE_THRESHOLD=0.6

# Rate Limits
RATE_LIMIT_FREE=20       # requests/min
RATE_LIMIT_PRO=120
RATE_LIMIT_ENTERPRISE=0  # 0 = unlimited

# Logging
LOG_LEVEL=info

# ASR model (Swift sidecar auto-downloads)
ASR_MODEL=mlx-community/parakeet-tdt-0.6b-v3
```

> **Note on `PRESIDIO_ANALYZER_URL`:** When using `make up` (Docker Compose for Postgres + Go), the URL is `http://presidio-analyzer:3000` (the docker-compose service name). For manual / native setups, point to wherever you've started the Presidio container — typically `http://localhost:5002` (port mapping in our compose file).

---

### 3. Start the Swift Sidecar

The Swift sidecar handles all audio AI — ASR, VAD, diarization, and TTS — via CoreML and the Apple Neural Engine.

```bash
# Build and run (models auto-download on first launch):
make swift-sidecar
# 🚀 Initializing FluidAudio engines (CoreML/ANE)...
# ✅ ASR engine loaded (Parakeet TDT v3, ANE)
# ✅ VAD engine loaded (Silero, ANE)
# ✅ Diarizer loaded (Sortformer, ANE)
# ✅ TTS engine loaded (PocketTTS, ANE)
# 🎙  Swift sidecar listening on 0.0.0.0:8101
```

Verify it's running:
```bash
curl http://localhost:8101/health
# {"status":"ok","models":{"asr":"loaded","vad":"loaded","diarizer":"loaded","tts":"loaded"}}
```

**First build note:** The initial `swift build` may take a few minutes to compile Vapor and FluidAudio dependencies. Subsequent builds are fast (incremental).

**Model auto-download:** FluidAudio downloads CoreML models from HuggingFace on first launch (~2 GB total). Models are cached in `~/.cache/huggingface` and persist across restarts.

#### Optional: enable full NeMo ITN (spoken → written form)

By default, the Swift sidecar logs `📝 ITN: Swift fallback (libnemo_text_processing not linked — passthrough)`. Without the optional native library, ITN is a no-op — spoken numbers like "twelve dollars" pass through unchanged. To enable real NeMo ITN (98.6% compatibility with NVIDIA's NeMo test suite, 7 languages):

```bash
# One-time setup: builds a ~10 MB static lib from the Rust port of NeMo.
# Requires the Rust toolchain (brew install rust on macOS).
make itn-build    # ~5s incremental, ~30s cold
make swift-build  # relink the sidecar to pick up the lib
make swift-test   # 12 tests, including a real-NeMo smoke test
```

Verify the link took effect:

```bash
make swift-sidecar
# 📝 ITN: NeMo library loaded (version=text-processing-rs-0.2.2)
```

The link is **optional and graceful**: removing the `.a` file reverts the sidecar to passthrough mode without any code changes. Per-request opt-out is also available via `?itn=false` on the WS endpoints or `itn=false` form field on the REST endpoints.

#### Toggling ITN off globally

Set `ENABLE_ITN=false` in `.env` and restart the Go server. This propagates to **all five STT ingress paths** (REST + WS, all protocols) — for any request that doesn't pass an explicit `itn=true` override, ITN is bypassed end-to-end. Per-request `itn=true` still wins if a client wants to force it on for one request.

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

### 4. Start the Python Sidecar (LLM Only — Optional)

The Python sidecar is only needed if you want on-device LLM transcript processing (summarization, action items, translation). Skip this step if you don't need LLM features.

```bash
# Pre-download LLM models
make setup

# Start sidecar
make sidecar
# 🚀 Starting Python sidecar (LLM only — MLX)
# Serving on http://0.0.0.0:8100
```

Verify:
```bash
curl http://localhost:8100/health
# {"status":"ok","models":{"llm":"loaded"}}
```

---

### 5. Start the Go Backend

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

### 6. Quick Test

```bash
# Transcribe using the admin API key
curl -X POST http://localhost:3000/api/v1/asr \
  -H "X-API-Key: gtx_live_..." \
  -F "audio=@test.wav" | jq

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

Docker Compose runs PostgreSQL, the Go API server, and the Presidio PII analyzer. The Swift and Python sidecars run natively on the host for CoreML/ANE access.

| Service | Image | Port | Notes |
|---------|-------|------|-------|
| `db` | postgres:16-alpine | 5432 | Persistent volume `pgdata` |
| `server` | Custom (Go, alpine) | 3000 | Waits for healthy db + presidio-analyzer |
| `presidio-analyzer` | mcr.microsoft.com/presidio-analyzer:latest | 5002→3000 | spaCy `en_core_web_lg` + REST `/analyze`. Bundled with the analyzer; no model download on first start. `start_period: 60s` because spaCy cold-loads. |

The Go server connects to the native sidecars via `host.docker.internal`:
- Swift sidecar: `http://host.docker.internal:8101`
- Python sidecar: `http://host.docker.internal:8100`

The Presidio container is on the same docker network as `server`, so it uses `http://presidio-analyzer:3000` internally (no `host.docker.internal` needed). The `5002` host port is exposed only so you can `curl http://localhost:5002/health` for sanity-checking.

### Commands

```bash
# Start Postgres + Go server (run sidecars separately)
make up

# Start sidecars in separate terminals:
make swift-sidecar       # Terminal 2 — ASR, VAD, diarization, TTS
make sidecar             # Terminal 3 — LLM only (optional)

docker compose logs -f server   # Watch Go server logs (admin creds here)
docker compose down              # Stop Docker services
docker compose down -v           # Stop + delete volumes (⚠️ data loss)
```

---

## Multi-Node Deployment

There are two approaches for multi-node deployments:
- **Co-located** — Go + Swift + Python on every Mac Mini (simpler, all-in-one)
- **Split** — Go API on normal server infra, Macs as pure inference nodes (recommended)

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

Each Mac runs the Swift sidecar (and optionally the Python sidecar for LLM):

```bash
git clone https://github.com/yourorg/gotranscribesrv.git
cd gotranscribesrv

# Start Swift sidecar (models auto-download on first run)
make swift-sidecar       # ASR, VAD, diarization, TTS on :8101

# Optional: Start Python sidecar for LLM
make setup               # Download LLM models
make sidecar             # LLM processing on :8100
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
SWIFT_SIDECAR_URL=http://inference.internal:80       # Caddy proxy to Mac pool
SWIFT_SIDECAR_WS_URL=ws://inference.internal:80      # WebSocket via Caddy
LLM_SIDECAR_URL=http://inference.internal:80         # Optional
# Or with TLS:
# SWIFT_SIDECAR_URL=https://inference.internal:443
# SWIFT_SIDECAR_WS_URL=wss://inference.internal:443

DATABASE_URL=postgres://user:pass@db-host:5432/transcribesrv?sslmode=disable
JWT_SECRET=your-shared-secret
```

Start the server:
```bash
make run   # or: docker compose up server db
```

#### Architecture Diagram

```
  Clients → Go API (:3000)  → Caddy → Mac Mini 1 (Swift :8101, Py :8100)
              on VPS/K8s         ↗   → Mac Mini 2 (Swift :8101, Py :8100)
                           LB  ↗    → Mac Mini 3 (Swift :8101, Py :8100)
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

# Swift sidecar (ASR, VAD, Diarization, TTS — CoreML/ANE)
swift-sidecar:    # Build & run Swift sidecar on :8101
swift-build:      # Build Swift sidecar in release mode

# Python sidecar (LLM only — MLX)
sidecar:          # Start Python sidecar on :8100
venv:             # Create Python 3.11 venv + install deps
setup-models:     # Pre-download LLM models
setup:            # Create venv + download LLM models

# Docker (Postgres + Go server)
up:               # docker compose up -d --build
down:             # docker compose down
logs:             # docker compose logs -f
rebuild:          # docker compose up -d --build

# Utilities
clean:            # Remove bin/, .venv, sidecar-swift/.build
tidy:             # go mod tidy
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
│  Free (16GB, no LLM)          ~9.95 GB         │
│  Free (16GB, with LLM)        ~5.45 GB         │
└─────────────────────────────────────────────────┘
```

### What Works on 16 GB

| Configuration | Status | Notes |
|---------------|--------|-------|
| ASR only (0.6B Parakeet) | ✅ Works | 3–5 concurrent streams |
| ASR + VAD + Diarization | ✅ Works | 3–5 concurrent streams |
| ASR + TTS + Diarization | ✅ Works | Performance may degrade with TTS under load |
| PostgreSQL co-located | ✅ Works | Only if LLM is disabled |
| LLM (Llama 8B Q4) | ❌ OOM | Requires ~4.5 GB — too much for 16 GB with other services |

### Critical Constraints

1. **LLM must be disabled** (`ENABLE_LLM=false`). The Llama 8B Q4 model alone requires ~4.5 GB, which is incompatible with 16 GB when running the full stack.
2. **No co-located PostgreSQL in production** — offload the database to an external PostgreSQL host (RDS, Supabase, or a dedicated $5/mo VPS) to conserve ~1 GB.
3. **Reduce concurrent streams** — expect 3–5 concurrent streams instead of 5–8. Monitor the node; if `process_time / audio_duration` exceeds 0.5, the node is saturated.
4. **No headroom for spikes** — 16 GB has minimal free memory (~10 GB). A memory spike (e.g., multiple large audio files queued) can trigger OOM kills. Add a swap file as a safety net:

   ```bash
   # Create a 4 GB swap file (macOS)
   sudo hdiutil attach -shadow /private/var/vm/swapfile -stdinpass 4096
   ```

### Acceptable Criteria for 16 GB Deployment

A 16 GB Mac Mini M4 is acceptable if:

- [ ] `ENABLE_LLM=false` (LLM sidecar not running)
- [ ] PostgreSQL is hosted externally (not co-located)
- [ ] Expected concurrent load is ≤5 streams
- [ ] Audio file sizes are modest (<30 min per file; streaming is fine)
- [ ] You accept that TTS under concurrent load may degrade performance
- [ ] A swap file is configured as a safety net for memory spikes

### Recommended `.env` for 16 GB

```bash
# Disable LLM — critical for 16 GB
ENABLE_LLM=false

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
| Production, any LLM features | 24 GB minimum |
| Production, full stack | 32 GB |
| 5+ concurrent streams expected | 24 GB minimum |

> **TL;DR:** A single 16 GB Mac Mini M4 works for ASR + TTS + diarization with an external database. It does NOT work with LLM. If you need LLM features now or soon, order the 24 GB — the ~$200 premium is worth the headroom and avoids a second deployment cycle.

---

## Troubleshooting

| Issue | Solution |
|-------|---------|
| Swift build fails | Ensure Xcode 16+ and Swift 6.0+ (`swift --version`) |
| `CUDA not available` | Expected — we use CoreML/ANE, not CUDA |
| Swift model download slow | Models download from HuggingFace on first run (~2 GB). Set `HF_HUB_CACHE` to a fast SSD |
| `ASR engine not loaded` | Check Swift sidecar logs — CoreML model may have failed to download |
| Port 8101 in use | Change `AUDIO_SIDECAR_PORT` env var for Swift sidecar |
| Port 3000 in use | Change `PORT` in `.env` |
| PostgreSQL connection refused | Check `brew services list` for postgres status |
| Sidecar health check fails | Ensure Swift sidecar is running on port 8101 |
| Out of memory on 16 GB | Disable LLM (`ENABLE_LLM=false`) or use a smaller ASR model |
| Admin credentials lost | Delete all users from DB and restart — seed runs again |
| Python sidecar not needed | Python is only required for LLM processing — skip it if you only need ASR/TTS |
| `PocketTTS model not initialized` | Check Swift sidecar logs — TTS model may have failed to download |
