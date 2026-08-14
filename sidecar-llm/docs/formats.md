# Wire formats

Companion to [endpoints.md](endpoints.md) — this document is the canonical
reference for the JSON shapes on the wire and the SSE event sequences. Most
users will only need the brief examples in `endpoints.md`; reach for this
when you're debugging shape mismatches, building a client, or writing a
tool-calling loop.

## Contents

- [OpenAI chat request](#openai-chat-request)
- [OpenAI chat response (non-stream)](#openai-chat-response-non-stream)
- [OpenAI chat streaming](#openai-chat-streaming)
- [OpenAI legacy completions](#openai-legacy-completions)
- [OpenAI embeddings](#openai-embeddings)
- [OpenAI images](#openai-images)
- [Anthropic messages request](#anthropic-messages-request)
- [Anthropic response (non-stream)](#anthropic-response-non-stream)
- [Anthropic streaming](#anthropic-streaming)
- [Tool-call wire format](#tool-call-wire-format)
- [Error envelope](#error-envelope)
- [Status labels](#status-labels)

---

## OpenAI chat request

`POST /v1/chat/completions`. JSON body:

```jsonc
{
  "model":        "mistral-7b-int4",
  "messages": [
    { "role": "system",    "content": "You are concise." },
    { "role": "user",      "content": "Capital of Japan?" },
    { "role": "assistant", "content": "Tokyo." },
    { "role": "user",      "content": "And France?" }
  ],
  "stream":      false,
  "max_tokens":  40,
  "temperature": 0.0,
  "top_k":       50,
  "tools":       [ /* see Tool-call wire format */ ],
  "tool_choice": "auto"            // string ("none" | "auto") or object
}
```

After a tool round-trip the `messages` array will include the assistant
message with `tool_calls` and matching `role: "tool"` messages:

```jsonc
{
  "messages": [
    { "role": "user", "content": "What is 7*6?" },
    { "role": "assistant", "content": null, "tool_calls": [{
        "id": "call_aB3xY7qZm",
        "type": "function",
        "function": { "name": "calculator", "arguments": "{\"expression\":\"7*6\"}" }
    }]},
    { "role": "tool", "tool_call_id": "call_aB3xY7qZm", "content": "42" }
  ]
}
```

Schema decoding lives in [Sources/App/DTOs.swift](../Sources/App/DTOs.swift)
(`ChatCompletionRequest`).

---

## OpenAI chat response (non-stream)

```jsonc
{
  "id":      "chatcmpl-0F8A...",
  "object":  "chat.completion",
  "created": 1737000000,
  "model":   "mistral-7b-int4",
  "choices": [{
    "index":         0,
    "finish_reason": "stop",
    "message":       { "role": "assistant", "content": "Paris." }
  }],
  "usage": { "prompt_tokens": 14, "completion_tokens": 1, "total_tokens": 15 }
}
```

`finish_reason` is one of `"stop"` (EOS) | `"length"` (hit `max_tokens`) |
`"tool_calls"`. The `"tool_calls"` branch replaces `content` with `null` and
adds a `tool_calls` array on the message — see
[Tool-call wire format](#tool-call-wire-format).

---

## OpenAI chat streaming

`Content-Type: text/event-stream`. Each frame ends with `\n\n`. The terminal
frame is `data: [DONE]\n\n`.

A typical text-only run looks like:

```
data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"One"}}]}

data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":", two"}}]}

...

data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":N,"completion_tokens":M,"total_tokens":N+M}}

data: [DONE]
```

For a tool round-trip:

```
data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_...","type":"function","function":{"name":"calculator","arguments":"{\"expression\":\"7*6\"}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]
```

Streaming frames are constructed in `Sources/App/DTOs.swift` (`SSEChunk`).

---

## OpenAI legacy completions

### Request

```jsonc
{
  "model":       "qwen3-1.7b-w8",
  "prompt":      "The capital of France is",
  "stream":      false,
  "max_tokens":  10,
  "temperature": 0.0,
  "top_k":       50
}
```

### Response

```jsonc
{
  "id":      "cmpl-7B3A...",
  "object":  "text_completion",
  "created": 1737000000,
  "model":   "qwen3-1.7b-w8",
  "choices": [{ "index": 0, "text": " Paris.", "finish_reason": "stop" }],
  "usage": { "prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7 }
}
```

### Legacy completions streaming

```
data: {"object":"text_completion","choices":[{"index":0,"text":" Paris","finish_reason":null}]}

data: {"object":"text_completion","choices":[{"index":0,"text":".","finish_reason":null}]}

data: {"object":"text_completion","choices":[{"index":0,"text":"","finish_reason":"stop"}]}

data: [DONE]
```

---

## OpenAI embeddings

### Request

```jsonc
{ "model": "all-minilm-l6-v2", "input": "hello world" }
// or
{ "model": "all-minilm-l6-v2", "input": ["hello", "world"] }
```

### Response

```jsonc
{
  "object": "list",
  "data": [
    { "object": "embedding", "index": 0, "embedding": [0.014, -0.027, ..., 0.003] }
  ],
  "model": "all-minilm-l6-v2",
  "usage": { "prompt_tokens": 3, "total_tokens": 3 }
}
```

Output is **L2-normalized** so dot product ≈ cosine similarity.

---

## OpenAI images

### Request

```jsonc
{
  "model":           "sd-1.5",
  "prompt":          "a red apple on wood",
  "negative_prompt": "blurry",
  "n":               1,
  "size":            "512x512",
  "steps":           15,
  "guidance_scale":  7.5,
  "seed":            42,
  "response_format": "b64_json"
}
```

`size` must equal `"<imageSize>x<imageSize>"` for the chosen model; otherwise
the server returns 400.

### Response

```jsonc
{
  "created": 1737000000,
  "data": [{ "b64_json": "iVBORw0KGgo..." }]
}
```

`response_format: "url"` is **not** implemented — the server always returns
`b64_json`.

---

## Anthropic messages request

`POST /v1/messages`. Headers: `Content-Type: application/json` +
`anthropic-version: 2023-06-01` (ignored) + `x-api-key: <anything>`
(ignored).

```jsonc
{
  "model":        "mistral-7b-int4",
  "system":       "You are concise.",
  "messages": [
    { "role": "user",      "content": "Capital of Japan?" },
    { "role": "assistant", "content": [
        { "type": "text", "text": "Tokyo." }
    ]},
    { "role": "user", "content": [
        { "type": "text", "text": "And France?" }
    ]}
  ],
  "max_tokens":     30,
  "stream":         false,
  "temperature":    0.0,
  "top_k":          50,
  "stop_sequences": ["\n\n"],
  "tools": [
    {
      "name": "calculator",
      "description": "Evaluate an arithmetic expression.",
      "input_schema": {
        "type": "object",
        "properties": { "expression": { "type": "string" } },
        "required": ["expression"]
      }
    }
  ],
  "tool_choice": { "type": "auto" }
}
```

`content` can be a string or an array of content blocks. Three block types
are recognized:

| `type` | Fields | Notes |
|---|---|---|
| `text` | `text` | The normal string body. |
| `tool_use` | `id`, `name`, `input` | Carried as part of an assistant message after a tool round-trip; on re-send the server converts these to internal `ChatMessage` tool-call records. |
| `tool_result` | `tool_use_id`, `content` | The result of a tool execution. Converted to a `role: "tool"` message. |
| anything else (image, audio, etc.) | — | Skipped (vision/audio models not yet exposed). |

Schema is in
[Sources/App/AnthropicRoutes.swift](../Sources/App/AnthropicRoutes.swift)
(`AnthropicRequest`, `AnthropicMessage`, `AnthropicTool`).

---

## Anthropic response (non-stream)

```jsonc
{
  "id":            "msg_aB3xY7qZm...",
  "type":          "message",
  "role":          "assistant",
  "model":         "mistral-7b-int4",
  "content": [
    { "type": "text", "text": "Paris." }
  ],
  "stop_reason":   "end_turn",
  "stop_sequence": null,
  "usage":         { "input_tokens": 14, "output_tokens": 1 }
}
```

`stop_reason` is one of `"end_turn"` | `"max_tokens"` | `"tool_use"`.
A response with tool calls:

```jsonc
{
  "id": "msg_...", "type": "message", "role": "assistant", "model": "...",
  "content": [
    { "type": "tool_use",
      "id":    "toolu_aB3xY7qZm",
      "name":  "calculator",
      "input": { "expression": "7*6" } }
  ],
  "stop_reason":   "tool_use",
  "stop_sequence": null,
  "usage":         { "input_tokens": N, "output_tokens": M }
}
```

If the model emitted prose before the tool marker, that text comes through
as a `{type: "text"}` block before the `{type: "tool_use"}` block. See
`textBeforeToolMarker` in `Sources/App/AnthropicRoutes.swift`.

---

## Anthropic streaming

Frame sequence for a plain text response:

```
event: message_start
data: {"type":"message_start","message":{
  "id":"msg_<24>","type":"message","role":"assistant","model":"mistral-7b-int4",
  "content":[],"stop_reason":null,"stop_sequence":null,
  "usage":{"input_tokens":N,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"One"}}

...

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":M}}

event: message_stop
data: {"type":"message_stop"}
```

For a tool-call response, additional `content_block_start` events with a
`{"type":"tool_use","id":"...","name":"...","input":{}}` block, each
followed by one or more `input_json_delta` chunks and a `content_block_stop`:

```
event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_...","name":"calculator","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"express"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ion\":\"7*6\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":M}}

event: message_stop
data: {"type":"message_stop"}
```

Frame construction in
[Sources/App/AnthropicRoutes.swift](../Sources/App/AnthropicRoutes.swift)
(`AnthropicSSE.*`).

---

## Tool-call wire format

The server is **a pass-through** for tool calling: the model emits a tool
call, the server parses and surfaces it as standard OpenAI / Anthropic
shapes, the client executes and re-sends the result. The server never
executes tools itself.

### What models emit

- **Mistral (`runtime: "standard"`)** emits:

  ```
  [TOOL_CALLS] [{"name": "calculator", "arguments": {"expression": "7*6"}}]
  ```

  Parser: `Sources/Tooling/ToolCallParser.swift`. Tolerant of the
  `[TOOL_CALLS]` marker being missing when the output is exactly a JSON
  array.

- **Gemma 4 (`runtime: "coreml-llm"`)** emits:

  ```
  <tool_call>call:calculator{"expression":"7*6"}<tool_call|>
  ```

  Parser: `Sources/ExternalRuntime/Gemma4ToolsSupport.swift`.

### What the server returns

**OpenAI surface (`/v1/chat/completions`)**

```jsonc
{
  "choices": [{
    "index":         0,
    "finish_reason": "tool_calls",
    "message": {
      "role":       "assistant",
      "content":    null,
      "tool_calls": [{
        "id":       "call_aB3xY7qZm",        // 9-char alnum (Mistral), or "toolu_<9>" (gemma)
        "type":     "function",
        "function": {
          "name":      "calculator",
          "arguments": "{\"expression\":\"7*6\"}"   // JSON string (OpenAI convention)
        }
      }]
    }
  }]
}
```

**Anthropic surface (`/v1/messages`)**

```jsonc
{
  "content": [
    { "type": "text", "text": "<any prose before the tool marker>" },
    { "type": "tool_use",
      "id":    "toolu_aB3xY7qZm",
      "name":  "calculator",
      "input": { "expression": "7*6" } }
  ],
  "stop_reason": "tool_use"
}
```

### Round-trip recipe

```text
client -> server    POST /v1/chat/completions  (messages + tools)
                  ←  finish_reason: "tool_calls"
client   execute tool on your MCP / DB / shell
client -> server    POST /v1/chat/completions
                    messages = [
                      ...,
                      {role:"assistant", content:null, tool_calls:[...]},
                      {role:"tool", tool_call_id:"call_aB3xY7qZm",
                       content:"42"}
                    ]
                  ←  normal text finish_reason: "stop"
```

The same pattern applies on the Anthropic surface using `tool_use` +
`tool_result` blocks.

### Disabling tools per request

- OpenAI: pass `"tool_choice": "none"`.
- Anthropic: pass `"tool_choice": {"type": "none"}`.

Both keep `messages` flowing but suppress tool-calling prompt rendering.

---

## Error envelope

```jsonc
{ "error": { "message": "Unknown model: gemma-99", "type": "invalid_request_error", "code": null } }
```

`type` values:

| `type` | When it appears |
|---|---|
| `invalid_request_error` | Bad request — unknown model, wrong kind, incompatible layout, packageNotFound, autoDownloadDisabled. |
| `feature_disabled` | Route requires a feature the server has off (`features.images = false`, etc.). |
| `server_error` | Generation threw unexpectedly. See server log. |

For streaming responses, errors arrive as a single SSE frame instead:

```
data: {"error":"<message>"}

data: [DONE]
```

Full error-code table:
[operations.md — HTTP error code reference](operations.md#http-error-code-reference).

---

## Status labels

Strings echoed by `GET /v1/models` and `GET /models/{id}/status`:

| Label | Meaning |
|---|---|
| `not_downloaded` | Entry exists in registry but no local files yet. |
| `downloading(N%)` | A `HubApi` download is in progress; `N` is reported as `fractionCompleted * 100`. Note: HubApi snapshots per-file so the percentage stays at 0 until each file completes — cosmetic only. |
| `downloaded` | Files present locally (raw `.mlpackage` / `Resources/`). |
| `compiling` | First-run `MLModel.compileModel` is in flight. Slow (minutes for a 7B). |
| `loading` | Compiled cache is being attached to a runner and warmed up. |
| `ready` | The runner is resident and accepts requests. |
| `failed: <message>` | The most recent download/compile/load attempt errored. |

State diagram and side effects:
[operations.md — Status state machine](operations.md#status-state-machine).
