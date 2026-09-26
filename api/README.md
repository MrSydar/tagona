# API Service

Go module: `mrsydar/tagona/api`

The public API gateway for Tagona, listening on `:8080`. A thin layer by design: it owns no business logic (no database, no object storage, no tagger access, no validation). It reverse-proxies every `/v1/*` request verbatim to the internal storage service and serves health/readiness/metrics locally.

This is the future home for cross-cutting concerns such as auth and RBAC.

---

## How it works

- `GET /healthz` — liveness (always `200` if the api service is up)
- `GET /readyz` — readiness: issues `GET {API_STORAGE_BASE_URL}/readyz` with a 2-second timeout; `200 ok` on success, `503` with a `not_ready` error otherwise
- `GET /metrics` — Prometheus metrics (`api_requests_total`, `api_errors_total`)
- `/v1/*` — reverse-proxied verbatim (path, query, headers, streaming body) to the storage service using `httputil.NewSingleHostReverseProxy` with no client-side timeout, so long-running uploads and queries are not cut off
- any other path — `404` with the standard error shape `{"error":{"code":"not_found","message":"not found"}}`

---

## Configuration

| Env Var | Required | Default | Description |
|---------|----------|---------|-------------|
| `API_HTTP_ADDR` | No | `:8080` | HTTP listen address |
| `API_STORAGE_BASE_URL` | Yes | — | Base URL of the internal storage service (e.g. `http://storage:8082`) |

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
    "best_effort": false
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

**Get Tags**

```bash
curl "http://localhost:8080/v1/collections/jobs/objects/{id}/tags?tags=golang,qa"
```

If any requested tag is missing, the storage service invokes the tagging engine before responding.

---

## Internal Structure

```
api/
├── cmd/api/          # main entry point
├── internal/
│   └── metrics/      # Prometheus metrics (api_requests_total, api_errors_total)
└── Dockerfile
```

The reference CLI client for the Tagona API lives in the storage module (`storage/cmd/client`, backed by `storage/pkg/client`) — since the storage service serves the same routes, the same client works against the api gateway and the internal storage service.

---

> **Note:** the api service is a pure proxy — data is always read from/written to the internal storage service on `:8082`. All state and business logic live in [`storage/`](../storage/).
