# ============================================================
# GoTranscribeSrv — Makefile
# ============================================================
# Quick reference (or run `make help`):
#
#   Dev (single machine):
#     make up              Postgres + Go server + Presidio (Docker)
#     make audio-sidecar   Audio inference sidecar, native :8101 (required)
#     make llm-sidecar     LLM inference sidecar, native :8080 (optional)
#
#   Production (see docs/production.md):
#     make node-up         Mac mini node: server + Presidio
#     make sidecar-install Mac mini node: audio sidecar auto-start via launchd
#     make llm-install     Mac mini node: LLM sidecar auto-start via launchd
#     make node-migrate    One-shot DB migration (first boot, one node only)
#     make db-up           DB VM: Postgres + Caddy load balancer
#     make db-backup       DB VM: compressed pg_dump into ./backups/
#     make caddy-reload    Apply Caddyfile changes (add/remove nodes)
# ============================================================

.DEFAULT_GOAL := help

.PHONY: help \
        run build test migrate lint tidy \
        audio-sidecar audio-build audio-test \
        llm-vendor llm-sidecar llm-build llm-install llm-restart llm-uninstall llm-status \
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

ITN_VENDOR_DIR  := sidecar-audio/Vendor/text-processing-rs
ITN_VERSION     := v0.2.2
ITN_RELEASE     := $(ITN_VENDOR_DIR)/target/$(RUST_TARGET)/release/libtext_processing_rs.a
ITN_TEST_FILTER := TextNormalizerTests

# Audio sidecar launchd agent (production nodes — see deploy/macos/).
# The legacy `com.gotranscribesrv.swift-sidecar` label is kept during the
# rename transition so existing deployments don't break; both plists are
# installed by `make sidecar-install` and point at the same binary.
SIDECAR_LABEL           := com.gotranscribesrv.audio-sidecar
SIDECAR_PLIST           := deploy/macos/$(SIDECAR_LABEL).plist
SIDECAR_LEGACY_LABEL    := com.gotranscribesrv.swift-sidecar
SIDECAR_LEGACY_PLIST    := deploy/macos/$(SIDECAR_LEGACY_LABEL).plist
SIDECAR_ROTATE_LABEL    := com.gotranscribesrv.swift-sidecar-logrotate
SIDECAR_ROTATE_PLIST    := deploy/macos/$(SIDECAR_ROTATE_LABEL).plist
SIDECAR_ROTATE_SCRIPT   := deploy/macos/rotate-sidecar-logs.sh

# LLM sidecar vendored dependency (mirrors itn-vendor). sidecar-llm itself is
# committed to this monorepo, but its `vendor/swift-embeddings` package is a
# patched copy of upstream (macOS 15 platform bump + @preconcurrency imports
# for Swift 6) and is NOT committed — `make llm-vendor` clones upstream and
# re-applies the patches. Override the version for testing:
#   make llm-vendor EMBEDDINGS_VERSION=0.2.0
EMBEDDINGS_VENDOR_DIR := sidecar-llm/vendor/swift-embeddings
EMBEDDINGS_REPO       ?= https://github.com/jkrukowski/swift-embeddings.git
EMBEDDINGS_VERSION    ?= 0.1.0

# LLM sidecar launchd agent.
LLM_SIDECAR_LABEL       := com.gotranscribesrv.llm-sidecar
LLM_SIDECAR_PLIST       := deploy/macos/$(LLM_SIDECAR_LABEL).plist
LLM_SIDECAR_PORT        := 8080

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
	@echo "    audio-sidecar       Audio inference sidecar, native on :8101 (required)"
	@echo "    llm-sidecar         LLM inference sidecar, native on :8080 (optional)"
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
	@echo "  Audio sidecar (CoreML/ANE — ASR, VAD, Diarization, TTS)"
	@echo "    audio-build         Release build (.build/release/Server — used by launchd) + codesign + build-info"
	@echo "    audio-test          Sidecar tests (ITN)"
	@echo "    sidecar-install     Install audio sidecar launchd agents (auto-start, log rotation)"
	@echo "    sidecar-deploy      Build + re-sign + restart + wait for health (USE FOR PROD UPDATES)"
	@echo "    sidecar-restart     Restart the audio sidecar launchd agent"
	@echo "    sidecar-remove-shim Retire the legacy swift-sidecar agent (port conflict fix)"
	@echo "    sidecar-uninstall   Remove the audio sidecar launchd agents"
	@echo "    sidecar-status      launchd state + :8101 health check (incl. build SHA)"
	@echo ""
	@echo "  LLM sidecar (CoreML — chat, embeddings, image generation)"
	@echo "    llm-vendor          Clone + patch swift-embeddings $(EMBEDDINGS_VERSION) into sidecar-llm/vendor (run FIRST)"
	@echo "    llm-build           Release build (sidecar-llm/.build/release/Server)"
	@echo "    llm-install         Install llm-sidecar launchd agent + log rotation"
	@echo "    llm-restart         Restart the llm-sidecar launchd agent"
	@echo "    llm-uninstall       Remove the llm-sidecar launchd agent"
	@echo "    llm-status          launchd state + :8080 health check"
	@echo ""
	@echo "  ITN (optional Rust build — run BEFORE audio-build)"
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
	@echo "    clean               Remove bin/, sidecar-audio/.build, sidecar-llm/.build, Go cache"
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

# ---------- Audio sidecar (CoreML/ANE — ASR, VAD, Diarization, TTS) ----------
audio-sidecar:
	@echo ""
	@echo "  🚀 Starting audio sidecar (CoreML/ANE)"
	@echo "  ℹ  Listening on http://localhost:8101"
	@echo "  ℹ  ASR, VAD, Diarization, TTS via FluidAudio"
	@echo ""
	cd sidecar-audio && swift run Server

audio-build:
	cd sidecar-audio && swift build -c release
	# Re-ad-hoc-sign after every build: launchd can kill a freshly
	# replaced binary on first spawn (OS_REASON_CODESIGNING) when it
	# still has the old inode cached — an explicit re-sign avoids the
	# flaky first launch after each deploy.
	codesign --force --sign - sidecar-audio/.build/release/Server
	# Build marker — the sidecar reads this at startup and surfaces it in
	# /health so the deployed version is remotely verifiable per node.
	@printf '{"sha":"%s","built_at":"%s"}\n' \
		"$$(git -C $(CURDIR) rev-parse --short HEAD 2>/dev/null || echo unknown)" \
		"$$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		> sidecar-audio/.build/release/build-info.json

# sidecar-deploy: build + re-sign + restart + wait for health. Use this
# (not bare audio-build/sidecar-restart) when updating production nodes.
sidecar-deploy: audio-build
	launchctl kickstart -k $(GUI_DOMAIN)/$(SIDECAR_LABEL)
	@echo "  ⏳ Waiting for sidecar health on :8101..."
	@for i in $$(seq 1 60); do \
		if curl -sf http://localhost:8101/health >/dev/null 2>&1; then \
			echo "  ✅ Audio sidecar healthy after $${i}s"; \
			curl -s http://localhost:8101/health | sed 's/^/     /'; \
			exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "  ❌ Sidecar did not become healthy within 60s — check deploy/macos/logs/"; \
	exit 1

audio-test:
	@echo ""
	@echo "  🧪 Running audio sidecar tests (ITN, ...)"
	@echo ""
	cd sidecar-audio && swift test --filter $(ITN_TEST_FILTER)

# ---------- Audio sidecar launchd agent (production nodes) ----------
# Auto-start the audio sidecar at login (pair with auto-login for headless
# minis) and restart it on crash. See deploy/macos/README.md for the full
# headless setup (pmset, FileVault, auto-login).
#
# Migration: this target installs BOTH `com.gotranscribesrv.audio-sidecar`
# (current) and `com.gotranscribesrv.swift-sidecar` (legacy shim) plists.
# Once the new label is healthy, run `make sidecar-uninstall` to drop both,
# then delete the legacy plist from ~/Library/LaunchAgents/ if desired.
sidecar-install: audio-build
	@echo ""
	@echo "  📦 Installing audio-sidecar LaunchAgent(s)"
	@echo "  ℹ  Repo path baked into plist: $(CURDIR)"
	@echo ""
	mkdir -p deploy/macos/logs $(LAUNCHAGENTS)
	chmod +x $(SIDECAR_ROTATE_SCRIPT)
	# New primary label
	sed 's|__REPO_PATH__|$(CURDIR)|g' $(SIDECAR_PLIST) > $(LAUNCHAGENTS)/$(SIDECAR_LABEL).plist
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_LABEL) 2>/dev/null
	launchctl bootstrap $(GUI_DOMAIN) $(LAUNCHAGENTS)/$(SIDECAR_LABEL).plist
	# Legacy shim (com.gotranscribesrv.swift-sidecar) — same binary
	sed 's|__REPO_PATH__|$(CURDIR)|g' $(SIDECAR_LEGACY_PLIST) > $(LAUNCHAGENTS)/$(SIDECAR_LEGACY_LABEL).plist
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_LEGACY_LABEL) 2>/dev/null
	launchctl bootstrap $(GUI_DOMAIN) $(LAUNCHAGENTS)/$(SIDECAR_LEGACY_LABEL).plist
	# Log rotation companion (covers both audio labels + llm)
	sed 's|__REPO_PATH__|$(CURDIR)|g' $(SIDECAR_ROTATE_PLIST) > $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_ROTATE_LABEL) 2>/dev/null
	launchctl bootstrap $(GUI_DOMAIN) $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist
	@echo ""
	@echo "  ✅ Audio sidecar installed (com.gotranscribesrv.audio-sidecar)"
	@echo "  ✅ Legacy shim installed (com.gotranscribesrv.swift-sidecar) — remove after migration"
	@echo "  ℹ  Log rotation installed — hourly, 10m x 3 (mirrors Docker logging)"
	@echo "  ℹ  Verify: make sidecar-status"
	@echo "  ℹ  Logs:   deploy/macos/logs/"
	@echo ""

sidecar-restart:
	launchctl kickstart -k $(GUI_DOMAIN)/$(SIDECAR_LABEL)
	@echo "  ✅ Audio sidecar restarted"

sidecar-uninstall:
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_LABEL)
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_LEGACY_LABEL)
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_ROTATE_LABEL)
	rm -f $(LAUNCHAGENTS)/$(SIDECAR_LABEL).plist
	rm -f $(LAUNCHAGENTS)/$(SIDECAR_LEGACY_LABEL).plist
	rm -f $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist
	@echo "  ✅ Audio sidecar LaunchAgents removed (new + legacy)"

# sidecar-remove-shim: retire ONLY the legacy com.gotranscribesrv.swift-sidecar
# label. The shim was a rename-transition aid — while installed, both agents
# compete for :8101, and a shim baked with an older repo path keeps serving
# the OLD binary. The primary agent and log rotation stay untouched.
sidecar-remove-shim:
	-launchctl bootout $(GUI_DOMAIN)/$(SIDECAR_LEGACY_LABEL)
	rm -f $(LAUNCHAGENTS)/$(SIDECAR_LEGACY_LABEL).plist
	@echo "  ✅ Legacy swift-sidecar shim removed (primary agent untouched)"
	@echo "  ℹ  Verify: make sidecar-status (shim should read 'not loaded')"

sidecar-status:
	@echo "  launchd (audio-sidecar):"
	@out=$$(launchctl print $(GUI_DOMAIN)/$(SIDECAR_LABEL) 2>/dev/null | grep -E "state|pid|last exit"); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (not loaded)"; fi
	@echo "  launchd (swift-sidecar shim):"
	@out=$$(launchctl print $(GUI_DOMAIN)/$(SIDECAR_LEGACY_LABEL) 2>/dev/null | grep -E "state|pid|last exit"); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (not loaded)"; fi
	@echo "  launchd (logrotate):"
	@out=$$(launchctl print $(GUI_DOMAIN)/$(SIDECAR_ROTATE_LABEL) 2>/dev/null | grep -E "state|pid|last exit"); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (not loaded)"; fi
	@echo "  health:"
	@out=$$(curl -sf http://localhost:8101/health 2>/dev/null); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (no response on :8101)"; fi

# ---------- LLM sidecar (CoreML — chat, embeddings, image generation) ----------
# sidecar-llm is committed to this repo, but its `vendor/swift-embeddings`
# path-dependency is not (same pattern as itn-vendor): `make llm-vendor`
# clones upstream and re-applies our local patches (macOS 15 platform bump +
# @preconcurrency imports for Swift 6 strict concurrency). llm-build /
# llm-sidecar / llm-install fail with a hint if it hasn't been vendored yet.

llm-vendor:
	@if [ ! -f "$(EMBEDDINGS_VENDOR_DIR)/Package.swift" ]; then \
		echo "📥 Cloning swift-embeddings $(EMBEDDINGS_VERSION)..."; \
		rm -rf $(EMBEDDINGS_VENDOR_DIR); \
		mkdir -p sidecar-llm/vendor; \
		git clone --depth 1 --branch $(EMBEDDINGS_VERSION) $(EMBEDDINGS_REPO) $(EMBEDDINGS_VENDOR_DIR); \
		echo "🔧 Applying patches: macOS 15 platform bump + @preconcurrency imports"; \
		sed -i '' 's/\.macOS(\.v14)/.macOS(.v15)/' $(EMBEDDINGS_VENDOR_DIR)/Package.swift; \
		grep -rl --include="*.swift" "^import Tokenizers$$" $(EMBEDDINGS_VENDOR_DIR)/Sources \
			| xargs sed -i '' 's/^import Tokenizers$$/@preconcurrency import Tokenizers/'; \
		grep -rl --include="*.swift" "^import Hub$$" $(EMBEDDINGS_VENDOR_DIR)/Sources \
			| xargs sed -i '' 's/^import Hub$$/@preconcurrency import Hub/'; \
		echo "✅ swift-embeddings vendored + patched at $(EMBEDDINGS_VENDOR_DIR)"; \
	else \
		echo "✅ swift-embeddings already vendored at $(EMBEDDINGS_VENDOR_DIR)"; \
	fi

llm-sidecar:
	@if [ ! -f "$(EMBEDDINGS_VENDOR_DIR)/Package.swift" ]; then \
		echo "❌ swift-embeddings not vendored at $(EMBEDDINGS_VENDOR_DIR)"; \
		echo "   Run: make llm-vendor"; \
		exit 1; \
	fi
	@echo ""
	@echo "  🚀 Starting LLM sidecar (CoreML)"
	@echo "  ℹ  Listening on http://localhost:$(LLM_SIDECAR_PORT)"
	@echo "  ℹ  OpenAI/Anthropic-compatible chat + embeddings + image generation"
	@echo ""
	cd sidecar-llm && PORT=$(LLM_SIDECAR_PORT) swift run Server

llm-build:
	@if [ ! -f "$(EMBEDDINGS_VENDOR_DIR)/Package.swift" ]; then \
		echo "❌ swift-embeddings not vendored at $(EMBEDDINGS_VENDOR_DIR)"; \
		echo "   Run: make llm-vendor"; \
		exit 1; \
	fi
	cd sidecar-llm && swift build -c release

llm-install: llm-build
	@echo ""
	@echo "  📦 Installing $(LLM_SIDECAR_LABEL) LaunchAgent"
	@echo "  ℹ  Repo path baked into plist: $(CURDIR)"
	@echo ""
	mkdir -p deploy/macos/logs $(LAUNCHAGENTS)
	chmod +x $(SIDECAR_ROTATE_SCRIPT)
	sed 's|__REPO_PATH__|$(CURDIR)|g' $(LLM_SIDECAR_PLIST) > $(LAUNCHAGENTS)/$(LLM_SIDECAR_LABEL).plist
	-launchctl bootout $(GUI_DOMAIN)/$(LLM_SIDECAR_LABEL) 2>/dev/null
	launchctl bootstrap $(GUI_DOMAIN) $(LAUNCHAGENTS)/$(LLM_SIDECAR_LABEL).plist
	# Reuse the existing logrotate companion (rotates all three log pairs).
	@if [ -f $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist ]; then \
		echo "  ℹ  Logrotate companion already installed (covers audio + llm logs)"; \
	else \
		sed 's|__REPO_PATH__|$(CURDIR)|g' $(SIDECAR_ROTATE_PLIST) > $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist; \
		launchctl bootstrap $(GUI_DOMAIN) $(LAUNCHAGENTS)/$(SIDECAR_ROTATE_LABEL).plist; \
		echo "  ✅ Logrotate companion installed"; \
	fi
	@echo ""
	@echo "  ✅ LLM sidecar installed — starts now and at every login"
	@echo "  ℹ  Verify: make llm-status"
	@echo "  ℹ  Logs:   deploy/macos/logs/llm-sidecar.{out,err}.log"
	@echo ""

llm-restart:
	launchctl kickstart -k $(GUI_DOMAIN)/$(LLM_SIDECAR_LABEL)
	@echo "  ✅ LLM sidecar restarted"

llm-uninstall:
	-launchctl bootout $(GUI_DOMAIN)/$(LLM_SIDECAR_LABEL)
	rm -f $(LAUNCHAGENTS)/$(LLM_SIDECAR_LABEL).plist
	@echo "  ✅ LLM sidecar LaunchAgent removed (logrotate companion left in place)"

llm-status:
	@echo "  launchd (llm-sidecar):"
	@out=$$(launchctl print $(GUI_DOMAIN)/$(LLM_SIDECAR_LABEL) 2>/dev/null | grep -E "state|pid|last exit"); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (not loaded)"; fi
	@echo "  health:"
	@out=$$(curl -sf http://localhost:$(LLM_SIDECAR_PORT)/health 2>/dev/null); \
		if [ -n "$$out" ]; then echo "$$out" | sed 's/^/    /'; else echo "    (no response on :$(LLM_SIDECAR_PORT))"; fi

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
		mkdir -p sidecar-audio/Vendor; \
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
	@echo "  ✅ Static lib built. Rebuild the audio sidecar to link it:"
	@echo "      make audio-build"
	@echo "  Run tests to verify:"
	@echo "      make audio-test"
	@echo ""

itn-clean:
	rm -rf $(ITN_VENDOR_DIR)/target

# ---------- Dev: Docker (Postgres + Go server + Presidio) ----------
up:
	$(DEV_COMPOSE) up -d --build
	@echo ""
	@echo "  ✅ Postgres + Go server + Presidio running"
	@echo "  ℹ  API at http://localhost:3000"
	@echo "  ℹ  Run 'make audio-sidecar' in another terminal"
	@echo ""

down:
	$(DEV_COMPOSE) down

logs:
	$(DEV_COMPOSE) logs -f

# ---------- Production: Mac mini node (docker-compose.node.yml) ----------
# Runs on each Mac mini: Go server + Presidio in Docker, audio sidecar
# native via launchd (see deploy/macos/). DB lives on the separate DB VM.
node-up:
	$(NODE_COMPOSE) up -d --build
	@echo ""
	@echo "  ✅ Node server + Presidio running"
	@echo "  ℹ  API at http://localhost:3000 (reachable by Caddy on the DB VM)"
	@echo "  ℹ  Audio sidecar is native — see deploy/macos/README.md"
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
	rm -rf bin/ sidecar-audio/.build sidecar-llm/.build
	go clean
