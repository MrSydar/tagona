# API Service

Go module: `mrsydar/tagona/api`

The public API gateway for Tagona, listening on `:8080` inside the compose network. In Docker Compose it is published on the host at `:8080` (plain HTTP); put a TLS-terminating reverse proxy in front of it for anything beyond local use. A thin layer by design: it owns no business logic (no database, no object storage, no tagger access). It does **not** forward requests: it checks each one against what its endpoint accepts, refuses everything else, and makes a new, clean request to the internal storage or keystorage service from the validated values. It also serves health/readiness/metrics and the API documentation locally.

This is the future home for cross-cutting concerns such as RBAC. Authentication is enforced here: every `/v1/*` request requires a Bearer API key, which is validated by the keystorage service. API key management requires admin HTTP Basic auth, which keystorage checks itself.

---

## How it works

- `GET /healthz` — liveness (always `200` if the api service is up, unauthenticated)
- `GET /readyz` — readiness: issues `GET /readyz` to both the storage and the keystorage service with a 2-second timeout; `200 ok` when both answer `200`, `503` with a `not_ready` error otherwise (unauthenticated)
- `GET /metrics` — Prometheus metrics (`api_requests_total`, `api_errors_total`, unauthenticated)
- `/v1/collections...` — the storage routes listed under [Public API](#public-api), each served by its own handler. Every request must carry `Authorization: Bearer <api key>`; the key is validated against the keystorage service first. The handler then checks the request: only the query parameters the endpoint defines are accepted, each at most once (`400 invalid_parameter`); JSON bodies are decoded strictly, with no unknown fields and nothing after the object (`400 invalid_json`); routes that take no body refuse one (`400 unexpected_body`); collection names, UUIDs, cursors, timestamps, numbers and booleans are checked for form and normalized. A new request is then built for the storage service (which has no `/v1` prefix): no client header is copied, uploads are streamed under a fixed `Content-Type` and the size cap, and only `Content-Type` and `Content-Length` are returned from the answer. Limits that are configured in storage (maximum page size, maximum tags per query) are enforced there
- a short validation cache sits between the key check and the handlers (see Configuration); the storage and keystorage calls, the key client and the readiness probe share one pooled HTTP transport
- request hardening: paths with dot segments, empty segments, backslashes, NUL bytes or encoded slashes are rejected with `400 invalid_path`; bodies above `API_MAX_BODY_BYTES` (uploads), 256 KiB (JSON) or 4 KiB (key management) get `413 payload_too_large`; no inbound header, cookie or forwarding header is ever passed to an internal service
- `GET /v1/docs` — interactive API documentation (Swagger UI), `GET /v1/openapi.json` / `GET /v1/openapi.yaml` — the OpenAPI 3 description of the `/v1` contract (all unauthenticated; see [API documentation](#api-documentation))
- `/v1/admin/api-keys` — key management (create/list/delete), three handlers that validate the request and make a new one to the keystorage service (`/api-keys`, `/api-keys/{id}`). Only the `Authorization` header is passed on, and the admin credentials are checked by keystorage, not here; bodies over 4 KiB get `413`, and a key id that is not a UUID gets `404`. Keystorage's key validation endpoint is never reachable through the gateway
- any other path — `404` with the standard error shape `{"error":{"code":"not_found","message":"not found"}}`; a listed path with the wrong method — `405 method_not_allowed`

---

## API documentation

| Path | Description |
|------|-------------|
| `GET /v1/docs` | Interactive documentation (Swagger UI). Use **Authorize** with an API key to try requests. |
| `GET /v1/openapi.json` | The OpenAPI 3 document as JSON, for client generators and tooling |
| `GET /v1/openapi.yaml` | The same document as YAML (the source, `openapi/v1.yaml`, embedded in the binary) |

They are public and unversioned-by-content: the document lives **under the version prefix** because it is the contract of that version. `info.version` is the contract's semantic version (additive changes bump the minor); a future `/v2` will get its own `/v2/openapi.json` and `/v2/docs` while `/v1` keeps documenting itself.

The UI loads Swagger UI from jsDelivr at a pinned version with Subresource Integrity hashes, so the browser needs internet access to render it (the JSON/YAML documents do not). To upgrade the UI, see the comment in `cmd/api/docs.go`.

**Keeping the spec honest:** `openapi/v1.yaml` is written by hand. A test (`TestOpenAPIMatchesRoutes`) fails when a route is added to or removed from the gateway without updating the spec, another checks that every `$ref` resolves, and CI lints the document with Redocly. Validate locally with `npx @redocly/cli lint api/openapi/v1.yaml`.

---

## Authentication

All `/v1/*` endpoints require an API key:

```
Authorization: Bearer <api key>
```

- Missing or malformed header → `401` `{"error":{"code":"missing_api_key","message":"authorization header with bearer api key is required"}}`
- Unknown key → `401` `{"error":{"code":"invalid_api_key","message":"invalid or unknown api key"}}`
- Keystorage unreachable during validation → `503` `{"error":{"code":"not_ready","message":"key service not available"}}`

There is no RBAC: every valid API key grants complete access to all `/v1/*` endpoints. Keys are minted via the admin endpoints below and stored (hashed) by the keystorage service in its own database schema — the api service owns no database. The Bearer header is stripped before the request is forwarded to the internal storage service.

`/healthz`, `/readyz`, and `/metrics` remain unauthenticated so Docker healthchecks and Prometheus scraping work.

---

## Admin: API key management

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/admin/api-keys` | Create an API key |
| `GET` | `/v1/admin/api-keys` | List API keys |
| `DELETE` | `/v1/admin/api-keys/{id}` | Delete an API key |

These endpoints require admin HTTP Basic auth, checked by the keystorage service against `KEYSTORAGE_ADMIN_USERNAME`/`KEYSTORAGE_ADMIN_PASSWORD` (the api has no admin settings of its own):

- Valid credentials → request proceeds
- Wrong or missing credentials → `401` `{"error":{"code":"invalid_admin_credentials","message":"admin authentication required"}}` with `WWW-Authenticate: Basic realm="tagona-admin"`
- Presenting `Authorization: Bearer ...` (an API key) → `403` `{"error":{"code":"forbidden","message":"api keys cannot be used for key management"}}` — API keys are rejected here even when valid
- Either keystorage admin variable unset → `403` `{"error":{"code":"admin_disabled","message":"admin credentials are not configured"}}`

**Create an API key**

```bash
curl -u admin:tagona -X POST http://localhost:8080/v1/admin/api-keys \
  -H "Content-Type: application/json" \
  -d '{"name":"dev"}'
```

Response `201 Created` — the raw key is shown only once, here:

```json
{
  "id": "8f1a...",
  "name": "dev",
  "key": "tagona_3f9c...64 hex chars...",
  "created_at": "2026-09-28T10:00:00Z"
}
```

**List API keys**

```bash
curl -u admin:tagona http://localhost:8080/v1/admin/api-keys
```

Response `200 OK`:

```json
{
  "keys": [
    {"id":"8f1a...","name":"dev","key_prefix":"tagona_3f9c1","created_at":"2026-09-28T10:00:00Z"}
  ]
}
```

**Delete an API key**

```bash
curl -u admin:tagona -X DELETE http://localhost:8080/v1/admin/api-keys/{id}
```

Response `204 No Content`, or `404` `{"error":{"code":"not_found","message":"api key not found"}}` for an unknown id.

**Using the key**

```bash
KEY="tagona_..."   # from the create response

curl -s -X POST http://localhost:8080/v1/collections \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"jobs","data_type":"txt"}'
```

---

## Configuration

| Env Var | Required | Default | Description |
|---------|----------|---------|-------------|
| `API_HTTP_ADDR` | No | `:8080` | HTTP listen address |
| `API_STORAGE_BASE_URL` | Yes | — | Base URL of the internal storage service (e.g. `http://storage:8082`) |
| `API_KEYSTORAGE_BASE_URL` | Yes | — | Base URL of the internal keystorage service (e.g. `http://keystorage:8083`) |
| `API_KEY_CACHE_TTL` | No | `30s` | How long key validation verdicts are cached (Go duration); `0` disables. Unknown keys are cached for at most 5s. Deleting a key through the gateway purges the cache; deletes made directly in keystorage take effect after the TTL |
| `API_MAX_BODY_BYTES` | No | `33554432` (32 MiB) | Maximum request body size; `0` disables. A backstop above storage's own per-object limit |

---

## Build & Run

Standalone (requires the storage service running):

```bash
cd api
go mod download
go run ./cmd/api
```

Build binary:

```bash
cd api
go build -o api ./cmd/api
./api
```

Run tests:

```bash
cd api
go test ./...
```

---

## Public API

### Collections

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/v1/collections` | List all collections |
| `POST` | `/v1/collections` | Create a collection |
| `DELETE` | `/v1/collections/{collection}` | Delete a collection |
| `GET` | `/v1/collections/{collection}/tags` | Object count and registered tags with per-tag counts |

**Create Collection**

Request:
```json
{"name":"jobs","data_type":"txt"}
```

Response `201 Created`:
```json
{"name":"jobs","data_type":"txt"}
```

**List Collections**

Response `200 OK`:
```json
{
  "collections": [
    {"id":"...","name":"jobs","data_type":"txt","created_at":"2026-06-13T12:00:00Z"}
  ]
}
```

**Delete Collection**

Response `204 No Content` — cascades to all objects and tags, and deletes S3 payloads.

**Collection Tags**

Tags are sparse: an object only has a value for a tag once the tag was evaluated for it (lazily, by a query or a tags request). This endpoint shows the collection's total object count and every tag registered in it, with how many objects the tag is true, false, or not yet evaluated for.

```bash
curl -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/v1/collections/jobs/tags?prefix=lang&limit=100"
```

Response `200 OK`:
```json
{
  "collection": "jobs",
  "total_objects": 120,
  "tags": [
    {"tag":"golang","true_count":40,"false_count":55,"first_seen_at":"2026-10-02T07:29:19Z"},
    {"tag":"lang:rust","true_count":3,"false_count":20,"first_seen_at":"2026-10-02T08:01:42Z"}
  ],
  "next": "bGFuZzpydXN0"
}
```

- `total_objects` — objects currently in the collection (expired objects are excluded even before the retention sweep removes them). It counts the whole collection, not just the tags returned.
- `true_count` / `false_count` — objects the tag is known true / false for; the remaining `total_objects - true_count - false_count` objects have not been evaluated for the tag yet.
- A tag stays registered once seen, even if every object carrying it is later deleted (its counts drop to `0`).
- Tags are ordered by name (byte order). Query parameters: `prefix` (literal prefix filter, max 128 bytes), `limit` (default `100`, max `1000`, else `400 invalid_limit`), `cursor` (the `next` value of the previous page; absent on the last page).
- Errors: `404 not_found` for an unknown collection, `400 invalid_collection_name`, `invalid_limit`, `invalid_prefix`, `invalid_cursor`.
- Counts are maintained by database triggers in the same transaction as every write, so reads are cheap and always consistent with the data.

### Objects

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/collections/{collection}/objects` | Upload an object |
| `GET` | `/v1/collections/{collection}/objects/{id}` | Get metadata |
| `GET` | `/v1/collections/{collection}/objects/{id}/data` | Download payload |
| `GET` | `/v1/collections/{collection}/objects/{id}/tags` | Get tags |
| `POST` | `/v1/collections/{collection}/objects/query` | Query by tags |
| `DELETE` | `/v1/collections/{collection}/objects/{id}` | Hard delete |

**Upload**

```bash
curl -X POST "http://localhost:8080/v1/collections/jobs/objects?data_type=txt&date=2026-06-07T12:00:00Z&ttl_seconds=3600" \
  -H "Content-Type: application/octet-stream" \
  -d 'hello world'
```

**Response `201 Created`**
```json
{
  "id": "...",
  "collection": "jobs",
  "data_type": "txt",
  "date": "2026-06-07T12:00:00Z",
  "size_bytes": 11,
  "content_hash": "..."
}
```

**Query by Tags**

```bash
curl -X POST http://localhost:8080/v1/collections/jobs/objects/query \
  -H "Content-Type: application/json" \
  -d '{
    "tags": {"golang": true, "qa": false},
    "date": {"gte": "2026-01-01T00:00:00Z", "lt": "2027-01-01T00:00:00Z"},
    "limit": 5,
    "cursor": "...",
    "timeout_ms": 30000,
    "best_effort": false,
    "evaluate": true
  }'
```

**Response**
```json
{
  "objects": [...],
  "next": "..."
}
```

- Tags are ANDed: all provided tags must match exactly.
- Missing tags are evaluated on-demand via the tagging engine.
- Ordering: `date DESC`, then `id ASC`.
- Cursor: `base64url(<unix_millis>|<uuid>)` of the last returned object.
- `timeout_ms`: query timeout in milliseconds. Defaults to `30000` (30s). Must be between `1000` (1s) and `300000` (5m); otherwise a `400 invalid_timeout` error is returned. If reached and `best_effort` is `false`, a `query_timeout` error is returned.
- `best_effort`: when `true` and the query times out, the server returns whatever objects were found up to that point instead of failing. A pagination `next` cursor is included.
- `evaluate` (default `true`): when `false`, the query is answered from tags that are **already known** and the tagging engine is never called. An object is returned only if every requested tag is known for it and matches, so objects whose requested tags have not been evaluated yet are left out (the result is a subset of what `evaluate: true` would return, and it can grow as tags get evaluated). It is fast and cheap, independent of the tagger's availability and speed, and does not register any new tags in the collection. Pagination (`cursor`/`next`) and the `date` filter work as usual; `best_effort` has no effect because there is no partial result to return.

**Get Tags**

```bash
curl "http://localhost:8080/v1/collections/jobs/objects/{id}/tags?tags=golang,qa"
```

If any requested tag is missing, the storage service invokes the tagging engine before responding. Without `tags`, only the tags already known for the object are returned.

Query parameter `evaluate` (default `true`): with `evaluate=false` the tagging engine is not called. Requested tags that are known come back as `true`/`false`; requested tags that have not been evaluated yet come back as `null`:

```bash
curl "http://localhost:8080/v1/collections/jobs/objects/{id}/tags?tags=golang,java&evaluate=false"
```
```json
{"id": "…", "tags": {"golang": true, "java": null}}
```

Any value other than a boolean (`true`/`false`/`1`/`0`) is rejected with `400 invalid_evaluate`.

---

## Internal Structure

```
api/
├── cmd/api/            # main entry point
├── openapi/            # v1.yaml, the OpenAPI description of the /v1 API (embedded and served)
├── internal/
│   ├── metrics/        # Prometheus metrics (api_requests_total, api_errors_total)
│   └── keystorageapi/  # HTTP client for keystorage's key validation endpoint
└── Dockerfile
```

The reference CLI client for the Tagona API lives in the storage module (`storage/cmd/client`, backed by `storage/pkg/client`) — since the storage service serves the same routes, the same client works against the api gateway and the internal storage service.

---

> **Note:** the api service has no state of its own — data is always read from/written to the internal storage service on `:8082` through requests the gateway builds itself. All state and business logic live in [`storage/`](../storage/). It enforces API key authentication but owns none of the data or the keys: keys live in [`keystorage/`](../keystorage/).
