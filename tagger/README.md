# Tagging Engine

Go module: `mrsydar/tagona/tagger`

A standalone HTTP service that evaluates boolean tags for objects stored in the Tagona storage service. It is a pure evaluator: it fetches an object's payload from the storage service via its public HTTP API, then runs the tagging logic. Objects are bytes: an evaluator decides what it makes of them (the bundled ones read them as UTF-8 text).

---

## Responsibilities

- Report its version
- Receive tag evaluation requests for specific objects
- Refuse requests for a version it does not run
- Fetch the object payload from the storage service (dogfooding public APIs)
- Evaluate tags against the payload
- Return tag results synchronously

---

## API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/version` | Return the tagger version |
| `POST` | `/tag` | Evaluate tags for an object |
| `GET` | `/healthz` | Liveness |
| `GET` | `/readyz` | Readiness |

### Version

**Request**
```bash
curl http://localhost:8081/version
```

**Response `200 OK`**
```json
{"version": "decisions/openai:gpt-6-luna"}
```

Different taggers, and different models behind one tagger, may tag the same object differently, so a collection records the version of the tagger that tags it. A version is any string; the convention is `<implementation>` or `<implementation>:<model>`: `grep`, `false`, `completions/openai:gpt-4o-mini`, `decisions/openai:gpt-6-luna`. It defaults to the evaluator's own (the implementation name and, for the LLM-backed ones, its `MODEL`), and `TAGGER_VERSION` replaces it, for example to keep a collection's version through a change of model that is known to tag alike.

### Tag

**Request**
```bash
curl -X POST http://localhost:8081/tag \
  -H "Content-Type: application/json" \
  -d '{
    "collection": "jobs",
    "object_id": "a1b2c3d4...",
    "tagger_version": "grep",
    "tags": ["golang", "qa"]
  }'
```

**Response `200 OK`**
```json
{"tags": {"golang": true, "qa": false}}
```

**Behavior**

1. Compares `tagger_version` (the version the object's collection is tagged with) with its own: another version is refused with `409` `tagger_version_mismatch` (`details`: `expected`, `running`), because its answer must not be stored as that collection's
2. Fetches object payload: `GET /v1/collections/{collection}/objects/{id}/data`
3. Evaluates tags against the payload
4. Returns only the requested tags

---

## Tag Evaluation Logic

The evaluator is selected via `TAGGER_EVALUATOR_IMPL`:

| Evaluator | Default version | Logic |
|-----------|-----------------|-------|
| `grep`    | `grep`          | Tag is `true` if the payload (read as UTF-8 text) contains the tag string as a substring. Case-sensitive. |
| `false`   | `false`         | All tags evaluate to `false`. |
| `completions/openai` | `completions/openai:<model>` | An LLM classifies the payload against the requested tags through a chat completions API in the OpenAI dialect; missing tags default to `false`. |
| `decisions/openai` | `decisions/openai:<model>` | One yes/no question per tag through OpenAI's [Decisions API](https://developers.openai.com/api/docs/guides/decisions) (`POST /decisions`). A tag is `true` when the returned probability is at least the threshold. |
| `decisions/vercel` | `decisions/vercel:<model>` | The same idea through the Vercel AI Gateway's evaluate endpoint (`POST /evaluate`), which has a different wire format. |

The part after the slash names the **API dialect**, not the vendor: any vendor that speaks it is used with its own base URL (and path, key header and extra parameters, below). The `completions/openai` evaluator replaces the former `openai`, and `decisions/vercel` the former `systemone` with its `vercel` backend.

When a collection is created without a `tagger_version`, the storage service asks the tagger for its `/version` and records it.

---

## LLM-backed evaluators

`completions/openai`, `decisions/openai` and `decisions/vercel` share one configuration scheme. Each reads the settings named after it: the prefix is `TAGGER_<KIND>_<DIALECT>_`, for example `TAGGER_DECISIONS_OPENAI_`. Bad values stop the tagger at startup with a message that names the setting.

| Setting (after the prefix) | Default | Description |
|----------------------------|---------|-------------|
| `API_KEY` | — | Key sent to the vendor. Empty sends no credentials (local servers) |
| `BASE_URL` | per dialect | Scheme, host and any path prefix: `https://api.openai.com/v1`, `https://ai-gateway.vercel.sh/v1` |
| `PATH` | per dialect | The endpoint below the base URL: `/chat/completions`, `/decisions`, `/evaluate` |
| `MODEL` | per dialect | `gpt-4o-mini`, `gpt-6-luna` (the only model the Decisions API accepts today), `typesafe-ai/jev` |
| `TIMEOUT` | `60s` | Per-request timeout |
| `AUTH_HEADER` | `Authorization` | Header that carries the key; vendors with e.g. an `api-key` header set it here |
| `AUTH_SCHEME` | `Bearer` | Prefix of the header value. **Set it empty to send the bare key** |
| `HEADERS` | — | Extra request headers, a JSON object: `{"OpenAI-Organization":"org-1"}` |
| `QUERY` | — | Extra query parameters, as a query string: `api-version=2024-10-21` |
| `PARAMS` | — | Extra top-level fields of the request body, a JSON object. They override the evaluator's own fields of the same name, and `null` removes one: `{"max_completion_tokens":50,"temperature":null}` (the second drops the `temperature` that `completions/openai` sends, for models that reject it) |

Specific to `completions/openai`: `SYSTEM_PROMPT` (default: "You are a tag evaluation engine that responds only with JSON.").

Specific to the decisions evaluators:

| Setting | Default | Description |
|---------|---------|-------------|
| `THRESHOLD` | `0.5` | A tag is `true` when the probability is at least this value (0 to 1). A tag the vendor does not answer, or refuses, is `false` |
| `BATCH_SIZE` | `50` | Questions per request; more tags are sent in several requests, and one failing request fails the evaluation. `0` sends everything at once |
| `INSTRUCTIONS` | *Analyze the text and determine whether the tag "{tag}" applies...* | The question put for each tag; `{tag}` is replaced by the tag |

**How the dialects differ.** `decisions/openai` sends `{"model","input","questions":[{"type":"predicate","name":"q0","instructions":"..."}]}` and reads `answers[]` (`predicate` answers carry `probability`; `refusal` answers count as false). Question names are positional (`q0`, `q1`, ...) so any tag text is safe. `decisions/vercel` sends `{"model","state","questions":{"<tag>":{"type":"boolean","instructions":"..."}}}` and reads `answers.<tag>.probability`. `completions/openai` reads the JSON object the model writes in `choices[0].message.content` and tolerates code fences and reasoning preambles.

**A compatible vendor under another URL.** Point the evaluator at it and adjust only what differs:

```bash
TAGGER_EVALUATOR_IMPL=completions/openai
TAGGER_COMPLETIONS_OPENAI_BASE_URL=https://llm.internal.example/openai/deployments/d1
TAGGER_COMPLETIONS_OPENAI_MODEL=d1
TAGGER_COMPLETIONS_OPENAI_AUTH_HEADER=api-key
TAGGER_COMPLETIONS_OPENAI_AUTH_SCHEME=
TAGGER_COMPLETIONS_OPENAI_QUERY=api-version=2024-10-21
TAGGER_COMPLETIONS_OPENAI_PARAMS='{"temperature":null,"max_completion_tokens":64}'
```

---

## Configuration

| Env Var | Required | Default | Description |
|---------|----------|---------|-------------|
| `TAGGER_HTTP_ADDR` | No | `:8081` | HTTP listen address |
| `TAGGER_STORAGE_BASE_URL` | Yes | `http://localhost:8082` | Base URL of the storage service to fetch objects from |
| `TAGGER_EVALUATOR_IMPL` | No | `grep` | Evaluator to use: `grep`, `false`, `completions/openai`, `decisions/openai` or `decisions/vercel` |
| `TAGGER_VERSION` | No | the evaluator's own | The version the tagger reports and accepts, 1-128 bytes of text; see [Version](#version) |
| `TAGGER_<KIND>_<DIALECT>_*` | No | per evaluator | Settings of the LLM-backed evaluators, listed under [LLM-backed evaluators](#llm-backed-evaluators); `tagger/.env.example` lists them all |

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
- **No interpretation of payload semantics**: the storage service stores bytes and does not understand them. The tagging engine is the only component that interprets content, and a collection records which tagger version it is tagged with.
