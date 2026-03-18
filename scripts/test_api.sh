#!/usr/bin/env bash
# ============================================================
# GoTranscribeSrv — Smoke Test
# ============================================================
# Hits the API endpoints to verify everything works end-to-end.
#
# Usage:
#   ./scripts/test_api.sh                          # uses defaults
#   ./scripts/test_api.sh -k gtx_live_abc123       # specify API key
#   ./scripts/test_api.sh -h http://remote:3000    # specify host
#   ./scripts/test_api.sh -a /path/to/audio.wav    # use existing audio
# ============================================================

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────
API_HOST="${API_HOST:-http://localhost:3000}"
API_KEY="${API_KEY:-}"
AUDIO_FILE=""
SAMPLE_TEXT="Hello, this is a test of the GoTranscribeSrv transcription system. The quick brown fox jumps over the lazy dog."

# ── Colors ──────────────────────────────────────────────────
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

ok()   { echo -e "  ${GREEN}✅ $1${NC}"; }
fail() { echo -e "  ${RED}❌ $1${NC}"; }
info() { echo -e "  ${CYAN}ℹ  $1${NC}"; }
warn() { echo -e "  ${YELLOW}⚠️  $1${NC}"; }

# Helper: pretty-print JSON (SIGPIPE-safe)
json_pp() {
    python3 -m json.tool 2>/dev/null | sed 's/^/     /' || true
}

# ── Parse args ──────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case $1 in
        -k|--key)     API_KEY="$2"; shift 2 ;;
        -h|--host)    API_HOST="$2"; shift 2 ;;
        -a|--audio)   AUDIO_FILE="$2"; shift 2 ;;
        *)            echo "Unknown option: $1"; exit 1 ;;
    esac
done

# ── Validate API key ───────────────────────────────────────
if [[ -z "$API_KEY" ]]; then
    if [[ -f .env ]]; then
        API_KEY=$(grep -E '^ADMIN_API_KEY=' .env 2>/dev/null | cut -d= -f2- || true)
    fi
    if [[ -z "$API_KEY" ]]; then
        echo -e "${RED}Error: No API key provided.${NC}"
        echo ""
        echo "Usage:"
        echo "  $0 -k YOUR_API_KEY"
        echo "  # or set ADMIN_API_KEY in .env"
        echo "  # or export API_KEY=YOUR_API_KEY"
        exit 1
    fi
fi

echo ""
echo "══════════════════════════════════════════════════════════"
echo "  GoTranscribeSrv — API Smoke Test"
echo "══════════════════════════════════════════════════════════"
echo ""
info "Host:  $API_HOST"
info "Key:   ${API_KEY:0:15}..."
echo ""

# ── Generate sample audio ──────────────────────────────────
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

if [[ -n "$AUDIO_FILE" ]]; then
    if [[ ! -f "$AUDIO_FILE" ]]; then
        fail "Audio file not found: $AUDIO_FILE"
        exit 1
    fi
    info "Using provided audio: $AUDIO_FILE"
elif [[ -f "sample_test.mp3" ]]; then
    AUDIO_FILE="sample_test.mp3"
    info "Using sample_test.mp3 ($(du -h "$AUDIO_FILE" | cut -f1 | xargs))"
else
    AUDIO_FILE="$TEMP_DIR/sample.wav"
    info "No sample_test.mp3 found — generating audio with macOS 'say'..."

    if command -v say &>/dev/null; then
        say -o "$TEMP_DIR/sample.aiff" "$SAMPLE_TEXT"
        afconvert "$TEMP_DIR/sample.aiff" "$AUDIO_FILE" -f WAVE -d LEI16@16000
        ok "Generated sample.wav ($(du -h "$AUDIO_FILE" | cut -f1 | xargs))"
    else
        fail "No audio file found. Provide one with: $0 -a /path/to/audio.wav"
        exit 1
    fi
fi

echo ""

# ── Test 1: Health Check ────────────────────────────────────
echo "── Test 1: Server Health ──────────────────────────"
HEALTH=$(curl -s -w "\n%{http_code}" "$API_HOST/health" 2>/dev/null || echo -e "\n000")
HTTP_CODE=$(echo "$HEALTH" | tail -1)
BODY=$(echo "$HEALTH" | sed '$d')

if [[ "$HTTP_CODE" == "200" ]]; then
    ok "Server healthy ($HTTP_CODE)"
    echo "$BODY" | json_pp
else
    fail "Server unreachable (HTTP $HTTP_CODE)"
    echo "     Is the server running on $API_HOST?"
    exit 1
fi

echo ""

# ── Test 2: Auth Check (bad key) ────────────────────────────
echo "── Test 2: Auth Rejection (bad key) ───────────────"
AUTH_CHECK=$(curl -s -w "\n%{http_code}" \
    -H "X-API-Key: gtx_live_invalid_key_12345" \
    "$API_HOST/api/v1/usage/summary" 2>/dev/null || echo -e "\n000")
HTTP_CODE=$(echo "$AUTH_CHECK" | tail -1)

if [[ "$HTTP_CODE" == "401" ]] || [[ "$HTTP_CODE" == "403" ]]; then
    ok "Bad API key correctly rejected ($HTTP_CODE)"
else
    warn "Unexpected response for bad key: HTTP $HTTP_CODE"
fi

echo ""

# ── Test 3: File Transcription ──────────────────────────────
echo "── Test 3: File Transcription (ASR) ───────────────"
info "Uploading $(du -h "$AUDIO_FILE" | cut -f1 | xargs) audio file..."

ASR_RESULT=$(curl -s --max-time 300 -w "\n%{http_code}" \
    -X POST "$API_HOST/api/v1/asr" \
    -H "X-API-Key: $API_KEY" \
    -F "audio=@$AUDIO_FILE" \
    -F "language=en" \
    2>/dev/null || echo -e "\n000")
HTTP_CODE=$(echo "$ASR_RESULT" | tail -1)
BODY=$(echo "$ASR_RESULT" | sed '$d')

if [[ "$HTTP_CODE" == "200" ]]; then
    ok "Transcription succeeded ($HTTP_CODE)"
    echo ""
    echo "     Response:"
    echo "$BODY" | json_pp
else
    fail "Transcription failed (HTTP $HTTP_CODE)"
    echo "$BODY" | json_pp
fi

echo ""

# ── Test 4: Transcription + Diarization ──────────────────────
echo "── Test 4: Transcription + Diarization ────────────"
info "Uploading with diarize=true..."

DIAR_RESULT=$(curl -s --max-time 300 -w "\n%{http_code}" \
    -X POST "$API_HOST/api/v1/asr" \
    -H "X-API-Key: $API_KEY" \
    -F "audio=@$AUDIO_FILE" \
    -F "diarize=true" \
    2>/dev/null || echo -e "\n000")
HTTP_CODE=$(echo "$DIAR_RESULT" | tail -1)
BODY=$(echo "$DIAR_RESULT" | sed '$d')

if [[ "$HTTP_CODE" == "200" ]]; then
    ok "Diarized transcription succeeded ($HTTP_CODE)"
    echo ""
    echo "     Response:"
    echo "$BODY" | json_pp
else
    warn "Diarized transcription failed (HTTP $HTTP_CODE) — diarizer may not be loaded"
fi

echo ""

# ── Test 5: TTS ─────────────────────────────────────────────
echo "── Test 5: Text-to-Speech (TTS) ───────────────────"
TTS_RESULT=$(curl -s --max-time 300 -w "\n%{http_code}" \
    -X POST "$API_HOST/api/v1/tts" \
    -H "X-API-Key: $API_KEY" \
    -H "Content-Type: application/json" \
    -d '{"text": "Hello from GoTranscribeSrv", "voice": "default"}' \
    -o "$TEMP_DIR/tts_output.wav" \
    2>/dev/null || echo "000")
HTTP_CODE=$(echo "$TTS_RESULT" | tail -1)

if [[ "$HTTP_CODE" == "200" ]]; then
    ok "TTS succeeded ($HTTP_CODE) — output: $(du -h "$TEMP_DIR/tts_output.wav" | cut -f1 | xargs)"
elif [[ "$HTTP_CODE" == "502" ]]; then
    warn "TTS unavailable (502) — LuxTTS may not be loaded"
else
    warn "TTS failed (HTTP $HTTP_CODE)"
fi

echo ""

# ── Test 6: Usage Summary ──────────────────────────────────
echo "── Test 6: Usage Summary ──────────────────────────"
USAGE_RESULT=$(curl -s -w "\n%{http_code}" \
    -H "X-API-Key: $API_KEY" \
    "$API_HOST/api/v1/usage/summary" 2>/dev/null || echo -e "\n000")
HTTP_CODE=$(echo "$USAGE_RESULT" | tail -1)
BODY=$(echo "$USAGE_RESULT" | sed '$d')

if [[ "$HTTP_CODE" == "200" ]]; then
    ok "Usage summary retrieved ($HTTP_CODE)"
    echo "$BODY" | json_pp
else
    warn "Usage endpoint returned HTTP $HTTP_CODE"
fi

echo ""
echo "══════════════════════════════════════════════════════════"
echo "  Smoke test complete"
echo "══════════════════════════════════════════════════════════"
echo ""
