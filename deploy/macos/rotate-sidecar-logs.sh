#!/bin/bash
# rotate-sidecar-logs.sh — size-based rotation for the GoTranscribeSrv
# sidecars' launchd flat-file logs (deploy/macos/logs/*.log).
#
# Runs hourly (and once at load) via the companion LaunchAgent
# com.gotranscribesrv.swift-sidecar-logrotate, installed by
# `make sidecar-install` (audio) and `make llm-install` (llm).
#
# Rotates:
#   - swift-sidecar.{out,err}.log  (legacy audio-sidecar shim — transition)
#   - audio-sidecar.{out,err}.log   (current audio sidecar)
#   - llm-sidecar.{out,err}.log     (LLM sidecar)
#
# Copy-truncate, NOT mv: launchd holds the log files open with O_APPEND
# for the life of the sidecar process. Moving the file out from under it
# would strand writes in an orphaned inode (invisible, still consuming
# disk). Truncating in place is safe — O_APPEND writes land at the new
# EOF, so the sidecar never needs a restart.
#
# Policy mirrors the Docker x-logging anchor in docker-compose*.yml
# (json-file, max-size 10m, max-file 3): worst case on disk is
# 2 files x 10 MB x (1 active + 3 archives) = 80 MB per sidecar.

set -euo pipefail

LOG_DIR="$(cd "$(dirname "$0")" && pwd)/logs"
MAX_SIZE="${LOG_MAX_SIZE:-10m}"   # bytes, or k/m/g suffix (matches compose env)
MAX_FILE="${LOG_MAX_FILE:-3}"

to_bytes() {
    case "$1" in
        *k|*K) echo $(( ${1%[kK]} * 1024 )) ;;
        *m|*M) echo $(( ${1%[mM]} * 1024 * 1024 )) ;;
        *g|*G) echo $(( ${1%[gG]} * 1024 * 1024 * 1024 )) ;;
        *)     echo "$1" ;;
    esac
}

rotate_one() {
    local log="$1" limit="$2" size i n f
    if [ ! -f "$log" ]; then
        return 0
    fi
    size=$(stat -f %z "$log")
    if [ "$size" -lt "$limit" ]; then
        return 0
    fi
    for (( i = MAX_FILE - 1; i >= 1; i-- )); do
        if [ -f "$log.$i" ]; then
            mv "$log.$i" "$log.$((i + 1))"
        fi
    done
    cp "$log" "$log.1"
    : > "$log"
    # Drop archives beyond retention (e.g. MAX_FILE was lowered)
    for f in "$log".[0-9]*; do
        [ -e "$f" ] || continue
        n="${f##*.}"
        if [ "$n" -gt "$MAX_FILE" ]; then
            rm -f "$f"
        fi
    done
    echo "rotate-sidecar-logs: rotated $(basename "$log") (was ${size} bytes)"
}

LIMIT=$(to_bytes "$MAX_SIZE")

# Audio sidecar — current label
rotate_one "$LOG_DIR/audio-sidecar.out.log" "$LIMIT"
rotate_one "$LOG_DIR/audio-sidecar.err.log" "$LIMIT"

# Audio sidecar — legacy shim (com.gotranscribesrv.swift-sidecar)
rotate_one "$LOG_DIR/swift-sidecar.out.log" "$LIMIT"
rotate_one "$LOG_DIR/swift-sidecar.err.log" "$LIMIT"

# LLM sidecar
rotate_one "$LOG_DIR/llm-sidecar.out.log" "$LIMIT"
rotate_one "$LOG_DIR/llm-sidecar.err.log" "$LIMIT"
