.PHONY: run build test migrate lint sidecar swift-sidecar swift-test swift-build \
        itn-build itn-vendor itn-clean clean setup-models setup-voices venv \
        up down logs rebuild

# ---------- Config ----------
VENV_DIR  := sidecar/.venv
VENV_PY   := $(VENV_DIR)/bin/python3
VENV_PIP  := $(VENV_DIR)/bin/pip

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

# ---------- Utilities ----------
clean:
	rm -rf bin/ $(VENV_DIR) sidecar-swift/.build
	go clean

tidy:
	go mod tidy
