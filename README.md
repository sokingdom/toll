<h1 align="center">
  <img src="assets/toll-logo.png" alt="toll" width="360">
</h1>

<p align="center"><b>A stateless egress gateway for AI services.</b></p>

**toll** sits between your backend and LLM providers. It passes every request
through unchanged and records **token usage per end user, API key and model**,
so you can see who is spending what without touching your application code.

```
[app backend] ──▶ toll ──▶ OpenAI / Anthropic / Gemini / xAI / vLLM …
                   │
                   ├─▶ events-*.jsonl   per-request usage, per end user
                   └─▶ /metrics         Prometheus, per model and key
```

- **Drop-in.** Native provider APIs. Point your SDK's `base_url` at toll and add one header.
- **Stateless.** No database, no shared state, no stored secrets. Provider keys pass straight through.
- **Per-user visibility.** Usage is attributed to the end user from `X-Toll-User` or the provider's own user field.
- **Loss is visible.** Every request gets a `request.started` and a `request.completed` event, so a crash shows up as an unmatched start instead of silently disappearing.

> toll trusts its callers and does no authentication. Run it on an internal network only.

## Get Started

### 1. Run toll

Requires Go 1.26 or later.

```sh
git clone https://github.com/sokingdom/toll.git
cd toll
go run ./cmd/toll
```

toll listens on `:8655` and works without a config file. OpenAI, Anthropic,
Gemini and xAI are built in.

Or with Docker:

```sh
docker build -f deploy/Dockerfile -t toll .
printf 'events: { output: stdout }\n' > toll.yaml
docker run --rm -p 8655:8655 \
  -v "$PWD/toll.yaml:/etc/toll/toll.yaml:ro" -e TOLL_CONFIG=/etc/toll/toll.yaml \
  toll
```

### 2. Send a request through toll

Change the base URL and tell toll who the end user is. Everything else stays the same.

```sh
curl http://localhost:8655/openai/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Toll-User: user_8f3a21" \
  -H "X-Toll-Tags: feature=chat" \
  -d '{"model": "gpt-4o-mini", "stream": true, "messages": [{"role": "user", "content": "Hello"}]}'
```

With an SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8655/openai/v1")  # key still comes from OPENAI_API_KEY
client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "Hello"}],
    extra_headers={"X-Toll-User": user.id, "X-Toll-Tags": "feature=chat"},
)
```

```python
from anthropic import Anthropic

client = Anthropic(base_url="http://localhost:8655/anthropic")
client.messages.create(
    model="claude-haiku-4-5",
    max_tokens=256,
    messages=[{"role": "user", "content": "Hello"}],
    metadata={"user_id": user.id},  # toll reads this when X-Toll-User is absent
)
```

### 3. See the usage

Events are written to `./events/` as JSON lines, one file per hour. A completed event looks like this (abridged):

```sh
tail -f events/*.jsonl
```

```json
{"type":"request.completed","request_id":"01M4NT4MF9JMB9XYEZ34Y9AEV9","user":"user_8f3a21","user_source":"header",
 "key_id":"k_3f9a1c07b2e4","key_hint":"…x9a1","provider":"openai","requested_model":"gpt-4o-mini",
 "model":"gpt-4o-mini-2024-07-18","tags":{"feature":"chat"},"stream":true,"usage_injected":true,
 "status":200,"partial":false,"usage_source":"provider","input_tokens":812,"cache_read_tokens":0,
 "cache_write_tokens":0,"output_tokens":240,"reasoning_tokens":0,"latency_ms":1830,"ttfb_ms":412}
```

Aggregates per model and key are at `http://localhost:8655/metrics`.

## How it works

### Routes

| Path | Upstream |
|---|---|
| `/openai/*` | `https://api.openai.com` |
| `/anthropic/*` | `https://api.anthropic.com` |
| `/gemini/*` | `https://generativelanguage.googleapis.com` |
| `/xai/*` | `https://api.x.ai` |
| `/<name>/*` | any OpenAI-compatible server you configure (vLLM, Ollama, …) |
| `/metrics`, `/healthz` | Prometheus metrics, health check |

### Metered endpoints

| Format | Endpoints |
|---|---|
| OpenAI (also xAI, compatible servers) | `chat/completions`, `completions`, `responses`, `embeddings` |
| Anthropic | `messages` |
| Gemini | `generateContent`, `streamGenerateContent` |

Other endpoints are proxied as-is and recorded with `usage_source: "none"`.

### Headers

| Header | Description |
|---|---|
| `X-Toll-User` | End-user ID. If absent, toll uses the provider's user field: `safety_identifier` or `user` (OpenAI), `metadata.user_id` (Anthropic). |
| `X-Toll-Tags` | Free-form `k=v,k=v` tags, e.g. feature or platform. |
| `X-Toll-Auth` | Shared secret, required only when `server.auth_token` is set. |
| `X-Toll-Request-Id` | Response header matching the events' `request_id`. |

`X-Toll-*` request headers are stripped before forwarding.

### What toll changes

toll passes requests and responses through byte for byte, with three exceptions:

1. `X-Toll-*` headers are removed.
2. Streaming OpenAI-format requests get `stream_options.include_usage: true`, because OpenAI omits usage from streams otherwise. Only that field is edited. Disable with `metering.inject_stream_usage: false`.
3. `Accept-Encoding` is limited to gzip and deflate so that toll can read usage from compressed responses.

Responses are never modified.

### API keys

toll never stores keys. It records a fingerprint (`k_` + first 12 hex chars of
SHA-256) and the last four characters. Name fingerprints with `key_aliases` to
get readable labels in metrics.

### Costs

toll records tokens, not money. Multiply the token fields by your price table
downstream. Input tokens exclude cached tokens, so the fields never overlap.

### Delivery

Events are written synchronously with no in-memory queue. Once written they
survive a toll crash, and a log shipper (Vector, Fluent Bit, …) can pick them
up at its own pace. For billing:

- Use `events.output: file` and a shipper that checkpoints its read position.
- Deduplicate on `request_id`.
- Alert on `request.started` events with no matching `request.completed`.
- Consider `events.on_write_error: fail_closed`, which rejects requests with 503 when an event cannot be written.

## Configuration

Every setting is optional. See [`toll.example.yaml`](toll.example.yaml) for the
full list with defaults. Pass a file with `-config` or `TOLL_CONFIG`. `${VAR}`
is replaced from the environment, and `SIGHUP` reloads the file.

## Roadmap

**Done**

- [x] Pass-through proxy for OpenAI, Anthropic, Gemini, xAI and OpenAI-compatible servers
- [x] Token usage for streaming and non-streaming responses
- [x] End-user attribution from header or provider user field
- [x] API key fingerprints and aliases
- [x] `request.started` / `request.completed` events to file or stdout, with rotation
- [x] Prometheus metrics
- [x] Config reload and graceful shutdown

**Next**

- [ ] Reference stack: Vector, ClickHouse and Grafana with per-user, per-model and per-key dashboards and an example price table
- [ ] Verification against live provider APIs
- [ ] Published container image and release binaries

**Later**

- [ ] Usage that is not tokens: built-in tool calls, images, audio
- [ ] Image and audio endpoints

**Not planned for v1**

- Rate limiting, model allow-lists, `max_tokens` caps
- Cost calculation inside toll
- Authentication and provider key management

## License

Apache License 2.0. See [LICENSE](LICENSE).
