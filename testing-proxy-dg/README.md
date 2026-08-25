# testing-proxy-dg

A man-in-the-middle capture proxy for the Deepgram `/v1/listen` API. It
relays traffic verbatim between your client and the real Deepgram API while
logging every text frame, HTTP response, and connection event — so you can
capture the ground-truth wire format (both realtime WebSocket and
pre-recorded HTTP) and diff it against gotranscribesrv's implementation.

- **Auth: pure passthrough.** Whatever credentials the client sends
  (`Authorization: Token <key>`, `?token=` query param, or the `token`
  WebSocket subprotocol) are forwarded upstream unchanged. The key is
  redacted in logs.
- **No audio capture.** Binary audio frames are relayed untouched and logged
  as one-line metadata (`direction`, `bytes`, timestamp) so the trace still
  shows audio cadence vs. results timing.
- **`/v1/listen` only.** WebSocket upgrades → WS MITM; plain HTTP (e.g.
  pre-recorded `POST`) → HTTP passthrough. All other paths return 404.

## Build & run

```sh
cd testing-proxy-dg
go build -o dgproxy .
./dgproxy
```

Flags (env fallback in parentheses):

| Flag | Env | Default |
|---|---|---|
| `-listen` | `DGPROXY_LISTEN` | `:9090` |
| `-upstream` | `DGPROXY_UPSTREAM` | `wss://api.deepgram.com/v1/listen` |
| `-log-dir` | `DGPROXY_LOG_DIR` | `./logs` |

## Usage

Point any Deepgram client at the proxy instead of `api.deepgram.com`, using
your real API key as usual.

**Pre-recorded (HTTP):**

```sh
curl -X POST "http://localhost:9090/v1/listen?model=nova-3&smart_format=true" \
  -H "Authorization: Token $DEEPGRAM_API_KEY" \
  -H "Content-Type: audio/mpeg" \
  --data-binary @../sample_test.mp3
```

**Realtime (WebSocket):**

```sh
websocat "ws://localhost:9090/v1/listen?model=nova-3&encoding=linear16&sample_rate=16000&channels=1&interim_results=true" \
  -H "Authorization: Token $DEEPGRAM_API_KEY" \
  --binary < audio-linear16-16k.raw
```

**Deepgram SDKs:** override the base URL / host to
`http://localhost:9090` (or `ws://localhost:9090`) in the client config;
keep your normal API key.

## Trace output

Each connection writes one JSONL file (`logs/<timestamp>-<id>.jsonl`) and
mirrors a human-readable version to the console:

```json
{"ts":"...","dir":"C->S","kind":"ws_open","payload":"/v1/listen?model=nova-3&..."}
{"ts":"...","dir":"C->S","kind":"ws_binary","bytes":640}
{"ts":"...","dir":"S->C","kind":"ws_text","payload":"{\"type\":\"Results\",\"channel\":{...}}"}
{"ts":"...","dir":"C->S","kind":"ws_text","payload":"{\"type\":\"CloseStream\"}"}
{"ts":"...","dir":"S->C","kind":"ws_close","code":1000,"reason":""}
{"ts":"...","dir":"S->C","kind":"http_response","status":200,"payload":"{...}"}
```

Event kinds: `ws_open`, `ws_handshake`, `ws_handshake_error`, `ws_text`,
`ws_binary`, `ws_close`, `ws_error`, `ws_session_end`, `http_request`,
`http_response`, `http_error`. Directions: `C->S` (client → Deepgram),
`S->C` (Deepgram → client).

Upstream handshake failures (bad key, bad params) are captured with
Deepgram's actual error response body and propagated to the client — useful
for the cases Deepgram's docs don't cover.

## Tests

```sh
go test ./...
```

Uses a fake in-process upstream to verify frame relay, auth/query
passthrough, trace capture, and handshake-error propagation.
