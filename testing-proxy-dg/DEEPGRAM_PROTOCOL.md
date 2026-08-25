# Deepgram `/v1/listen` Protocol Reference

Protocol spec for Deepgram's transcription endpoint, **derived from live
wire captures** taken through `dgproxy` (see `logs/`) on 2026-08-25.

Every schema and behavior below is marked:

- ✅ **Captured** — observed verbatim on the wire in these sessions.
- ⚠️ **Documented only** — from Deepgram's public docs, **not** present in
  these captures; verify before relying on exact shape.

Captured sessions (all realtime WS, `model=nova-2-phonecall`):

| Session | Duration | Binary frames | Text frames (S→C) | Ended by |
|---|---|---|---|---|
| `20260825-141007-7ee0` | 12.5s | 0 | 1 (Metadata) | server timeout |
| `20260825-141007-e688` | 12.5s | 0 | 1 (Metadata) | server timeout |
| `20260825-141033-8ec0` | 40.8s | 257 | 34 | server timeout |
| `20260825-141033-c530` | 40.7s | 257 | 34 | server timeout |
| `20260825-141130-3030` | 56.8s | 406 | 55 | server timeout |
| `20260825-141130-41c8` | 56.8s | 406 | 54 | server timeout |

---

## 1. Endpoints & authentication

| Mode | Endpoint | Captured |
|---|---|---|
| Realtime streaming | `wss://api.deepgram.com/v1/listen?<query>` | ✅ |
| Pre-recorded | `POST https://api.deepgram.com/v1/listen?<query>` | ⚠️ not yet captured |

Auth methods (client → server):

- ✅ `Authorization: Token <API_KEY>` header (captured passthrough)
- ⚠️ `?token=<API_KEY>` query param (for browsers that can't set headers)
- ⚠️ WebSocket subprotocols: `Sec-WebSocket-Protocol: token, <API_KEY>`

Keys never appear in request paths; the API key is purely header/param based.

---

## 2. Realtime connection lifecycle ✅

Observed sequence for a healthy session:

```
C→S  HTTP GET /v1/listen?<params>  (Upgrade: websocket, Authorization header)
S→C  101 Switching Protocols                     [+0.13–0.29s observed]
C→S  binary audio frames (stream begins)
S→C  {"type":"Results", ...} interim results     [first at ~+1.1s observed]
     ... interleaved audio (C→S) / Results (S→C) ...
S→C  {"type":"Metadata", ...} session summary    [on termination]
S→C  Close frame
```

Termination paths observed:

| Trigger | Behavior |
|---|---|
| ✅ ~10–12s with no audio or text from client | Server sends final `Metadata` (with `duration`, `models`, `model_info`), then close **1011** with reason `Deepgram did not receive audio data or a text message within the timeout window. See https://dpgr.am/net0001` |
| ⚠️ Client sends `{"type":"CloseStream"}` | Server sends final `Results` + `Metadata`, then close 1000 (docs; not captured — client in these sessions never sent control messages) |

⚠️ Keepalive: docs say send `{"type":"KeepAlive"}` (text) to reset the
inactivity timeout during silence. Not captured.

---

## 3. Query parameters

✅ **Captured in use** (all sessions):

| Param | Value used | Meaning |
|---|---|---|
| `model` | `nova-2-phonecall` | Model selection |
| `encoding` | `mulaw` | Audio encoding of binary frames |
| `sample_rate` | `8000` | Sample rate of the streamed audio |
| `interim_results` | `true` | Emit non-final `Results` while speaking |
| `endpointing` | `200` | ms of silence before `speech_final=true` |
| `smart_format` | `true` | Formatting (punctuation, numbers, etc.) |

⚠️ **Documented, not captured:** `channels`, `punctuate`, `vad_events`,
`utterance_end_ms`, `finalize` (see §6 control messages), `diarize`,
`multichannel`, `language`, `tier`, `keywords`, `redact`, `filler_words`,
`profanity_filter`, `numerals`, `mip_opt_out`, `tag`, `extra`.

---

## 4. Client → Server messages

### 4.1 Binary audio frames ✅

Raw audio in the negotiated `encoding`/`sample_rate`, one chunk per binary
WebSocket frame. Observed with `mulaw`/`8000`:

- Chunk sizes: **2080, 960, 800, 480 bytes** (plus a 27-byte tail) —
  i.e. ~60–260 ms of audio per frame at 8 kHz (1 byte/sample).
- Send cadence roughly real-time; Deepgram tolerates arbitrary chunking.
- No framing/headers inside the payload — just raw samples.

### 4.2 Text control messages ⚠️ (documented, not captured)

| Message | Effect |
|---|---|
| `{"type":"CloseStream"}` | Signal end of audio; server finishes processing, sends final `Results` + `Metadata`, closes |
| `{"type":"KeepAlive"}` | Reset the inactivity timeout without sending audio |
| `{"type":"Finalize"}` | Force the last interim result to become `is_final=true` immediately; the resulting `Results` has `"from_finalize":true` |

---

## 5. Server → Client messages

All server messages are JSON text frames with a `type` discriminator.
Observed types: `Results`, `Metadata`. Documented-only: `UtteranceEnd`,
`SpeechStarted`, `Error`.

### 5.1 `Results` ✅

The core transcript message. All four variants below were captured.

**Fields (all captured):**

| Field | Type | Notes |
|---|---|---|
| `type` | `"Results"` | discriminator |
| `channel_index` | `[int, int]` | e.g. `[0,1]` — channel / total channels |
| `duration` | float | seconds of audio processed so far |
| `start` | float | seconds; offset of this segment in the stream |
| `is_final` | bool | transcript for this segment won't change |
| `speech_final` | bool | endpointing decided the utterance ended |
| `from_finalize` | bool | true when caused by a `Finalize` control msg (always `false` in captures) |
| `channel.alternatives[]` | array | exactly 1 alternative observed |
| `channel.alternatives[0].transcript` | string | full segment transcript (may be `""`) |
| `channel.alternatives[0].confidence` | float | 0.0–1.0 (0.0 when empty) |
| `channel.alternatives[0].words[]` | array | word-level timing (empty during silence) |
| `metadata.request_id` | uuid | matches the session `Metadata.request_id` |
| `metadata.model_info` | object | `{name, version, arch}` |
| `metadata.model_uuid` | uuid | |

**Word object (captured):**

```json
{"word":"this","start":14.08,"end":14.4,"confidence":0.95703125,"punctuated_word":"This"}
```

`word` is lowercase raw; `punctuated_word` carries capitalization and
trailing punctuation (`smart_format=true` in effect).

**Interim, no speech yet** (empty interim results stream continuously even
during silence): ✅

```json
{"type":"Results","channel_index":[0,1],"duration":1.0799375,"start":0.0,"is_final":false,"speech_final":false,"channel":{"alternatives":[{"transcript":"","confidence":0.0,"words":[]}]},"metadata":{"request_id":"01a03ac3-92a1-79f0-ad85-2cee59754422","model_info":{"name":"2-phonecall-nova","version":"2024-02-07.20824","arch":"nova-2"},"model_uuid":"7e3b5bdf-85ed-4fd2-9f7a-7721bbcad97b"},"from_finalize":false}
```

**Interim with words** (`is_final=false`): ✅

```json
{"type":"Results","channel_index":[0,1],"duration":3.119938,"start":12.08,"is_final":false,"speech_final":false,"channel":{"alternatives":[{"transcript":"This is the","confidence":0.95703125,"words":[{"word":"this","start":14.08,"end":14.4,"confidence":0.95703125,"punctuated_word":"This"},{"word":"is","start":14.4,"end":14.8,"confidence":0.9970703,"punctuated_word":"is"},{"word":"the","start":14.8,"end":15.199938,"confidence":0.43286133,"punctuated_word":"the"}]}]},"metadata":{"request_id":"01a03ac2-b34c-7433-9df5-62b0600b10bf","model_info":{"name":"2-phonecall-nova","version":"2024-02-07.20824","arch":"nova-2"},"model_uuid":"7e3b5bdf-85ed-4fd2-9f7a-7721bbcad97b"},"from_finalize":false}
```

**Final with `speech_final=true`** (endpointing triggered; note interim
transcript can be revised in the final — compare to the interim above): ✅

```json
{"type":"Results","channel_index":[0,1],"duration":4.7700005,"start":12.08,"is_final":true,"speech_final":true,"channel":{"alternatives":[{"transcript":"This is the Oh, no. I meant all then.","confidence":0.8676758,"words":[{"word":"this","start":14.08,"end":14.4,"confidence":0.9482422,"punctuated_word":"This"},{"word":"is","start":14.4,"end":14.639999,"confidence":0.9970703,"punctuated_word":"is"},{"word":"the","start":14.639999,"end":14.799999,"confidence":0.8569336,"punctuated_word":"the"},{"word":"oh","start":14.96,"end":15.04,"confidence":0.6854248,"punctuated_word":"Oh,"},{"word":"no","start":15.04,"end":15.2,"confidence":0.9951172,"punctuated_word":"no."},{"word":"i","start":15.2,"end":15.5199995,"confidence":0.95947266,"punctuated_word":"I"},{"word":"meant","start":15.5199995,"end":15.555,"confidence":0.8676758,"punctuated_word":"meant"},{"word":"all","start":15.84,"end":16.08,"confidence":0.27124023,"punctuated_word":"all"},{"word":"then","start":16.08,"end":16.58,"confidence":0.8317871,"punctuated_word":"then."}]}]},"metadata":{"request_id":"01a03ac2-b34c-7433-9df5-62b0600b10bf","model_info":{"name":"2-phonecall-nova","version":"2024-02-07.20824","arch":"nova-2"},"model_uuid":"7e3b5bdf-85ed-4fd2-9f7a-7721bbcad97b"},"from_finalize":false}
```

**Final without `speech_final`** (segment boundary without endpointing
trigger): ✅

```json
{"type":"Results","channel_index":[0,1],"duration":4.460001,"start":21.63,"is_final":true,"speech_final":false,"channel":{"alternatives":[{"transcript":"I guess we'll see what the output is at the end, but hopefully, it works.","confidence":0.9902344,"words":[{"word":"i","start":21.869999,"end":21.949999,"confidence":0.41015625,"punctuated_word":"I"},{"word":"guess","start":21.949999,"end":22.269999,"confidence":0.9633789,"punctuated_word":"guess"},{"word":"we'll","start":22.269999,"end":22.509998,"confidence":0.88549805,"punctuated_word":"we'll"},{"word":"see","start":22.509998,"end":22.589998,"confidence":0.99902344,"punctuated_word":"see"},{"word":"what","start":22.589998,"end":22.75,"confidence":0.99902344,"punctuated_word":"what"},{"word":"the","start":22.75,"end":23.07,"confidence":0.80322266,"punctuated_word":"the"},{"word":"output","start":23.07,"end":23.39,"confidence":1.0,"punctuated_word":"output"},{"word":"is","start":23.39,"end":23.47,"confidence":0.99902344,"punctuated_word":"is"},{"word":"at","start":23.47,"end":23.63,"confidence":0.99316406,"punctuated_word":"at"},{"word":"the","start":23.63,"end":23.869999,"confidence":0.9902344,"punctuated_word":"the"},{"word":"end","start":23.869999,"end":24.109999,"confidence":0.8725586,"punctuated_word":"end,"},{"word":"but","start":24.109999,"end":24.349998,"confidence":1.0,"punctuated_word":"but"},{"word":"hopefully","start":24.349998,"end":24.849998,"confidence":0.63623047,"punctuated_word":"hopefully,"},{"word":"it","start":24.91,"end":25.15,"confidence":1.0,"punctuated_word":"it"},{"word":"works","start":25.15,"end":25.65,"confidence":0.98950195,"punctuated_word":"works."}]}]},"metadata":{"request_id":"01a03ac2-b34c-7433-9df5-62b0600b10bf","model_info":{"name":"2-phonecall-nova","version":"2024-02-07.20824","arch":"nova-2"},"model_uuid":"7e3b5bdf-85ed-4fd2-9f7a-7721bbcad97b"},"from_finalize":false}
```

Behavioral notes (captured):

- Interim `Results` arrive continuously (~1/second observed), including
  empty ones during silence.
- `start`/`duration` describe the segment window; word `start`/`end` are
  absolute stream offsets.
- An empty final (`transcript:""`, `is_final:true`, `speech_final:true`)
  is emitted when endpointing fires during pure silence. ✅
- `confidence` values are float32-ish (e.g. `0.9970703`, `0.27124023`).

### 5.2 `Metadata` ✅

Sent on connection termination (also documented to be available at stream
start). Two captured shapes:

**Minimal** (session that received no audio — note `duration: 0.0`, no
`models`/`model_info` maps): ✅

```json
{"type":"Metadata","transaction_key":"deprecated","request_id":"01a03ac2-4f93-7d63-8729-c20d350e2f6e","sha256":"incomplete","created":"2026-08-25T21:10:20.046Z","duration":0.0,"channels":1}
```

**Full** (normal session end): ✅

```json
{"type":"Metadata","transaction_key":"deprecated","request_id":"01a03ac3-92a1-79f0-ad85-2cee59754422","sha256":"incomplete","created":"2026-08-25T21:11:31.685Z","duration":44.259937,"channels":1,"models":["7e3b5bdf-85ed-4fd2-9f7a-7721bbcad97b"],"model_info":{"7e3b5bdf-85ed-4fd2-9f7a-7721bbcad97b":{"name":"2-phonecall-nova","version":"2024-02-07.20824","arch":"nova-2"}}}
```

| Field | Notes |
|---|---|
| `transaction_key` | always the literal `"deprecated"` in captures |
| `request_id` | uuid; same id echoed in every `Results.metadata.request_id` |
| `sha256` | `"incomplete"` for streaming |
| `created` | RFC 3339 session start |
| `duration` | total seconds of audio processed |
| `channels` | channel count |
| `models[]` / `model_info{}` | present only when audio was processed |

### 5.3 Close frames ✅

Server-initiated close observed verbatim:

```
code=1011 reason="Deepgram did not receive audio data or a text message
within the timeout window. See https://dpgr.am/net0001"
```

Observed ~12s after last client data in all six sessions. The server sends
the final `Metadata` immediately **before** the close frame.

### 5.4 `UtteranceEnd` / `SpeechStarted` / `Error` ⚠️

Documented but **not captured** (require `vad_events=true` /
`utterance_end_ms`, and an error condition respectively). Documented shapes:

```json
{"type":"UtteranceEnd","channel":[0,1],"last_word_end":16.58}
{"type":"SpeechStarted","channel":[0,1],"timestamp":12.08}
{"type":"Error","message":"...","variant":"...","description":"..."}
```

---

## 6. Pre-recorded HTTP ⚠️ (documented, not captured)

`POST https://api.deepgram.com/v1/listen?<same query params>` with the audio
file as the request body (`Content-Type: audio/<format>`) or
`{"url":"https://..."}` JSON body for URL fetch. Response is a single JSON
document with `metadata` + `results.channels[].alternatives[]` (same
`transcript`/`confidence`/`words` schema as §5.1). **Run one through the
proxy to replace this section with captured truth.**

---

## 7. Timing observations ✅

From the six captured sessions:

| Measurement | Observed |
|---|---|
| WS handshake (open → 101) | 131–293 ms |
| First `Results` after audio starts | ~1.1 s |
| Interim result cadence | ~1/second during streaming |
| net0001 inactivity timeout | ~12 s after last client frame (audio or text) |
| Session teardown | final `Metadata` → close 1011, within ~25 ms |

---

## 8. Capture gaps — to fill with future dgproxy runs

1. **Graceful close**: send `{"type":"CloseStream"}` and capture final
   `Results` + `Metadata` + close 1000.
2. **`Finalize`**: capture a `Results` with `"from_finalize":true`.
3. **`vad_events=true` + `utterance_end_ms`**: capture `SpeechStarted` /
   `UtteranceEnd`.
4. **Pre-recorded POST**: capture the full HTTP response document.
5. **Error paths**: invalid model name / bad encoding → capture `Error`
   frame or handshake rejection body.
6. **KeepAlive**: verify it actually resets the net0001 timer.
7. **Multichannel/diarize**: capture `channel_index` and speaker fields.

---

*Generated from `testing-proxy-dg/logs/*.jsonl` (dgproxy MITM captures,
2026-08-25). Re-run captures to verify ⚠️ sections.*
