# ============================================================
# GoTranscribeSrv — Makefile
# ============================================================
# Quick reference (or run `make help`):
#
#   Dev (single machine):
#     make up              Postgres + Go server + Presidio (Docker)
#     make swift-sidecar   Swift inference sidecar, native :8101 (required)
#
#   Production (see docs/production.md):
#     make node-up         Mac mini node: server + Presidio
#     make sidecar-install Mac mini node: Swift sidecar auto-start via launchd
#     make node-migrate    One-shot DB migration (first boot, one node only)
#     make db-up           DB VM: Postgres + Caddy load balancer
#     make db-backup       DB VM: compressed pg_dump into ./backups/
#     make caddy-reload    Apply Caddyfile changes (add/remove nodes)
# ============================================================

.DEFAULT_GOAL := help

.PHONY: help \
        run build test migrate lint tidy \
        swift-sidecar swift-build swift-test \
        sidecar-install sidecar-restart sidecar-uninstall sidecar-status \
        itn-vendor itn-build itn-clean \
        up down logs \
        node-up node-down node-logs node-migrate \
        db-up db-down db-logs db-backup db-restore caddy-reload \
        presidio-pull presidio-up presidio-down presidio-logs presidio-shell \
        clean

# ---------- Config ----------
DEV_COMPOSE  := docker compose
NODE_COMPOSE := docker compose -f docker-compose.node.yml
DB_COMPOSE   := docker compose -f docker-compose.db.yml

# Presidio analyzer (PII redaction for logs). Runs inside the compose
# stacks; the presidio-* targets are for standalone/ad-hoc use only.
PRESIDIO_IMAGE := mcr.microsoft.com/presidio-analyzer:latest
PRESIDIO_PORT  := 5002
PRESIDIO_NAME  := gotranscribesrv-presidio

# Rust host target for vendored text-processing-rs (used by `make itn-build`).
# Override with `make itn-build RUST_TARGET=x86_64-apple-darwin` on Intel Macs.
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),arm64)
  RUST_TARGET ?= aarch64-apple-darwin
else
  RUST_TARGET ?= x86_64-apple-darwin
endif

ITN_VENDOR_DIR  := sidecar-swift/Vendor/text-processing-rs
ITN_VERSION     := v0.2.2
ITN_RELEASE     := $(ITN_VENDOR_DIR)/target/$(RUST_TARGET)/release/libtext_processing_rs.a
ITN_TEST_FILTER := TextNormalizerTests

# Swift sidecar launchd agent (production nodes — see deploy/macos/)
SIDECAR_LABEL           := com.gotranscribesrv.swift-sidecar
SIDECAR_PLIST           := deploy/macos/$(SIDECAR_LABEL).plist
SIDECAR_ROTATE_LABEL    := com.gotranscribesrv.swift-sidecar-logrotate
SIDECAR_ROTATE_PLIST    := deploy/macos/$(SIDECAR_ROTATE_LABEL).plist
SIDECAR_ROTATE_SCRIPT   := deploy/macos/rotate-sidecar-logs.sh
LAUNCHAGENTS    := $(HOME)/Library/LaunchAgents
GUI_DOMAIN      := gui/$(shell id -u)

# DB backup location (on the DB VM)
BACKUP_DIR := backups

# ---------- Help ----------
help:
	@echo ""
	@echo "  GoTranscribeSrv — make targets"
	@echo ""
	@echo "  Dev (single machine)"
	@echo "    up / down / logs    Postgres + Go server + Presidio (docker-compose.yml)"
	@echo "    swift-sidecar       Swift inference sidecar, native on :8101 (required)"
	@echo "    run / build / test  Go backend: run natively, build bin/server, run tests"
	@echo ""
	@echo "  Production — Mac mini nodes (docker-compose.node.yml)"
	@echo "    node-up             Build + start server + Presidio"
	@echo "    node-down           Stop node stack"
	@echo "    node-logs           Tail node logs"
	@echo "    node-migrate        One-shot DB migration (first boot, ONE node only)"
	@echo ""
	@echo "  Production — DB VM (docker-compose.db.yml)"
	@echo "    db-up / db-down     Start/stop Postgres + Caddy"
	@echo "    db-logs             Tail DB VM logs"
	@echo "    db-backup           pg_dump (compressed) into ./backups/"
	@echo "    db-restore          Restore: make db-restore FILE=backups/....dump"
	@echo "    caddy-reload        Zero-downtime reload after editing Caddyfile"
	@echo ""
	@echo "  Swift sidecar"
	@echo "    swift-build         Release build (.build/release/Server — used by launchd)"
	@echo "    swift-test          Sidecar tests (ITN)"
	@echo "    sidecar-install     Install launchd agents (auto-start at login, restart on crash, log rotation)"
	@echo "    sidecar-restart     Restart the launchd agent (e.g. after git pull + swift-build)"
	@echo "    sidecar-uninstall   Remove the launchd agent"
	@echo "    sidecar-status      launchd state + :8101 health check"
	@echo ""
	@echo "  ITN (optional Rust build — run BEFORE swift-build)"
	@echo "    itn-vendor          Clone text-processing-rs $(ITN_VERSION)"
	@echo "    itn-build           Build libtext_processing_rs.a ($(RUST_TARGET))"
	@echo "    itn-clean           Remove Rust build artifacts"
	@echo ""
	@echo "  Presidio (standalone — normally handled by compose)"
	@echo "    presidio-up/down    Start/stop analyzer on 127.0.0.1:$(PRESIDIO_PORT)"
	@echo "    presidio-logs       Tail analyzer logs"
	@echo ""
	@echo "  Utilities"
	@echo "    migrate             Run DB migrations and exit (native)"
	@echo "    lint / tidy         golangci-lint / go mod tidy"
	@echo "    clean               Remove bin/, sidecar-swift/.build, Go cache"
	@echo ""

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

tidy:
	go mod tidy

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

# ---------- Swift sidecar launchd agent (production nodes) ----------
# Auto-start the Swift sidecar at login (pair with auto-login for headless
# minis) and restart it on crash. See deploy/macos/README.md for the full
# headless setup (pmset, FileVault, auto-login).
sidecar-install: swift-build
	@echo ""
	@echo "  📦 Installing $(SIDECAR_LABEL) LaunchAgent"
	@echo "  ℹ  Repo path baked into plist: $(CURDIR)"
	@echo ""
	mkdir -p deploy/macos/logs $(LAUNCHAGENTS)
	sed 's|__REPO_PATH__|$(CURDIR)|g' $(SIDECAR_PLIST) > $(LAUNCHAGENTS)/$(SIDECAR_LABEL).plist
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_LABEL) 2>/dev/null
	launchctl bootstrap $(GUI_DOMAIN) $(LAUNCHAGENTS)/$(SIDECAR_LABEL).plist
	chmod +x $(SIDECAR_ROTATE_SCRIPT)
	sed 's|__REPO_PATH__|$(CURDIR)|g' $(SIDECAR_ROTATE_PLIST) > $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_ROTATE_LABEL) 2>/dev/null
	launchctl bootstrap $(GUI_DOMAIN) $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist
	@echo ""
	@echo "  ✅ Sidecar installed — starts now and at every login"
	@echo "  ℹ  Log rotation installed — hourly, 10m x 3 (mirrors Docker logging)"
	@echo "  ℹ  Verify: make sidecar-status"
	@echo "  ℹ  Logs:   deploy/macos/logs/"
	@echo ""

sidecar-restart:
	launchctl kickstart -k $(GUI_DOMAIN)/$(SIDECAR_LABEL)
	@echo "  ✅ Sidecar restarted"

sidecar-uninstall:
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_LABEL)
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_ROTATE_LABEL)
	rm -f $(LAUNCHAGENTS)/$(SIDECAR_LABEL).plist
	rm -f $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist
	@echo "  ✅ Sidecar LaunchAgents removed"

sidecar-status:
	@echo "  launchd (sidecar):"
	@out=$$(launchctl print $(GUI_DOMAIN)/$(SIDECAR_LABEL) 2>/dev/null | grep -E "state|pid|last exit"); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (not loaded)"; fi
	@echo "  launchd (logrotate):"
	@out=$$(launchctl print $(GUI_DOMAIN)/$(SIDECAR_ROTATE_LABEL) 2>/dev/null | grep -E "state|pid|last exit"); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (not loaded)"; fi
	@echo "  health:"
	@out=$$(curl -sf http://localhost:8101/health 2>/dev/null); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (no response on :8101)"; fi

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

# ---------- Dev: Docker (Postgres + Go server + Presidio) ----------
up:
	$(DEV_COMPOSE) up -d --build
	@echo ""
	@echo "  ✅ Postgres + Go server + Presidio running"
	@echo "  ℹ  API at http://localhost:3000"
	@echo "  ℹ  Run 'make swift-sidecar' in another terminal"
	@echo ""

down:
	$(DEV_COMPOSE) down

logs:
	$(DEV_COMPOSE) logs -f

# ---------- Production: Mac mini node (docker-compose.node.yml) ----------
# Runs on each Mac mini: Go server + Presidio in Docker, Swift sidecar
# native via launchd (see deploy/macos/). DB lives on the separate DB VM.
node-up:
	$(NODE_COMPOSE) up -d --build
	@echo ""
	@echo "  ✅ Node server + Presidio running"
	@echo "  ℹ  API at http://localhost:3000 (reachable by Caddy on the DB VM)"
	@echo "  ℹ  Swift sidecar is native — see deploy/macos/README.md"
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
db-up:
	$(DB_COMPOSE) up -d
	@echo ""
	@echo "  ✅ Postgres + Caddy running"
	@echo "  ℹ  First time? cp Caddyfile.example Caddyfile, list your node IPs"
	@echo "  ℹ  After Caddyfile edits: make caddy-reload"
	@echo ""

db-down:
	$(DB_COMPOSE) down

db-logs:
	$(DB_COMPOSE) logs -f

# Compressed pg_dump (custom format) into ./backups/ on the DB VM.
# Schedule with cron for automatic backups, e.g. nightly at 03:00:
#   0 3 * * * cd /path/to/gotranscribesrv && make db-backup
db-backup:
	@mkdir -p $(BACKUP_DIR)
	$(DB_COMPOSE) exec -T db sh -c 'pg_dump -U "$$POSTGRES_USER" -Fc "$$POSTGRES_DB"' \
		> $(BACKUP_DIR)/transcribesrv-$$(date +%Y%m%d-%H%M%S).dump
	@echo "  ✅ Backup written:"
	@ls -lh $(BACKUP_DIR)/*.dump | tail -1

# Restore from a backup file: make db-restore FILE=backups/transcribesrv-....dump
# WARNING: drops and recreates existing objects (--clean --if-exists).
db-restore:
	@test -n "$(FILE)" || { echo "  usage: make db-restore FILE=$(BACKUP_DIR)/transcribesrv-....dump"; exit 1; }
	@test -f "$(FILE)" || { echo "  ❌ file not found: $(FILE)"; exit 1; }
	$(DB_COMPOSE) exec -T db sh -c 'pg_restore -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" --clean --if-exists' < $(FILE)
	@echo "  ✅ Restore complete from $(FILE)"

# Zero-downtime reload after editing the Caddyfile (add/remove nodes).
caddy-reload:
	$(DB_COMPOSE) exec caddy caddy reload --config /etc/caddy/Caddyfile

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

# ---------- Utilities ----------
clean:
	rm -rf bin/ sidecar-swift/.build
	go clean
