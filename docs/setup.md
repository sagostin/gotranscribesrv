# Setup Guide

## Quick Start (Docker)

The fastest way to get running:

```bash
cp .env.example .env
# Edit .env: set JWT_SECRET and POSTGRES_PASSWORD

docker compose up -d
docker compose logs -f server   # Watch for admin credentials
```

This starts PostgreSQL, the Python inference sidecar (with auto model download), and the Go backend. On first boot, an admin user and API key are printed to the console.

---

## Manual Setup

### Requirements

| Dependency | Version | Notes |
|-----------|---------|-------|
| macOS | 14 Sonoma+ | Apple Silicon required (M1/M2/M4) |
| Go | 1.22+ | |
| Python | 3.11 | 3.12+ not supported by nemo_toolkit yet |
| PostgreSQL | 15+ | Local or remote |
| RAM | 16 GB min | 24 GB recommended for 1.1B model |

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

# Python Sidecar
SIDECAR_URL=http://localhost:8100
SIDECAR_WS_URL=ws://localhost:8100

# Models
ASR_MODEL=mlx-community/parakeet-tdt-0.6b-v3    # Parakeet TDT on MLX
ASR_RUNTIME=mlx                        # mlx or coreml
ENABLE_DIARIZATION=true
ENABLE_TTS=true

# Rate Limits
RATE_LIMIT_FREE=20       # requests/min
RATE_LIMIT_PRO=120
```

---

### 3. Pre-Download Models & Voices

```bash
# Download all ML models (~3.5 GB total)
make setup-models

# Download and curate LibriTTS-R voice presets
make setup-voices
```

Or download selectively:
```bash
make venv
.venv/bin/python3 scripts/download_models.py --skip-tts    # ASR + diarizer + VAD only
```

**Model sizes:**

| Model | Size | First-Run Download |
|-------|------|--------------------||
| Parakeet TDT 1.1B | ~2.2 GB | ~2 min on fast connection |
| Parakeet TDT 0.6B | ~1.2 GB | ~1 min |
| LuxTTS | ~1.0 GB | ~1 min |
| Sortformer (diarization) | ~160 MB | ~15 sec |
| TitaNet (speaker embed) | ~46 MB | ~5 sec |
| Silero VAD | ~4 MB | instant |
| Voice presets (LibriTTS-R) | ~50 MB | via setup script |

---

### 4. Start the Python Sidecar

```bash
# The Makefile handles venv creation, dependency install, and LuxTTS clone:
make sidecar
# Serving on http://0.0.0.0:8100
# Models loaded in ~5s on M4
```

Verify it's running:
```bash
curl http://localhost:8100/health
# {"status": "ok", "models": {"asr": "loaded", "diarizer": "loaded", "tts": "loaded", "vad": "loaded"}}
```

---

### 5. Start the Go Backend

```bash
make dev        # Runs go mod tidy + starts server
# or manually:
go mod download
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
# {"status": "ok", "sidecar": "connected"}
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

| Service | Image | Port | Notes |
|---------|-------|------|-------|
| `db` | postgres:16-alpine | 5432 | Persistent volume `pgdata` |
| `sidecar` | Custom (Python 3.11) | 8100 | Model cache volume, 60s start period |
| `server` | Custom (Go, alpine) | 3000 | Waits for healthy db + sidecar |

### Commands

```bash
docker compose up -d          # Start all services
docker compose logs -f        # Watch all logs
docker compose logs -f server # Watch Go server only (admin creds here)
docker compose down           # Stop all services
docker compose down -v        # Stop + delete volumes (⚠️ data loss)
```

### Model Persistence

ML models are cached in a Docker volume (`model_cache` → `~/.cache/huggingface`). This means models are downloaded once and persist across container restarts.

---

## Multi-Node Deployment

### On Each Mac Mini

1. Clone the repo and follow steps 2–5 above
2. Point `DATABASE_URL` to the shared PostgreSQL host
3. Use the **same `JWT_SECRET`** across all nodes

### Load Balancer (Caddy Example)

Install Caddy on the load balancer host:

```bash
brew install caddy
```

Create `Caddyfile`:
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

```bash
caddy run
```

WebSocket connections are automatically proxied and remain sticky to the connected node.

---

## Makefile Reference

```makefile
run:            # Start Go backend
build:          # Build binary to bin/server
test:           # Run Go tests
migrate:        # Run GORM migrations only
lint:           # golangci-lint
venv:           # Create Python 3.11 venv + install deps
sidecar:        # Start Python sidecar (auto-creates venv)
setup-luxtts:   # Clone LuxTTS repo into sidecar/
setup-models:   # Pre-download all ML models
setup-voices:   # Download LibriTTS-R voice presets
setup:          # Clone LuxTTS + venv + models + voices
up:             # docker compose up -d
down:           # docker compose down
logs:           # docker compose logs -f
clean:          # Remove bin/ and .venv
dev:            # go mod tidy + run
```

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| `CUDA not available` | Expected — we use MLX/CoreML, not CUDA |
| Models downloading slowly | Set `HF_HUB_CACHE` to a fast SSD path |
| `mps not available` | Ensure macOS 14+ and PyTorch 2.1+ |
| Port 3000 in use | Change `PORT` in `.env` |
| PostgreSQL connection refused | Check `brew services list` for postgres status |
| Sidecar health check fails | Ensure Python sidecar is running on port 8100 |
| Out of memory on 16 GB | Use default 0.6B model or set `HF_HUB_CACHE` to a larger drive |
| Admin credentials lost | Delete all users from DB and restart — seed runs again |
| Docker sidecar slow to start | Normal — models load on first boot (60s start period) |
