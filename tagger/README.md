# Tagging Engine

Go module: `mrsydar/tagona/tagger`

A standalone HTTP service that evaluates boolean tags for objects stored in the Tagona storage service. It is a pure evaluator: it fetches object metadata and payload from the storage service via its public HTTP API, then runs the tagging logic.

---

## Responsibilities

- Expose supported data types
- Receive tag evaluation requests for specific objects
- Fetch object metadata + payload from the storage service (dogfooding public APIs)
- Evaluate tags based on `data_type` and payload content
- Return tag results synchronously

---

## API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/supported-types` | Return supported data types |
| `POST` | `/tag` | Evaluate tags for an object |
| `GET` | `/healthz` | Liveness |
| `GET` | `/readyz` | Readiness |

### Supported Types

**Request**
```bash
curl http://localhost:8081/supported-types
```

**Response `200 OK`**
```json
{"types": ["txt"]}
```

### Tag

**Request**
```bash
curl -X POST http://localhost:8081/tag \
  -H "Content-Type: application/json" \
  -d '{
    "collection": "jobs",
    "object_id": "a1b2c3d4...",
    "tags": ["golang", "qa"]
  }'
```

**Response `200 OK`**
```json
{"tags": {"golang": true, "qa": false}}
```

**Behavior**

1. Fetches object metadata from storage: `GET /v1/collections/{collection}/objects/{id}`
2. Fetches object payload: `GET /v1/collections/{collection}/objects/{id}/data`
3. Evaluates tags based on `data_type`
4. Returns only the requested tags

---

## Tag Evaluation Logic

The evaluator is selected via `TAGGER_EVALUATOR_IMPL`:

| Evaluator | Supported `data_type`s | Logic |
|-----------|----------------------|-------|
| `grep`    | `txt`                | Tag is `true` if the payload (UTF-8 text) contains the tag string as a substring. Case-sensitive. |
| `false`   | `txt`, `png`         | All tags evaluate to `false`. |
| `openai`  | `txt`                | An LLM classifies the payload against the requested tags via an OpenAI-compatible chat completions API; missing tags default to `false`. |
| `systemone` | `txt`              | Pluggable-backend evaluator, selected via `TAGGER_SYSTEMONE_BACKEND` (default `vercel`). See [Systemone](#systemone) below. |

The storage service validates that `data_type` is in the supported-types set (reported by the evaluator) before creating a collection.

---

## Systemone

`TAGGER_EVALUATOR_IMPL=systemone` selects a pluggable-backend evaluator. The backend is chosen via `TAGGER_SYSTEMONE_BACKEND`:

- Default: `vercel` (currently the only backend; more LLM gateway backends will be added later).
- The `vercel` backend sends one boolean question per tag to the Vercel AI Gateway `evaluate` endpoint and marks a tag `true` when the returned probability meets `TAGGER_VERCEL_THRESHOLD`.

The `vercel` backend keeps using the same env vars with unchanged behavior and threshold semantics: `TAGGER_VERCEL_API_KEY`, `TAGGER_VERCEL_BASE_URL`, `TAGGER_VERCEL_MODEL`, `TAGGER_VERCEL_THRESHOLD`, `TAGGER_VERCEL_TIMEOUT`.

---

## Configuration

| Env Var | Required | Default | Description |
|---------|----------|---------|-------------|
| `TAGGER_HTTP_ADDR` | No | `:8081` | HTTP listen address |
| `TAGGER_STORAGE_BASE_URL` | Yes | `http://localhost:8082` | Base URL of the storage service to fetch objects from |
| `TAGGER_EVALUATOR_IMPL` | No | `grep` | Evaluator to use: `grep`, `false`, `openai`, or `systemone` |
| `TAGGER_SYSTEMONE_BACKEND` | No | `vercel` | Systemone backend to use (only `vercel` for now) |
| `TAGGER_OPENAI_API_KEY` | No | — | OpenAI API key (required when `TAGGER_EVALUATOR_IMPL=openai`) |
| `TAGGER_OPENAI_BASE_URL` | No | `https://api.openai.com/v1` | OpenAI-compatible API base URL |
| `TAGGER_OPENAI_MODEL` | No | `gpt-4o-mini` | Model name for chat completions |
| `TAGGER_OPENAI_TIMEOUT` | No | `60s` | HTTP timeout for OpenAI API requests |
| `TAGGER_VERCEL_API_KEY` | No | — | Vercel AI Gateway token (required for the systemone `vercel` backend) |
| `TAGGER_VERCEL_BASE_URL` | No | `https://ai-gateway.vercel.sh/v1` | Vercel AI Gateway base URL |
| `TAGGER_VERCEL_MODEL` | No | `typesafe-ai/jev` | Model name sent to the evaluate endpoint |
| `TAGGER_VERCEL_THRESHOLD` | No | `0.5` | Probability threshold: tag is `true` when probability ≥ threshold |
| `TAGGER_VERCEL_TIMEOUT` | No | `60s` | HTTP timeout for Vercel AI Gateway requests |

---

## Build & Run

Standalone (requires the storage service running):

```bash
cd tagger
go mod download
go run ./cmd/tagger
```

Build binary:

```bash
cd tagger
go build -o tagger ./cmd/tagger
./tagger
```

Run tests:

```bash
cd tagger
go test ./...
```

---

## Public Client

The `pkg/client` package implements `storage/pkg/client.Tagger` — the interface the storage service uses to call the tagging engine. It handles retries with exponential backoff.

---

## Internal Structure

```
tagger/
├── cmd/tagger/            # main entry point
├── internal/
│   └── server/
│       └── server.go        # HTTP handlers + tag evaluation logic
└── pkg/client/              # public Go client (implements storage/client.Tagger)
    └── client.go
```

This service intentionally stays minimal. It has no database, no object storage client, and no caching (MVP). All state is fetched from the storage service on every request.

---

## Design Decisions

- **No caching (MVP)**: tag results are not cached. The storage service persists evaluated tags.
- **Public API only**: the tagging engine uses the same public HTTP APIs as any external client. No internal/private endpoints are used.
- **No interpretation of payload semantics**: the storage service does not understand `data_type`; it only validates it against the supported set. The tagging engine is the only component that interprets content.
