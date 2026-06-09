.PHONY: run build test migrate lint sidecar swift-sidecar clean setup-models setup-voices venv \
        up down logs rebuild swift-test

# ---------- Config ----------
VENV_DIR  := sidecar/.venv
VENV_PY   := $(VENV_DIR)/bin/python3
VENV_PIP  := $(VENV_DIR)/bin/pip

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
	cd sidecar-swift && swift test

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
