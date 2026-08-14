# sidecar-llm docs

Documentation for the [sidecar-llm](../README.md) Swift + Vapor inference
server — part of the gotranscribesrv stack. Provides OpenAI- and
Anthropic-compatible HTTP over local Core ML chat, image, and embedding models.

## Contents

| File | When to read it |
|---|---|
| [setup.md](setup.md) | First install, environment variables, first-run smoke tests, on-disk layout. |
| [configuration.md](configuration.md) | Full `models.json` schema — settings, every entry field, env-var overrides, per-runtime / per-kind notes. |
| [endpoints.md](endpoints.md) | Every HTTP route: request/response shapes, curl examples, streaming variants, errors. |
| [formats.md](formats.md) | OpenAI vs Anthropic JSON shapes, SSE event sequences, the tool-call wire format, error envelopes. |
| [architecture.md](architecture.md) | Module map, request data flow, compile cache, runtime dispatch. |
| [operations.md](operations.md) | Status state machine, HTTP error codes, LRU eviction, performance/RAM, troubleshooting. |
| [conversion.md](conversion.md) | Convert a Hugging Face causal LM to a stateful Core ML `.mlpackage`. |
| [models.md](models.md) | Verified working models, per-task recommendations, runtimes, gotchas. |

## Reading order

- **First time?** setup → configuration → endpoints.
- **Operator?** operations (state machine + error codes + perf + troubleshooting).
- **Adopting a model?** models → conversion.
- **Hitting the API?** endpoints → formats (and the OpenAI/Anthropic SDK snippets).
