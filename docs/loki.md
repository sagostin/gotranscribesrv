# Loki Logging

`gotranscribesrv` ships structured events to [Grafana Loki](https://grafana.com/oss/loki/) in addition to stdout. The local emit backend is Go's stdlib `log/slog`; the optional Loki push uses a non-blocking, drop-on-full channel so the request path is never stalled by Loki latency or downtime.

## Configuration

All knobs live in `.env`:

```bash
# ── Loki Logging (optional) ────────────────────────────────
# Set LOKI_ENABLED=true to ship structured events to Grafana Loki
# in addition to stdout. Non-blocking; drops events on channel
# full rather than stalling the request path. Auth is HTTP Basic.
LOKI_ENABLED=false
LOKI_PUSH_URL=http://loki:3100
LOKI_USERNAME=
LOKI_PASSWORD=
LOKI_JOB=gotranscribesrv
SERVER_ID=node-1
```

| Var | Default | Purpose |
|---|---|---|
| `LOKI_ENABLED` | `false` | Master switch. When `false` the LogManager still runs (one idle goroutine + a 512-buffered channel) and emits every event to stdout, but the network push is skipped. |
| `LOKI_PUSH_URL` | `http://loki:3100` | Base Loki URL. `/loki/api/v1/push` is appended automatically. |
| `LOKI_USERNAME` / `LOKI_PASSWORD` | empty | HTTP Basic auth. If either is empty, no `Authorization` header is sent. |
| `LOKI_JOB` | `gotranscribesrv` | Value of the `job` label on every stream (used by Promtail/LogQL to filter). |
| `SERVER_ID` | (unset) | Value of the `server_id` label — useful when running multiple nodes behind a load balancer. |

## What gets shipped

The `LogManager` is the single source of truth for events. It is initialized in `cmd/server/main.go:49` and gracefully drained on shutdown via `defer lm.CloseLogManager()`.

Two paths feed it:

1. **Local handlers / middleware** call `lm.SendLog(lm.BuildLog(...))` at the boundaries listed below. These carry the structured metadata you most often want to grep in Grafana.
2. **The existing `slog` calls** (in handlers, middleware, sidecar client) are unchanged. They continue to print to stdout. The `LogManager` is the only thing that adds Loki shipping.

When `LOKI_ENABLED=true`, every event emitted via `SendLog` is also queued onto an in-memory channel (cap 512). A single consumer goroutine drains the channel and POSTs entries to Loki, one entry per request (no batching — the latency-vs-volume tradeoff favors per-event detail for ASR/TTS debugging).

If the channel is full, a single `slog.Warn("log channel full, dropping log", ...)` is emitted locally and the event is dropped. **The request path is never blocked.**

## Capture points

| Event | Type label | Captured fields |
|---|---|---|
| File ASR received | `ASR_REQUEST_RECEIVED` | filename, file_size, language, diarize, itn, ip |
| File ASR completed | `ASR_COMPLETED` | filename, file_size, audio_ms, asr_ms, sidecar_ms, model, language, diarized, num_speakers, speakers, word_count, segment_count, **transcript** |
| File ASR failed | `ASR_FAILED` | filename, sidecar_ms, error |
| ASR file too large / missing audio | `ASR_FILE_TOO_LARGE` / `ASR_MISSING_AUDIO` | filename, file_size, ip |
| Whisper-compat request | `WHISPER_REQUEST_RECEIVED` / `WHISPER_COMPLETED` / `WHISPER_FAILED` | filename, model, language, response_format, **transcript** |
| Deepgram WS session | `DEEPGRAM_SESSION_STARTED` / `DEEPGRAM_SESSION_ENDED` | request_id, audio_bytes, audio_duration_ms, process_ms, realtime_x |
| Deepgram sidecar error | `DEEPGRAM_SESSION_ERROR` | request_id, error |
| Watson HTTP batch | `WATSON_RECOGNIZE_RECEIVED` / `_COMPLETED` / `_FAILED` | content_type, audio_ms, **transcript** |
| Watson WS session | `WATSON_SESSION_STARTED` / `_ENDED` | request_id, audio_bytes, audio_duration_ms, process_ms, realtime_x, speaker_labels |
| Watson sidecar error | `WATSON_SIDECAR_ERROR` | request_id, error |
| Native WS ASR | `WS_ASR_SESSION_STARTED` / `_ENDED` | audio_bytes, audio_duration_ms, process_ms, realtime_x |
| TTS | `TTS_REQUEST_RECEIVED` / `TTS_COMPLETED` / `TTS_FAILED` | voice, voice_id, text_length, output_bytes, output_duration_ms, synth_time_ms |
| Voice clone | `VOICE_CLONE_STARTED` / `_COMPLETED` / `_FAILED` | user_id, name, file_size, embedding_bytes, audio_duration_ms, clone_time_ms |
| LLM process | `LLM_PROCESS_STARTED` / `_COMPLETED` / `_FAILED` | task, input_length, output_length, tokens_generated, process_time_ms, **result** |
| Aggregated 4xx/5xx | `REQUEST_FAILED` | endpoint, status, error_code, method, path, ip, user_agent, process_ms, user_id, api_key_id |

### Labels vs fields

Every entry lands in Loki as one stream with these labels (low-cardinality, indexed):

- `job` — `LOKI_JOB`
- `server_id` — `SERVER_ID` env
- `type` — the event `type` (e.g. `ASR_COMPLETED`)
- `level` — `INFO` / `WARN` / `ERROR` / `DEBUG`
- `endpoint` — promoted from the `endpoint` field when present

All other fields (including `transcript`, `filename`, audio meta, error strings, sidecar timings) end up in the JSON line body, which is searchable via LogQL but not indexed.

## Sample queries

```logql
# All ASR completions for a specific file
{job="gotranscribesrv", type="ASR_COMPLETED"} | json | filename="meeting_2026_03_15.m4a"

# ASR failures in the last hour
{job="gotranscribesrv", type="ASR_FAILED"} | json | error!=""

# Realtime factor distribution (processing speed)
{job="gotranscribesrv", type="ASR_COMPLETED"} | json | unwrap sidecar_ms [5m]

# Failed HTTP requests (4xx/5xx)
{job="gotranscribesrv", type="REQUEST_FAILED"} | json | status="500"

# All TTS events for a specific voice
{job="gotranscribesrv", type=~"TTS_.*"} | json | voice="jane"

# WS streaming sessions
{job="gotranscribesrv", type="WS_ASR_SESSION_ENDED"} | json | audio_duration_ms > 60000
```

## Local docker-compose snippet

The simplest way to try it locally is a minimal Grafana + Loki + Promtail stack. Drop this into a separate `docker-compose.loki.yml`:

```yaml
services:
  loki:
    image: grafana/loki:3.3.0
    ports:
      - "3100:3100"
    command: -config.file=/etc/loki/local-config.yaml
    volumes:
      - loki-data:/loki

  grafana:
    image: grafana/grafana:11.4.0
    ports:
      - "3001:3000"
    environment:
      - GF_AUTH_ANONYMOUS_ENABLED=true
      - GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
    depends_on:
      - loki

volumes:
  loki-data:
```

Then in your main `.env`:

```bash
LOKI_ENABLED=true
LOKI_PUSH_URL=http://localhost:3100
```

Open `http://localhost:3001` → Explore → Loki → `{job="gotranscribesrv"}`.

## Performance characteristics

- **One idle goroutine** when `LOKI_ENABLED=false` (drains the channel and skips the network call).
- **512-entry buffered channel** caps memory at a few MB even under load spikes.
- **3-second HTTP timeout** per push; failures log locally and are dropped, never retried (avoids head-of-line blocking during a Loki outage).
- **Zero new dependencies** — stdlib `net/http`, `encoding/json`, `log/slog`, `sync`, `time` only.

## Graceful shutdown

`CloseLogManager()` is called via `defer` in `main.go:62`, after `app.Shutdown()` has stopped accepting new connections. It:

1. Closes the log channel.
2. Waits for the consumer goroutine to drain any in-flight entries.

A 3-second HTTP timeout bounds how long shutdown can take.
