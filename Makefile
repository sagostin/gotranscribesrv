.PHONY: run build test migrate lint sidecar clean setup-models setup-voices venv \
       up down logs rebuild

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

# ---------- Python venv ----------
venv: $(VENV_PY)

PYTHON    := $(shell command -v python3.11 || echo python3)

$(VENV_PY): sidecar/requirements.txt
	$(PYTHON) -m venv $(VENV_DIR)
	$(VENV_PIP) install --upgrade pip
	$(VENV_PIP) install -r sidecar/requirements.txt
	@touch $(VENV_PY)

# ---------- Python sidecar (native, MPS-accelerated) ----------
sidecar: setup-luxtts
	@echo ""
	@echo "  🚀 Starting native Python sidecar (MPS acceleration)"
	@echo "  ℹ  Listening on http://localhost:8100"
	@echo "  ℹ  Workers: auto-detect (override with SIDECAR_WORKERS=N)"
	@echo ""
	cd sidecar && PYTHONPATH="$$(pwd)/LuxTTS:$$PYTHONPATH" $(abspath $(VENV_PY)) main.py

# ---------- Setup scripts ----------
setup-luxtts: venv
	@test -d sidecar/LuxTTS || git clone https://github.com/ysharma3501/LuxTTS.git sidecar/LuxTTS
	@test -f $(VENV_DIR)/.luxtts_installed || ( \
		$(VENV_PIP) install "linacodec @ git+https://github.com/ysharma3501/LinaCodec.git" && \
		$(VENV_PIP) install piper-phonemize --find-links https://k2-fsa.github.io/icefall/piper_phonemize.html && \
		$(VENV_PIP) install sidecar/LuxTTS && \
		touch $(VENV_DIR)/.luxtts_installed \
	)

setup-models: venv
	cd sidecar && $(abspath $(VENV_PY)) ../scripts/download_models.py

setup-voices: venv
	$(VENV_PY) scripts/setup_voices.py

setup: setup-luxtts venv setup-models setup-voices
	@echo "✅ All models and voices ready"

# ---------- Docker (Postgres + Go server) ----------
up:
	docker compose up -d --build
	@echo ""
	@echo "  ✅ Postgres + Go server running"
	@echo "  ℹ  API at http://localhost:3000"
	@echo "  ℹ  Run 'make sidecar' in another terminal"
	@echo ""

down:
	docker compose down

logs:
	docker compose logs -f

rebuild:
	docker compose up -d --build

# ---------- Utilities ----------
clean:
	rm -rf bin/ $(VENV_DIR)
	go clean

tidy:
	go mod tidy
