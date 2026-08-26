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
| `SERVER_ID` | (unset) | Value of the `server_id` label **and** the top-level `server_id` JSON field on every event, **and** a `server_id` attr on every stdout slog line — useful when running multiple nodes behind a load balancer. |

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
| File ASR completed | `ASR_COMPLETED` | filename, file_size, audio_ms, asr_ms, sidecar_ms, model, language, diarized, num_speakers, speakers, word_count, segment_count, **transcript (PII-redacted)**, pii_redacted, pii_entity_types |
| File ASR failed | `ASR_FAILED` | filename, sidecar_ms, error |
| ASR file too large / missing audio | `ASR_FILE_TOO_LARGE` / `ASR_MISSING_AUDIO` | filename, file_size, ip |
| Whisper-compat request | `WHISPER_REQUEST_RECEIVED` / `WHISPER_COMPLETED` / `WHISPER_FAILED` | filename, model, language, response_format, **transcript (PII-redacted)**, pii_redacted |
| Deepgram WS session | `DEEPGRAM_SESSION_STARTED` / `DEEPGRAM_SESSION_ENDED` | request_id, audio_bytes, audio_duration_ms, process_ms, realtime_x |
| Deepgram sidecar error | `DEEPGRAM_SESSION_ERROR` | request_id, error |
| Watson HTTP batch | `WATSON_RECOGNIZE_RECEIVED` / `_COMPLETED` / `_FAILED` | content_type, audio_ms, **transcript (PII-redacted)**, pii_redacted |
| Watson WS session | `WATSON_SESSION_STARTED` / `_ENDED` | request_id, audio_bytes, audio_duration_ms, process_ms, realtime_x, speaker_labels |
| Watson sidecar error | `WATSON_SIDECAR_ERROR` | request_id, error |
| Native WS ASR | `WS_ASR_SESSION_STARTED` / `_ENDED` | audio_bytes, audio_duration_ms, process_ms, realtime_x |
| TTS | `TTS_REQUEST_RECEIVED` / `TTS_COMPLETED` / `TTS_FAILED` / `TTS_VOICE_LOAD_FAILED` | voice, voice_id, text_length, output_bytes, output_duration_ms, synth_time_ms |
| Voice clone | `VOICE_CLONE_STARTED` / `_COMPLETED` / `_FAILED` / `_DIR_ERROR` / `_WRITE_ERROR` / `_DB_ERROR` | user_id, name, file_size, embedding_bytes, audio_duration_ms, clone_time_ms |
| Voice list / delete | `VOICE_LIST_DB_ERROR` / `VOICE_DELETE_DB_ERROR` | user_id, voice_id |
| Aggregated 4xx/5xx | `REQUEST_FAILED` | endpoint, status, error_code, method, path, ip, user_agent, process_ms, user_id, api_key_id |
| Failed auth | `AUTH_FAILED` | endpoint, method, auth_method (`jwt`/`api_key`/`basic`/`jwt_query`), reason (`expired`/`bad_signature`/`blacklisted`/`unknown_or_revoked`/etc.), ip, user_agent, request_id — **never logs the raw token/key** |
| PII analyzer fault | `PII_REDACTOR_ERROR` | endpoint, text_len, request_id — emitted when Presidio is unreachable; the associated `*_COMPLETED` event has `transcript: "<REDACTED-ERROR>"` |
| OpenAI realtime transcription (WS `/v1/realtime`) | `OPENAI_REALTIME_PARTIAL_SENT` / `OPENAI_REALTIME_FINAL_SENT` | request_id, engine, **transcript (PII-redacted)**, pii_redacted, pii_entity_types, is_final |
| OpenAI realtime S2S user transcript | `OPENAI_REALTIME_S2S_TRANSCRIPT_SENT` | request_id, engine, **transcript (PII-redacted)**, pii_redacted, pii_entity_types, speech_final |

### Central redaction guard

`LogManager.SendLog` enforces the redaction boundary on **every** event, for both stdout and Loki. The sensitive field keys — `transcript`, `text`, `delta`, `prompt`, `content` (case-insensitive) — are guarded:

- Values wrapped in `logging.Redacted(...)` (i.e. already passed through the PII redactor) are shipped as-is.
- A **plain string** under a sensitive key is replaced with the literal `<REDACTED-UNSAFE>` sentinel before emission — fail-closed, so a call site that forgets to redact can never leak raw content into logs.
- All other keys and non-string values pass through untouched.

Operators can surface every event caught by the guard with:

```logql
{job="gotranscribesrv"} |~ "<REDACTED-UNSAFE>"
```

Direct `slog.*` calls bypass the LogManager by design; handlers must never pass raw transcript text to `slog` either (the raw sidecar frames and client text frames are logged as byte counts / parsed event types only).

### Labels vs fields

Every entry lands in Loki as one stream with these labels (low-cardinality, indexed):

- `job` — `LOKI_JOB`
- `server_id` — `SERVER_ID` env (also stamped as a top-level `server_id` field in the JSON line body, so the emitting node is identifiable even when querying across merged streams)
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

# ── Audit / security ───────────────────────────────────────────

# Failed auth attempts in the last hour, grouped by reason
sum by (reason) (
  count_over_time({job="gotranscribesrv", type="AUTH_FAILED"} [1h])
)

# Same, by source IP — useful for spotting credential-stuffing or brute-force
sum by (ip) (
  count_over_time({job="gotranscribesrv", type="AUTH_FAILED"} [15m])
)

# Token-blacklist hits
{job="gotranscribesrv", type="AUTH_FAILED"} | json | reason="blacklisted"

# PII redactor health — should be 0 except during Presidio outages
sum(rate({job="gotranscribesrv", type="PII_REDACTOR_ERROR"} [5m]))

# ASR completions where the redactor fell back to <REDACTED-ERROR>
{job="gotranscribesrv", type="ASR_COMPLETED"} | json | transcript="<REDACTED-ERROR>"

# Distribution of PII entity types being scrubbed
sum by (entity_type) (
  count_over_time({job="gotranscribesrv", type="ASR_COMPLETED"} | json | pii_redacted > 0 [1h])
)
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

## Cross-correlating requests

Every HTTP request and WebSocket session is tagged with a UUID (`request_id`) that flows through every log event — both `LogManager` events (which ship to Loki) and the existing `slog.*` calls (which print to stdout). The id is:

- **Generated server-side** (inbound `X-Request-ID` headers are ignored — never trust a client-provided value for internal correlation)
- **Stored in `c.Locals("request_id")`** for HTTP handlers and middleware to read
- **Stored on `context.Context`** via the `RequestID()` middleware, picked up automatically by the `ContextHandler` for any `slog.InfoContext(c.UserContext(), ...)` call
- **Echoed on the response** as the `X-Request-ID` header so clients can correlate their own logs
- **Serialized as a top-level JSON field** in LogManager events (not a Loki label, to avoid per-request stream pressure)
- **Attributed as a top-level slog attr** in stdout output

For HTTP requests, the id is generated by `internal/middleware/requestid.go` and assigned by the `RequestID()` middleware (registered as the first `app.Use(...)` in `main.go` so even 401/429 failures are correlated).

For WebSocket sessions (`/ws/asr`, `/v1/listen`, `/v1/recognize`), the handler reuses the middleware-minted id from `c.Locals("request_id")` when present (so the HTTP access log, upgrade log, and every session event share one correlation id); it only mints a fresh UUID if none was set.

### Node attribution (`server_id`)

When `SERVER_ID` is set, every log surface identifies the emitting node:

- **stdout slog** — baked in as a `server_id` attr on the default logger in `main.go`, so *every* line (startup, handlers, background goroutines, sidecar client) carries it.
- **Loki** — present both as the `server_id` stream label and as a top-level `server_id` field in each event's JSON payload (stamped by `BuildLog`).
- **HTTP access log** — the Fiber access-log line includes `server=<id>` and `req=<request_id>`.

### Two correlation paths

1. **`LogManager.SendLog`** events — pass `"request_id": middleware.RequestIDFromCtx(c)` in the fields map. The id is hoisted to the top-level JSON field and shipped to Loki. (This is what shows up in Grafana.)

2. **Plain `slog.*` calls** in handlers — use the `*Context` variant (`slog.InfoContext(c.UserContext(), ...)`, `slog.ErrorContext`, etc.) and the `ContextHandler` automatically injects the id from the context. No call-site plumbing required beyond switching `slog.Info` → `slog.InfoContext`. The id appears as a `request_id` attr in stdout (and would in Loki too if these calls were ever mirrored).

WS handlers can't use the `*Context` variant directly (the WebSocket Conn doesn't expose `c.UserContext()`), so they pass `request_id` explicitly as a slog arg — same pattern they were already using for `requestID`.

### Sample correlation queries

```logql
# All events for a specific request (most common debugging query)
{job="gotranscribesrv"} | json | request_id="8f3a2b1c-4d5e-6f7a-8b9c-0d1e2f3a4b5c"

# All ASR events for a specific request, formatted
{job="gotranscribesrv", type=~"ASR_.*"} | json | request_id="8f3a-..."

# All errors for a specific request (catch failed ASR + 4xx/5xx)
{job="gotranscribesrv", level=~"WARN|ERROR"} | json | request_id="8f3a-..."

# Find all requests that failed in the last hour
{job="gotranscribesrv", type="REQUEST_FAILED"} | json

# Count requests by outcome
sum by (status) (
  count_over_time({job="gotranscribesrv", type="REQUEST_FAILED"} | json [1h])
)
```

### Client integration

Clients receive the generated id in the response header:

```bash
$ curl -i -X POST http://localhost:3000/api/v1/asr -F audio=@sample.wav
HTTP/1.1 200 OK
X-Request-ID: 8f3a2b1c-4d5e-6f7a-8b9c-0d1e2f3a4b5c
Content-Type: application/json
...
```

Log that id on the client side. When the user reports a problem, the support team can grep Loki for `request_id=<the value the client saw>` and pull the full server-side timeline.

## Graceful shutdown

`CloseLogManager()` is called via `defer` in `main.go:62`, after `app.Shutdown()` has stopped accepting new connections. It:

1. Closes the log channel.
2. Waits for the consumer goroutine to drain any in-flight entries.

A 3-second HTTP timeout bounds how long shutdown can take.
