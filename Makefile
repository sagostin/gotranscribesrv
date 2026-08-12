.PHONY: run build test migrate lint sidecar swift-sidecar swift-test swift-build \
        itn-build itn-vendor itn-clean clean setup-models setup-voices venv \
        up down logs rebuild \
        node-up node-down node-logs node-migrate db-up db-down db-logs caddy-reload \
        presidio-up presidio-down presidio-logs presidio-pull presidio-shell

# ---------- Config ----------
VENV_DIR  := sidecar/.venv
VENV_PY   := $(VENV_DIR)/bin/python3
VENV_PIP  := $(VENV_DIR)/bin/pip

# Presidio analyzer (PII redaction for logs). Pulled as a Docker image and
# started alongside the rest of the stack via `make up`. These targets exist
# for ad-hoc inspection / when running the Go server outside of compose.
PRESIDIO_IMAGE  := mcr.microsoft.com/presidio-analyzer:latest
PRESIDIO_PORT   := 5002
PRESIDIO_NAME   := gotranscribesrv-presidio

# Rust host target for vendored text-processing-rs (used by `make itn-build`).
# Override with `make itn-build RUST_TARGET=x86_64-apple-darwin` on Intel Macs.
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),arm64)
  RUST_TARGET ?= aarch64-apple-darwin
else
  RUST_TARGET ?= x86_64-apple-darwin
endif

ITN_VENDOR_DIR := sidecar-swift/Vendor/text-processing-rs
ITN_VERSION    := v0.2.2
ITN_RELEASE    := $(ITN_VENDOR_DIR)/target/$(RUST_TARGET)/release/libtext_processing_rs.a
ITN_TEST_FILTER := TextNormalizerTests

# ---------- Go backend ----------
run:
	go run cmd/server/main.go

build:
	go build -o bin/server cmd/server/main.go

test:
	go test ./...

migrate:
	go run cmd/server/main.go -migrate

lint:
	golangci-lint run ./...

# ---------- Swift sidecar (CoreML/ANE — ASR, VAD, Diarization, TTS) ----------
swift-sidecar:
	@echo ""
	@echo "  🚀 Starting Swift sidecar (CoreML/ANE)"
	@echo "  ℹ  Listening on http://localhost:8101"
	@echo "  ℹ  ASR, VAD, Diarization, TTS via FluidAudio"
	@echo ""
	cd sidecar-swift && swift run Server

swift-build:
	cd sidecar-swift && swift build -c release

swift-test:
	@echo ""
	@echo "  🧪 Running Swift sidecar tests (ITN, ...)"
	@echo ""
	cd sidecar-swift && swift test --filter $(ITN_TEST_FILTER)

# ---------- ITN (Inverse Text Normalization) — NeMo via text-processing-rs ----------
# Optional: builds the Rust static lib that FluidAudio's TextNormalizer dlsym()s
# at runtime. Without this build step, ITN is a no-op passthrough. With it, the
# sidecar gets full NeMo ITN/TN (EN, DE, ES, FR, HI, JA, ZH).
#
# Requires Rust toolchain (cargo). Install: `brew install rust` (Apple Silicon) or
# `rustup target add $(RUST_TARGET)` if using rustup.

itn-vendor:
	@if [ ! -f "$(ITN_VENDOR_DIR)/Cargo.toml" ]; then \
		echo "📥 Cloning text-processing-rs $(ITN_VERSION)..."; \
		rm -rf $(ITN_VENDOR_DIR); \
		mkdir -p sidecar-swift/Vendor; \
		git clone --depth 1 --branch $(ITN_VERSION) https://github.com/FluidInference/text-processing-rs.git $(ITN_VENDOR_DIR); \
	else \
		echo "✅ text-processing-rs already vendored at $(ITN_VENDOR_DIR)"; \
	fi

itn-build:
	@if [ ! -f "$(ITN_VENDOR_DIR)/Cargo.toml" ]; then \
		echo "❌ text-processing-rs not vendored at $(ITN_VENDOR_DIR)"; \
		echo "   Run: make itn-vendor"; \
		exit 1; \
	fi
	@echo ""
	@echo "  🔨 Building text-processing-rs static lib for $(RUST_TARGET)"
	@echo "  ℹ  Output: $(ITN_RELEASE)"
	@echo ""
	cd $(ITN_VENDOR_DIR) && \
		MACOSX_DEPLOYMENT_TARGET=14.0 cargo build --release --features ffi --target $(RUST_TARGET)
	@echo ""
	@echo "  ✅ Static lib built. Rebuild the swift sidecar to link it:"
	@echo "      make swift-build"
	@echo "  Run tests to verify:"
	@echo "      make swift-test"
	@echo ""

itn-clean:
	rm -rf $(ITN_VENDOR_DIR)/target

# ---------- Python venv ----------
venv: $(VENV_PY)

PYTHON    := $(shell command -v python3.11 || echo python3)

$(VENV_PY): sidecar/requirements.txt
	$(PYTHON) -m venv $(VENV_DIR)
	$(VENV_PIP) install --upgrade pip
	$(VENV_PIP) install -r sidecar/requirements.txt
	@touch $(VENV_PY)

# ---------- Python sidecar (LLM only — MLX) ----------
sidecar: venv
	@echo ""
	@echo "  🚀 Starting Python sidecar (LLM only — MLX)"
	@echo "  ℹ  Listening on http://localhost:8100"
	@echo ""
	cd sidecar && $(abspath $(VENV_PY)) main.py

# ---------- Setup scripts ----------
setup-models: venv
	cd sidecar && $(abspath $(VENV_PY)) ../scripts/download_models.py

setup-voices:
	@echo "Voices now managed by Swift sidecar (PocketTTS)"

setup: venv setup-models
	@echo "✅ LLM models ready. Swift sidecar downloads models on first run."

# ---------- Docker (Postgres + Go server) ----------
up:
	docker compose up -d --build
	@echo ""
	@echo "  ✅ Postgres + Go server running"
	@echo "  ℹ  API at http://localhost:3000"
	@echo "  ℹ  Run 'make swift-sidecar' and 'make sidecar' in other terminals"
	@echo ""

down:
	docker compose down

logs:
	docker compose logs -f

rebuild:
	docker compose up -d --build

# ---------- Production: Mac mini node (docker-compose.node.yml) ----------
# Runs on each Mac mini: Go server + Presidio in Docker, sidecars native
# via launchd (see deploy/macos/). DB lives on the separate DB VM.
NODE_COMPOSE := docker compose -f docker-compose.node.yml

node-up:
	$(NODE_COMPOSE) up -d --build
	@echo ""
	@echo "  ✅ Node server + Presidio running"
	@echo "  ℹ  API at http://localhost:3000 (reachable by Caddy on the DB VM)"
	@echo "  ℹ  Sidecars are native — see deploy/macos/README.md"
	@echo ""

node-down:
	$(NODE_COMPOSE) down

node-logs:
	$(NODE_COMPOSE) logs -f

# FIRST BOOT (and after upgrades): run migrations once, from ONE node,
# before starting the fleet.
node-migrate:
	$(NODE_COMPOSE) --profile migrate run --rm migrate

# ---------- Production: DB VM (docker-compose.db.yml) ----------
# Runs on the virtualized DB server: Postgres + Caddy load balancer.
DB_COMPOSE := docker compose -f docker-compose.db.yml

db-up:
	$(DB_COMPOSE) up -d
	@echo ""
	@echo "  ✅ Postgres + Caddy running"
	@echo "  ℹ  Edit Caddyfile with your node IPs, then 'make caddy-reload'"
	@echo ""

db-down:
	$(DB_COMPOSE) down

db-logs:
	$(DB_COMPOSE) logs -f

# Zero-downtime reload after editing the Caddyfile (add/remove nodes).
caddy-reload:
	$(DB_COMPOSE) exec caddy caddy reload --config /etc/caddy/Caddyfile

# ---------- Utilities ----------
clean:
	rm -rf bin/ $(VENV_DIR) sidecar-swift/.build
	go clean

tidy:
	go mod tidy

# ---------- Presidio (PII redaction) ----------
# These targets are for ad-hoc Presidio management — when running the Go
# server natively (not via `make up`), you'll want to start Presidio
# manually with `make presidio-up`. The `make up` path already includes
# Presidio via docker-compose, so these are mostly for debugging.

presidio-pull:
	docker pull $(PRESIDIO_IMAGE)

presidio-up: presidio-pull
	@echo ""
	@echo "  🚀 Starting Presidio analyzer (PII redaction)"
	@echo "  ℹ  Listening on http://localhost:$(PRESIDIO_PORT) → container :3000"
	@echo "  ℹ  Set PRESIDIO_ANALYZER_URL=http://localhost:$(PRESIDIO_PORT) in .env"
	@echo "  ℹ  Host port is bound to 127.0.0.1 only — LAN-reachable via docker-compose service name"
	@echo ""
	docker run -d --rm \
		--name $(PRESIDIO_NAME) \
		-p 127.0.0.1:$(PRESIDIO_PORT):3000 \
		$(PRESIDIO_IMAGE)

presidio-down:
	-docker rm -f $(PRESIDIO_NAME)

presidio-logs:
	docker logs -f $(PRESIDIO_NAME)

presidio-shell:
	docker exec -it $(PRESIDIO_NAME) /bin/bash
