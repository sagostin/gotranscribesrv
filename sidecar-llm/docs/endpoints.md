# HTTP endpoints

11 routes in total. Each entry below covers **method/path**, the request
shape (with a field table), the response shape, a working `curl` example,
notes on streaming variants where applicable, and the errors you'd
typically see.

The server speaks two dialects:

- **OpenAI-flavored** under `/v1/*` — `chat/completions`, `completions`,
  `embeddings`, `images/generations`, `models`.
- **Anthropic-flavored** under `/v1/messages` — full Messages API + SSE
  streaming.
- **Management** under `/models/:id/{download,load,unload,status}` and
  `/health`. Works across all runtimes.

> **Gateway:** in the full GoTranscribeSrv deployment this sidecar stays
> unauthenticated on localhost; the Go server (`../cmd/server`) proxies
> these same paths publicly with JWT/API-key auth, rate limiting, and
> per-model token usage tracking. Model management is proxied admin-only
> under `/api/v1/admin/llm/models/:id/*`. Develop against this sidecar
> directly (`api_key` can be anything); integrate against the Go gateway.

Quick links: [OpenAI chat](#openai-chat) · [OpenAI legacy completions](#openai-legacy-completions) ·
[OpenAI embeddings](#openai-embeddings) · [OpenAI images](#openai-images) ·
[OpenAI models](#openai-models) · [Anthropic](#anthropic) ·
[Management](#management-routes) · [Errors](#error-envelope)

For exact JSON shapes and SSE event sequences, see [formats.md](formats.md).

---

## `GET /health`

Liveness probe.

| | |
|---|---|
| Request | — |
| Response | `200 { "status": "ok" }` |

```bash
curl 127.0.0.1:8080/health
# {"status":"ok"}
```

---

## Management routes

These four routes take the model id as a path parameter and work for any
registered entry (`chat` / `image` / `embedding`, any runtime). Status
transitions are documented in [operations.md — Status state machine](operations.md#status-state-machine).

### `GET /models/{id}/status`

| | |
|---|---|
| Response | `200 { "id": "...", "status": "..." }` |

`status` is one of: `not_downloaded`, `downloading(N%)`, `downloaded`,
`compiling`, `loading`, `ready`, `failed: <message>` (see
[formats.md — status labels](formats.md#status-labels)).

```bash
curl 127.0.0.1:8080/models/mistral-7b-int4/status
# {"id":"mistral-7b-int4","status":"ready"}
```

Poll this until it returns `"ready"` to watch a first-run download/compile.

### `POST /models/{id}/download`

| | |
|---|---|
| Response | `202 { "id": "...", "status": "download_started" }` |
| Errors | `404 unknown model`, `404 unknown model id` |

Downloads the model's files without loading. Works even when
`autoDownload` is `false` (which would otherwise cause the first request to
the model to 409). The download runs in the background; poll
`/models/{id}/status` until `downloaded` or `ready`.

```bash
curl -X POST 127.0.0.1:8080/models/gemma-4-E2B/download
# {"id":"gemma-4-E2B","status":"download_started"}
```

### `POST /models/{id}/load`

| | |
|---|---|
| Response | `200 { "id": "...", "status": "ready" }` on success |
| Errors | `404`, `400 packageNotFound`, `400 wrongKind`, `500` on compile failure |

Forces compilation + load + warm-up without making a generation request.
Useful for warming a model ahead of a benchmark.

```bash
curl -X POST 127.0.0.1:8080/models/mistral-7b-int4/load
# {"id":"mistral-7b-int4","status":"ready"}
```

### `POST /models/{id}/unload`

| | |
|---|---|
| Response | `200 { "id": "...", "status": "unloaded" }` |
| Side effect | Drops the resident runner (LRU-slot freed). Status reverts to `downloaded` (or stays `failed` if the entry had failed). |

```bash
curl -X POST 127.0.0.1:8080/models/mistral-7b-int4/unload
# {"id":"mistral-7b-int4","status":"unloaded"}
```

---

## OpenAI models

### `GET /v1/models`

List registry entries with live status.

| | |
|---|---|
| Response | `200 { "object": "list", "data": [ <model>, ... ] }` |

Each `<model>`:

```jsonc
{
  "id":        "mistral-7b-int4",
  "object":    "model",
  "created":   0,
  "owned_by":  "local",
  "kind":      "chat",          // chat | image | embedding
  "runtime":   "standard",      // standard | coreml-llm  (chat only)
  "repo":      "apple/mistral-coreml",
  "status":    "ready",
  "preload":   true,
  "notes":     "..."
}
```

```bash
curl 127.0.0.1:8080/v1/models | jq
```

---

## OpenAI chat

### `POST /v1/chat/completions`

The workhorse endpoint. Streams by default when `stream: true`.

**Request fields**

| Field | Type | Default | Notes |
|---|---|---|---|
| `model` | string | required | Registry id (e.g. `mistral-7b-int4`). |
| `messages` | array | required | OpenAI shape — `{role, content, tool_calls?, tool_call_id?}`. Roles: `system`, `user`, `assistant`, `tool`. |
| `stream` | bool | `false` | If `true`, response is `text/event-stream` (SSE). |
| `max_tokens` | int | registry's `maxNewTokens` (default 512) | Generation cap. |
| `temperature` | double | `0` | Sampling temperature; 0 = greedy. |
| `top_k` | int | `50` | Top-k cutoff for sampling. |
| `tools` | array | — | OpenAI function schema. Triggers tool-calling prompt rendering. |
| `tool_choice` | string\|object | — | Pass `"none"` to disable tools for this turn; otherwise the `tools[]` array is used. |

**Non-streaming response**

```jsonc
{
  "id":      "chatcmpl-<uuid>",
  "object":  "chat.completion",
  "created": 1737000000,
  "model":   "mistral-7b-int4",
  "choices": [{
    "index":         0,
    "finish_reason": "stop" | "length" | "tool_calls",
    "message":       { "role": "assistant", "content": "...", "tool_calls"?: [...] }
  }],
  "usage": { "prompt_tokens": N, "completion_tokens": M, "total_tokens": N+M }
}
```

**Streaming response (`text/event-stream`)**

`Content-Type: text/event-stream`. Each frame is one OpenAI chunk ending in
`\n\n`. The terminal frame is `data: [DONE]\n\n`. See
[formats.md — OpenAI chat streaming](formats.md#openai-chat-streaming) for the
exact frame shapes.

**Tool calling**

If `tools[]` is present and the model emits a tool call, the response has
`finish_reason: "tool_calls"` and `message.tool_calls` is populated:

```jsonc
{
  "choices": [{
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "content": null,
      "tool_calls": [{
        "id":       "call_aB3xY7qZm",
        "type":     "function",
        "function": { "name": "calculator", "arguments": "{\"expression\":\"7*6\"}" }
      }]
    }
  }]
}
```

Execute the call on the client, then send the conversation back with the
assistant message (including `tool_calls`) plus a `role: "tool"` message
linking back via `tool_call_id`. The server never executes tools — that's the
client's job.

**Examples**

```bash
# Plain chat
curl 127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"mistral-7b-int4",
    "max_tokens":40,
    "messages":[{"role":"user","content":"Capital of Japan? One word."}]
  }'
# {"choices":[{"message":{"content":"Tokyo."}}],"usage":{"prompt_tokens":14,"completion_tokens":2,...}}

# Streaming
curl -N 127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"mistral-7b-int4","max_tokens":30,"stream":true,
    "messages":[{"role":"user","content":"Count to five."}]
  }'
# data: {"choices":[{"delta":{"content":"One"},"finish_reason":null}]}
# data: {"choices":[{"delta":{"content":", two"},"finish_reason":null}]}
# ...
# data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
# data: [DONE]

# Tool round-trip (Mistral 7B Int4)
curl 127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"mistral-7b-int4",
    "messages":[{"role":"user","content":"What is 7*6?"}],
    "tools":[{
      "type":"function",
      "function":{
        "name":"calculator",
        "parameters":{"type":"object","properties":{"expression":{"type":"string"}}}
      }
    }]
  }'
# finish_reason: "tool_calls" → execute → re-send with role:"tool" → final answer "42"
```

**OpenAI Python SDK**

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="not-used")

resp = client.chat.completions.create(
    model="mistral-7b-int4",
    messages=[{"role": "user", "content": "Capital of Japan? One word."}],
    max_tokens=20,
)
print(resp.choices[0].message.content)  # "Tokyo."

# Streaming
stream = client.chat.completions.create(
    model="mistral-7b-int4", max_tokens=30, stream=True,
    messages=[{"role": "user", "content": "Count to five."}],
)
for chunk in stream:
    d = chunk.choices[0].delta.content
    if d: print(d, end="", flush=True)
```

**Errors**

- `404 unknown model` — `model` not in registry.
- `409 auto_download_disabled` — model isn't downloaded and
  `settings.autoDownload` is `false`; call `POST /models/{id}/download`
  first.
- `400 incompatible_layout` — usually means the compiled model is corrupt;
  delete `Models/compiled/<id>.mlmodelc/` and reload.
- `400 wrongKind` — model isn't a chat entry.
- `500 server_error` — generation threw; see the server log.

For the `coreml-llm` runtime (`gemma-4-E2B`), tool calls are still surfaced
in the same OpenAI shape; the wire format the gemma model emits is parsed in
`Sources/ExternalRuntime/Gemma4ToolsSupport.swift`.

---

## OpenAI legacy completions

### `POST /v1/completions`

Raw prompt → text completion. No chat template, no tools. Best for base
models like `qwen3-1.7b-w8` whose chat template would emit EOS first.

**Request fields**

| Field | Type | Default | Notes |
|---|---|---|---|
| `model` | string | required | |
| `prompt` | string | required | The raw prompt text. Tokens are produced by the model's tokenizer. |
| `stream` | bool | `false` | |
| `max_tokens` | int | 128 | |
| `temperature` | double | `0` | |
| `top_k` | int | `50` | |

**Non-streaming response**

```jsonc
{
  "id": "cmpl-<uuid>",
  "object": "text_completion",
  "created": 1737000000,
  "model": "qwen3-1.7b-w8",
  "choices": [{ "index": 0, "text": "...", "finish_reason": "stop" | "length" }],
  "usage": { "prompt_tokens": N, "completion_tokens": M, "total_tokens": N+M }
}
```

**Streaming response** — `text/event-stream` frames with the same
`object: text_completion` shape. See
[formats.md — legacy completions streaming](formats.md#openai-legacy-completions-streaming).

```bash
curl 127.0.0.1:8080/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-1.7b-w8","prompt":"The capital of France is","max_tokens":10}'
```

---

## OpenAI embeddings

### `POST /v1/embeddings`

L2-normalized sentence embeddings from any registered
`kind: "embedding"` entry.

**Request fields**

| Field | Type | Notes |
|---|---|---|
| `model` | string | Required. Must be an embedding entry. |
| `input` | string \| string[] | A single string or an array of strings. |

**Response**

```jsonc
{
  "object": "list",
  "data": [
    { "object": "embedding", "index": 0, "embedding": [0.014, -0.027, ..., 0.003] },
    { "object": "embedding", "index": 1, "embedding": [...] }
  ],
  "model": "all-minilm-l6-v2",
  "usage": { "prompt_tokens": 3, "total_tokens": 3 }
}
```

Returns `403 feature_disabled` if `settings.features.embeddings` is `false`
or the entry's architecture isn't supported.

```bash
curl 127.0.0.1:8080/v1/embeddings \
  -H 'Content-Type: application/json' \
  -d '{"model":"all-minilm-l6-v2","input":["hello world","goodbye"]}' | jq
```

---

## OpenAI images

### `POST /v1/images/generations`

Stable Diffusion 1.5 / SDXL generation. The native size is locked per-model;
mismatched `size` → 400.

**Request fields**

| Field | Type | Default | Notes |
|---|---|---|---|
| `model` | string | required | A registered `kind: "image"` entry. |
| `prompt` | string | required | |
| `negative_prompt` | string | `""` | |
| `n` | int | `1` | Number of images to generate. |
| `size` | string | (native) | Must equal `"<imageSize>x<imageSize>"`, e.g. `"512x512"` or `"1024x1024"`. |
| `steps` | int | `25` | Number of diffusion steps. |
| `guidance_scale` | double | `7.5` | CFG scale. |
| `seed` | uint | random | For reproducible outputs. |
| `response_format` | string | — | Always `b64_json` (URL not implemented). |

**Response**

```jsonc
{
  "created": 1737000000,
  "data": [{ "b64_json": "iVBORw0KGgoAA..." }, ...]
}
```

Returns `403 feature_disabled` if `settings.features.images` is `false`.

```bash
curl 127.0.0.1:8080/v1/images/generations \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"sd-1.5","prompt":"a red apple on wood","steps":15,"seed":42
  }' | jq -r '.data[0].b64_json' | base64 -d > apple.png
```

---

## Anthropic

### `POST /v1/messages`

Anthropic Messages API + streaming. Works with any registered chat entry.
For `runtime: "coreml-llm"` entries (gemma), the tool format is parsed
internally and emitted as `tool_use` blocks on the wire.

**Required headers**

- `Content-Type: application/json`
- `anthropic-version: 2023-06-01` — accepted and ignored (version is not enforced).
- `x-api-key: <anything>` — accepted and ignored (no auth).

**Request fields**

| Field | Type | Default | Notes |
|---|---|---|---|
| `model` | string | required | |
| `messages` | array | required | `[{role: "user"\|"assistant", content: string\|ContentBlock[]}]`. |
| `system` | string\|ContentBlock[] | — | Top-level system prompt. |
| `max_tokens` | int | entry's `maxNewTokens` | Required by the spec; we default if you omit it. |
| `stream` | bool | `false` | Enables SSE event sequence. |
| `temperature` | double | `0` | |
| `top_k` | int | `50` | |
| `tools` | array | — | `[{name, description, input_schema}]`. |
| `tool_choice` | object | — | `{type: "none"}` disables tools. |
| `stop_sequences` | string[] | — | Currently not enforced end-to-end (model-side EOS only). |

**Non-streaming response**

```jsonc
{
  "id": "msg_<24-chars>",
  "type": "message",
  "role": "assistant",
  "model": "mistral-7b-int4",
  "content": [
    { "type": "text", "text": "..." }
    // OR, when tools are used:
    // { "type": "text",       "text": "<prefix before tool marker>" },
    // { "type": "tool_use",   "id": "toolu_<9-chars>", "name": "...", "input": { ... } }
  ],
  "stop_reason":  "end_turn" | "max_tokens" | "tool_use",
  "stop_sequence": null,
  "usage": { "input_tokens": N, "output_tokens": M }
}
```

**Streaming response (SSE event sequence)**

```
event: message_start
data: {"type":"message_start","message":{... id, role, model, content:[], usage:...}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":N}}

event: message_stop
data: {"type":"message_stop"}
```

For tool use, additional `content_block_start` blocks with
`{"type":"tool_use","id":"...","name":"...","input":{}}` arrive, followed by
`input_json_delta` partial-JSON chunks, then `content_block_stop`. See
[formats.md — Anthropic streaming](formats.md#anthropic-streaming).

**Anthropic Python SDK**

```python
import anthropic
client = anthropic.Anthropic(
    base_url="http://127.0.0.1:8080",
    api_key="not-used",            # server ignores auth headers
)
msg = client.messages.create(
    model="mistral-7b-int4",
    max_tokens=30,
    messages=[{"role": "user", "content": "Count to five."}],
)
print(msg.content[0].text)
```

**curl example**

```bash
curl 127.0.0.1:8080/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d '{
    "model":"mistral-7b-int4","max_tokens":30,"stream":true,
    "messages":[{"role":"user","content":"Count to five."}]
  }'
```

---

## Error envelope

All non-2xx responses (except plain aborts) use OpenAI's error envelope:

```jsonc
{ "error": { "message": "...", "type": "...", "code": null } }
```

`type` values you'll see: `invalid_request_error`, `feature_disabled`,
`server_error`. Streaming errors arrive as a single SSE frame
`data: {"error":"..."}` and then `data: [DONE]`. See
[operations.md — HTTP error code reference](operations.md#http-error-code-reference)
for the full table.
