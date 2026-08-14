# OpenAI Realtime — Transcription & Speech-to-Speech

How GoTranscribeSrv implements the OpenAI Realtime API family on fully
on-device models (CoreML / Apple Neural Engine): the wire protocol we speak,
the two session modes, the speech-to-speech pipeline and its latency budget,
and how tool (function) calling works end to end.

> **Status legend:** ✅ implemented · 🚧 designed, not yet implemented
> Both modes are live: transcription is always on; S2S (Phases 1–3 below) is
> opt-in via `REALTIME_S2S_ENABLED=true` + `?model=gpt-realtime*`.

---

## 1. Overview — two session modes

OpenAI's Realtime surface splits into two session types, and we mirror that
split exactly:

| Mode | OpenAI equivalent | What happens | Status |
|------|-------------------|--------------|--------|
| **Realtime transcription** | Transcription sessions (`gpt-4o-transcribe`, `gpt-4o-mini-transcribe`) | Audio in → streaming transcript events out. No LLM, no audio output. | ✅ Live on `WS /v1/realtime` |
| **Speech-to-speech (S2S)** | Realtime sessions (`gpt-realtime`, `gpt-realtime-mini`) | Audio in → ASR → LLM → TTS → audio out, with turn-taking, interruptions (barge-in), and client-side function calling. | ✅ Opt-in (`REALTIME_S2S_ENABLED=true`) |

Both modes share **one WebSocket endpoint** — `WS /v1/realtime` — and are
selected by the `model` in `session.update` (or the `?model=` query param),
exactly like OpenAI. A session that only ever sends audio and never triggers
`response.create` behaves as a pure transcription session; S2S activates when
the model maps to an S2S session or the client sends `response.create`.

```
                          WS /v1/realtime
                                │
                    ┌───────────┴────────────┐
                    ▼                        ▼
        Transcription session        Speech-to-speech session
        (gpt-4o-transcribe →         (gpt-realtime → ASR engine
         ASR engine only)             + LLM + TTS pipeline)
                    │                        │
        transcript events only      full event protocol incl.
                                    audio deltas + tool calls
```

### Why a cascaded pipeline (not end-to-end audio)

OpenAI's `gpt-realtime` is a single audio-to-audio model. Ours is a **cascade
of three on-device models** — streaming ASR (Parakeet EOU / Nemotron /
Unified), a chat LLM (Mistral 7B / Gemma 4, CoreML), and streaming TTS
(PocketTTS). That's what the hardware supports today, and it has real
advantages: every stage is independently swappable and observable, you get
true text transcripts of both sides for free, and tool calling is native to
the LLM stage rather than bolted on. The cost is latency, which is what the
rest of this doc is about minimizing.

---

## 2. Connection & authentication

**Connect:** `WS /v1/realtime?model=<openai-model-or-engine>&encoding=linear16&sample_rate=16000`

- Auth: standard JWT (`Authorization: Bearer …`) or API key
  (`X-API-Key: gtx_live_…`) — same as every other endpoint. (OpenAI uses an
  ephemeral-token minting step; our API keys already fill that role.)
- `?model=` is optional and can be overridden by the first `session.update`.
- On connect the server immediately sends `session.created` (per spec), then
  dials the audio sidecar's `/stream/realtime` with the resolved engine.

---

## 3. Mode 1 — Realtime transcription ✅

The current implementation (`internal/handlers/openai_realtime.go`) is a
protocol-translating proxy onto the audio sidecar's true-streaming engine
(cache-aware encoder states, incremental partials, EOU/VAD turn events).

### Model → engine mapping

| `session.model` (OpenAI-style)              | Sidecar engine  | Session type   |
|---------------------------------------------|-----------------|----------------|
| `gpt-4o-transcribe`                         | `eou-320`       | transcription  |
| `gpt-4o-mini-transcribe`                    | `nemotron-560`  | transcription  |
| `gpt-4o-realtime-preview`, `gpt-4o-realtime`| `eou-320`       | transcription¹ |
| `gpt-4o-mini-realtime-preview`              | `nemotron-560`  | transcription¹ |
| `gpt-realtime`, `gpt-realtime-mini`         | `eou-320`       | **S2S** ✅     |
| `nova-3`, `parakeet-unified-320`            | `unified-320`   | transcription  |
| anything containing `unified` / `nemotron` / `eou-` | mapped / pass-through | transcription |
| empty / unknown                             | server default (`eou-320`) | transcription |

¹ The legacy `gpt-4o-realtime*` aliases resolve to transcription sessions for
backwards compatibility with existing clients. S2S is reserved for the
`gpt-realtime*` family (and explicit opt-in, see §9).

### Events (transcription mode)

Client → server: `session.update`, `input_audio_buffer.append` (base64 PCM16),
`input_audio_buffer.commit` (ack-only), `input_audio_buffer.clear` (no-op).

Server → client: `session.created`, `session.updated`,
`input_audio_buffer.speech_started`, `input_audio_buffer.speech_stopped`,
`conversation.item.input_audio_transcription.delta` (per partial),
`conversation.item.input_audio_transcription.completed` (per final),
`input_audio_buffer.committed` (turn-end marker), `error`.

Sidecar event translation is 1:1 (`partial` → `.delta`, `final` →
`.completed`, VAD events → `speech_started/stopped`, `end_of_turn` →
`.committed` + fresh `item_id`). Usage is logged as `asr_openai_realtime`
(audio duration + processing ms).

Full event tables: [docs/api.md → WS /v1/realtime](api.md#ws-v1realtime-openai-realtime-compatible).

---

## 4. Mode 2 — Speech-to-speech ✅

> Implemented in `internal/handlers/openai_realtime_s2s.go`. Enable with
> `REALTIME_S2S_ENABLED=true`; clients connect with `?model=gpt-realtime`.
> The Go server orchestrates the whole loop — the sidecars never talk to
> each other and know nothing about sessions, turns, or tools.

### 4.1 Architecture

The orchestrator lives in the **Go server** (a sibling handler to the
existing transcription proxy), not in a Swift sidecar. Auth, rate limiting,
usage metering, and PII redaction already live in Go; the sidecars stay dumb,
single-purpose inference engines.

```
        Client
          │  WS (OpenAI Realtime events)
          ▼
┌─────────────────────────────────────────────────┐
│  Go orchestrator (internal/handlers/             │
│  openai_realtime_s2s.go)                         │
│                                                  │
│  Session state machine · turn-taking · barge-in  │
│  sentence splitter · TTS job queue · tool relay  │
└───┬──────────────┬──────────────────┬───────────┘
    │ WS           │ SSE              │ HTTP chunked stream
    ▼              ▼                  ▼
 audio sidecar   LLM sidecar      audio sidecar
 /stream/realtime /v1/chat/       /synthesize/stream
 (ASR + VAD + EOU) completions    (PocketTTS, 80 ms
                  (stream:true)    L16 24 kHz frames)
```

All three hops are localhost (or LAN in split deployments) — each adds well
under 5 ms of transport. The pipeline is **fully streaming end to end**:
audio streams in, tokens stream out of the LLM, sentences stream into TTS,
80 ms audio frames stream back to the client.

### 4.2 Turn-taking state machine

```
   IDLE ──speech_started──► LISTENING ──EOU / VAD speech_end──► THINKING
     ▲                        │                                   │
     │                        │ (partials → transcription.delta)  │ LLM SSE stream
     │                        ▼                                   ▼
     │                     (barge-in:                          SPEAKING
     │                      cancel LLM+TTS,                   (sentence N → TTS
     │                      emit response.cancelled,          → audio.delta)
     │                      back to LISTENING)                     │
     └─────────────────── response.done ◄── LLM stream end ◄──────┘
```

- **Turn end — fast path:** the Parakeet EOU engine detects end-of-utterance
  in the acoustic model itself (~0–200 ms after the utterance actually ends).
  This is why `eou-320` is the default S2S engine.
- **Turn end — fallback:** streaming Silero VAD `speech_stopped` + a short
  hangover timeout (configurable, default ~400 ms) for engines without EOU
  (Nemotron, Unified).
- **Only finals go to the LLM.** Partial transcripts are relayed to the
  client as `conversation.item.input_audio_transcription.delta` but never
  trigger generation. (Speculative pre-generation on high-confidence
  partials is a Phase-4 stretch — see §10.)
- **Barge-in:** a `speech_started` event while THINKING or SPEAKING cancels
  the in-flight LLM stream and flushes the TTS queue, emits
  `conversation.item.truncated` + `response.cancelled`, and returns to
  LISTENING. Server tracks how many audio frames were sent per item so
  `truncated.audio_end_ms` is accurate (clients use it to cut playback).

### 4.3 Response pipeline (THINKING → SPEAKING)

1. On turn end, the final transcript is appended to the conversation history
   (kept per-session in the orchestrator, seeded from
   `session.instructions` as the system prompt).
2. `POST /v1/chat/completions` to the LLM sidecar with `stream: true`, the
   session's `tools` (if any), and the conversation history.
3. **Token stream → sentence splitter.** Tokens accumulate until a clause
   boundary: terminal punctuation (`. ! ? …` / newline), or a soft boundary
   (`, ; : —`) once the buffer exceeds ~8 words. First chunk is emitted
   aggressively (min ~6 words) so TTS starts early; later chunks can be
   longer to keep TTS throughput high.
4. **TTS job queue.** Each sentence is POSTed to `/synthesize/stream` in
   order. Sentence N+1's HTTP request starts while sentence N's audio is
   still draining, so synthesis pipelines with playback.
5. Each 80 ms L16 24 kHz frame from TTS → base64 →
   `response.audio.delta` (OpenAI's `pcm16` output format is also 24 kHz —
   no resampling). Parallel `response.audio_transcript.delta` events carry
   the sentence text for the text modality.
6. LLM stream end → final TTS job drains → `response.audio.done`,
   `response.audio_transcript.done`, `response.output_item.done`,
   `response.done` (with token usage).
7. If the LLM's streamed output is a **tool call instead of speech**, no TTS
   jobs are created — the session goes IDLE waiting on the client's tool
   result (see §6).

---

## 5. Latency budget

End-to-end target on an M4 (dev-spec node): **~800 ms–1.2 s from end of
user speech to first audio byte out**, roughly what feels "instant" in a
voice conversation.

| Stage | Budget | Notes |
|-------|--------|-------|
| Turn detection (EOU engine) | 0–200 ms | Built into Parakeet EOU; VAD fallback adds ~300–500 ms |
| ASR final transcript | ~50 ms | Already incremental — final is a formality after EOU |
| Orchestrator overhead | <5 ms | Localhost WS + JSON |
| LLM time-to-first-token | 150–400 ms | Model-size dependent (7B Int4, CoreML stateful cache) |
| Tokens → first sentence | 50–150 ms | Split at ~6–10 words |
| TTS time-to-first-frame | ~80–150 ms | PocketTTS streams 80 ms frames as generated |
| **Total (typical)** | **~500 ms – 1 s** | worst case ~1.5 s with VAD fallback + cold LLM |

### Tuning knobs

| Knob | Effect |
|------|--------|
| `?engine=eou-160` | 160 ms chunks instead of 320 ms — snappier partials, slightly lower accuracy |
| Smaller LLM (Gemma 4 vs Mistral 7B) | Cuts TTFT; quality tradeoff |
| `session.max_response_output_tokens` | Bounds SPEAKING duration; keeps turns snappy |
| Keep-alive / warm models | `POST /api/v1/admin/llm/models/:id/load` at boot — avoids a cold-compile first turn |
| First-chunk aggressiveness | Shorter first sentence = earlier first audio (config constant) |
| `session.instructions` brevity | Shorter system prompt = fewer prefill tokens = lower TTFT |

### Concurrency impact

One S2S session holds an ASR stream + (during a turn) an LLM stream + a TTS
stream simultaneously. Budget **1 S2S session ≈ 2 plain ASR streams** in the
per-node capacity tables (README "Pricing & Infrastructure"). An M4 24 GB
node lands at ~3–4 concurrent S2S sessions; M4 Pro ~5–6.

---

## 6. Tool (function) calling — client-side ✅ design

**The server never executes tools.** The connecting client owns its tool
implementations, exactly like OpenAI's Realtime API and exactly like
sidecar-llm's existing pass-through chat tool calling (OpenAI/Anthropic
dialects already return `tool_calls` for the client to execute). The
orchestrator's job is purely to relay tool calls across the realtime event
protocol.

Why client-side is the right call here:

- **Security boundary** — tool code is client business logic (their DBs,
  their APIs). The server executing arbitrary tools would be a remote-code-
  execution surface by design.
- **Consistency** — sidecar-llm is already deliberately pass-through
  ("the backend never executes tools — that's the client's job").
- **OpenAI parity** — any client already written against OpenAI Realtime
  function calling works unchanged.

### Event flow

```
1. Client      → session.update          { session: { tools: [...], tool_choice: "auto" } }
2. (user speaks, turn ends; orchestrator forwards tools with the chat request)
3. LLM decides to call a tool (streamed tool_call, no text content)
4. Server      → response.output_item.added        (item.type = "function_call")
5. Server      → response.function_call_arguments.delta   (streamed JSON args)
6. Server      → response.function_call_arguments.done    (complete args, call_id, name)
7. Server      → response.output_item.done
8. Server     → response.done           (status "completed"; output contains
                                         the function_call item(s))
   — no audio is synthesized for this turn; session waits —
9. Client executes the tool locally, then:
10. Client     → conversation.item.create { item: { type: "function_call_output",
                                                  call_id: "...", output: "..." } }
11. Server     → conversation.item.created (ack)
12. Client     → response.create
13. Orchestrator appends the tool result to history, re-calls the LLM,
    and the normal SPEAKING pipeline resumes (steps 3–7 of §4.3).
```

Implementation notes:

- The LLM sidecar streams tool calls in OpenAI SSE form
  (`choices[0].delta.tool_calls`); the orchestrator accumulates
  `id`/`name`/`arguments` fragments across SSE chunks and re-emits them as
  Realtime `response.function_call_arguments.*` events.
- `tool_choice` (`auto`/`none`/`required`/named) is forwarded verbatim to the
  LLM sidecar, which already supports it.
- Parallel tool calls: relayed as multiple `output_item`s; the client must
  send one `function_call_output` item per `call_id` before
  `response.create`.
- While waiting on tool output the ASR stream stays live — user speech
  during a tool wait behaves as a normal new turn (and implicitly cancels
  the pending tool continuation).
- If a deployment ever needs server-executed tools, the sane shape is an
  explicit allowlist of webhook URLs in `session.update` — documented as a
  possible Phase 5, not part of the base design.

---

## 7. Full protocol event reference (S2S mode) ✅

### Client → server

| Event | Support | Notes |
|-------|---------|-------|
| `session.update` | ✅ | `model`, `instructions`, `voice`, `tools`, `tool_choice`, `turn_detection`, `max_response_output_tokens`, `modalities` |
| `input_audio_buffer.append` | ✅ | base64 PCM16, 16 kHz input (resampled internally as today) |
| `input_audio_buffer.commit` | ✅ | ack-only (auto-incremental engine) |
| `input_audio_buffer.clear` | ⚠️ | No-op (logged) — no sidecar flush API yet |
| `response.create` | ✅ | Force a response (e.g. after tool output, or text-only turn) |
| `response.cancel` | ✅ | Client-initiated cancel == barge-in handling |
| `conversation.item.create` | ✅ | `function_call_output` items (tools); `message` items (text turns) |
| `conversation.item.truncate` | 🚧 | Client playback-sync hint; not yet accepted |

### Server → client

| Event | When |
|-------|------|
| `session.created` / `session.updated` | Connect / config change |
| `input_audio_buffer.speech_started` / `speech_stopped` | VAD events (barge-in signal) |
| `input_audio_buffer.committed` | Turn boundary |
| `conversation.item.created` | New user/assistant/tool item |
| `conversation.item.input_audio_transcription.delta` / `.completed` | User-side partials / finals |
| `conversation.item.truncated` | Barge-in cut assistant audio |
| `response.created` | Generation begins |
| `response.output_item.added` / `.done` | Assistant message or function call item |
| `response.content_part.added` / `.done` | Audio/text part boundaries |
| `response.audio.delta` / `.done` | 80 ms PCM16 24 kHz chunks (base64) |
| `response.audio_transcript.delta` / `.done` | Assistant speech as text |
| `response.text.delta` / `.done` | Text modality |
| `response.function_call_arguments.delta` / `.done` | Tool call streaming |
| `response.done` | Turn complete — `status` `completed` (with token `usage`) or `cancelled` (with `status_details.reason` = `turn_detected` / `client_cancelled`) |
| `response.cancelled` | Barge-in / `response.cancel` (compat event — spec-shaped `response.done` with `status:"cancelled"` is also emitted) |
| `rate_limits.updated` | ⚠️ Not emitted — limits are enforced per-request by middleware (429s), no token-reservation concept |
| `error` | Spec shape: `error.type` is `invalid_request_error` (client) or `server_error` (upstream); `error.code` carries the specific code (`llm_unavailable`, `tts_unavailable`, `invalid_event`, …) |

Event payloads follow OpenAI's wire shapes as published in the OpenAI
OpenAPI spec (`RealtimeServerEvent*` schemas): `response.created` /
`response.done` carry the full `realtime.response` object (`object`,
`status`, `status_details`, `output`, `usage`), output items carry
`object: "realtime.item"` + `content` parts, `input_audio_buffer.committed`
and `conversation.item.created` carry `previous_item_id`, and turn-end
ordering is `committed` → `item.created` → `transcription.completed` →
`response.created`.

### `session.update` fields honored

| Field | Maps to |
|-------|---------|
| `model` | Session mode + ASR engine (§3 table) |
| `instructions` | LLM system prompt |
| `voice` | PocketTTS voice (17 built-ins or a stored cloned `voice_id`) |
| `tools`, `tool_choice` | Forwarded to LLM sidecar |
| `turn_detection.type` | `server_vad` (VAD fallback) / EOU is automatic with EOU engines; `none` = push-to-talk via `response.create` |
| `max_response_output_tokens` | LLM `max_tokens` |
| `modalities` | `["audio","text"]` or `["text"]` (text-only skips TTS) |
| `temperature` | LLM sampling temperature |

---

## 8. What we deliberately don't do

- **Server-executed tools** — see §6. Possible later opt-in via explicit
  webhook allowlist, never ambient execution.
- **Audio-in-prompt (multimodal audio understanding beyond ASR)** — the
  cascade only "hears" through the transcript. Prosody/emotion is lost;
  accepted tradeoff of the cascaded design.
- **`input_audio_buffer.commit`-driven manual VAD segmentation** — the
  engine is auto-incremental; push-to-talk clients use
  `turn_detection: none` + `response.create`.
- **Ephemeral token minting endpoint** — our API keys already are scoped,
  revocable credentials with per-key usage tracking.

---

## 9. Configuration & operations

New env vars (Go server):

| Var | Default | Purpose |
|-----|---------|---------|
| `REALTIME_S2S_ENABLED` | `false` (Phase 1), `true` later | Master switch for S2S mode |
| `REALTIME_S2S_MODEL` | `mistral-7b-int4` | LLM model for S2S turns |
| `REALTIME_S2S_VOICE` | `default` | PocketTTS voice for S2S audio |
| `REALTIME_S2S_MAX_TOKENS` | `300` | Per-turn response cap |
| `REALTIME_S2S_TEMPERATURE` | `0.7` | LLM sampling |
| `REALTIME_S2S_INTERRUPTIONS` | `true` | Barge-in on/off |

Existing relevant vars: `SIDECAR_REALTIME_ENGINE` (default ASR engine),
`ENABLE_LLM` / `LLM_SIDECAR_URL`, `SIDECAR_TTS_DEFAULT_BACKEND` (S2S always
uses the streaming backend = PocketTTS).

**Metrics (new Prometheus series):**
`gotranscribesrv_realtime_s2s_turn_latency_seconds` (histogram, end-of-speech
→ first audio byte), `..._llm_ttft_seconds`, `..._tts_first_chunk_seconds`,
`..._interruptions_total`, `..._tool_calls_total`.

**Usage tracking:** S2S sessions log ASR audio-in ms, TTS audio-out ms
(characters), and LLM prompt/completion tokens against the user + API key —
same `usage_logs` pipeline as the existing proxies.

**Tracing a turn across the pipeline:** every log line and Loki event carries
two correlation IDs —

- `request_id` — the WebSocket session (from the request-ID middleware,
  stable for the whole connection)
- `turn_id` — one per conversation turn (the `resp_*` response ID),
  appearing on the LLM-stream start, per-sentence TTS start, TTFT, turn
  completion, interruption, and tool-call logs

The Go server also sends `X-Request-ID: <turn_id>` on the LLM
`/v1/chat/completions` call and every `/synthesize/stream` sentence call, so
a single turn can be traced end to end: ASR `final` → LLM stream → TTS
sentences → `response.done`. Query Loki with `{job="gotranscribesrv"} |=
"turn_id=resp_abc123"` to see one turn's full chain.

**Health:** `/health` already reports sidecar reachability; S2S mode
additionally requires the LLM sidecar — if `ENABLE_LLM=false` or the LLM
sidecar is down, S2S sessions fail fast with an `error` event
(`llm_unavailable`) while transcription sessions keep working.

---

## 10. Implementation phases

| Phase | Scope | Status |
|-------|-------|--------|
| **1. Basic loop** | EOU turn-end → LLM SSE → sentence split → TTS stream → `response.audio.delta` | ✅ Shipped |
| **2. Barge-in** | Cancel LLM/TTS on `speech_started`; `response.cancelled`, `conversation.item.truncated`; `response.cancel` | ✅ Shipped |
| **3. Client-side tools** | `tools` forwarding, function-call event relay, `function_call_output` + `response.create` resume | ✅ Shipped |
| **4. Tuning** | First-chunk aggressiveness knobs, warm-model keep-alive, optional speculative pre-generation on stable partials, true `input_audio_buffer.clear` flush, `conversation.item.truncate` acceptance, pipelined TTS (start sentence N+1 synthesis while N drains), cloned-voice ID resolution in S2S | 🚧 Future |

Phases 1–3 run behind `REALTIME_S2S_ENABLED` and leave transcription
sessions untouched.

---

## 11. Testing strategy

- **Scripted WS client** (`scripts/test_realtime_s2s.sh` or a small Go
  harness): plays a WAV as `input_audio_buffer.append` frames, asserts
  event ordering (`response.created` → `audio.delta`+ → `audio.done` →
  `response.done`), and measures end-of-speech → first-`audio.delta`
  latency.
- **Tool round-trip test**: session with a mock `get_weather` tool; assert
  `function_call_arguments.done` args parse, feed a canned
  `function_call_output`, assert the spoken answer references it.
- **Barge-in test**: start playback, send audio mid-response, assert
  `response.cancelled` precedes any further `audio.delta`.
- **Soak**: 100 sequential turns watching goroutine count, sidecar memory,
  and event ordering under interleaved partials.

---

## 12. Backwards compatibility

- Existing `/v1/realtime` clients that never send `response.create` see
  **zero behavior change** — same events, same engine mapping, same usage
  logging.
- `gpt-4o-realtime*` model aliases stay mapped to transcription sessions;
  S2S requires `gpt-realtime*` (or `REALTIME_S2S_ENABLED` + explicit
  opt-in), so no existing client accidentally starts paying LLM/TTS costs.
- The native `/stream/realtime` and Deepgram `/v2/listen` endpoints are
  unaffected.
