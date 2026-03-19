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
| RAM | 16 GB min | 24 GB recommended for full stack |

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

# LLM Processing (opt-in, requires ~4.5 GB extra RAM + Python sidecar)
ENABLE_LLM=false
LLM_MODEL=mlx-community/Meta-Llama-3.1-8B-Instruct-4bit

# Rate Limits
RATE_LIMIT_FREE=20       # requests/min
RATE_LIMIT_PRO=120
RATE_LIMIT_ENTERPRISE=0  # 0 = unlimited

# Logging
LOG_LEVEL=info

# ASR model (Swift sidecar auto-downloads)
ASR_MODEL=mlx-community/parakeet-tdt-0.6b-v3
```

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

Docker Compose runs PostgreSQL and the Go API server. Both sidecars run natively on the host for CoreML/ANE access.

| Service | Image | Port | Notes |
|---------|-------|------|-------|
| `db` | postgres:16-alpine | 5432 | Persistent volume `pgdata` |
| `server` | Custom (Go, alpine) | 3000 | Waits for healthy db |

The Go server connects to the native sidecars via `host.docker.internal`:
- Swift sidecar: `http://host.docker.internal:8101`
- Python sidecar: `http://host.docker.internal:8100`

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

## Troubleshooting

| Issue | Solution |
|-------|----------|
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
