# Storage Service

Go module: `mrsydar/tagona/storage`

The main HTTP API and storage layer for Tagona. Handles collections, object upload/download, metadata, tag queries, and on-demand tag evaluation via the tagging engine.

---

## Responsibilities

- **Collections**: create, list, validate, and delete collections
- **Object Storage**: stream payloads to S3-compatible storage, compute SHA-256 content hashes
- **Metadata**: store object metadata and sparse tags in Postgres
- **Tag Queries**: evaluate missing tags on-demand to satisfy queries with limit/pagination
- **Retention**: background sweeper deleting expired objects by TTL

---

## API

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
  "content_hash": "a591a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f146e"
}
```

Upload is idempotent by content hash within a collection. If a duplicate is detected after insert, the newly uploaded S3 payload is deleted and the existing metadata is returned.

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
- `best_effort`: when `true` and the query times out, the server returns whatever objects were found up to that point instead of failing. A pagination `next` cursor is included when there may be more objects remaining to scan.

**Get Tags**

```bash
curl "http://localhost:8080/v1/collections/jobs/objects/{id}/tags?tags=golang,qa"
```

If any requested tag is missing, the storage service invokes the tagging engine before responding.

### Operational

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Liveness (always `200` if up) |
| `GET` | `/readyz` | Readiness (checks DB + S3 connectivity) |

---

## Configuration

| Env Var | Required | Default | Description |
|---------|----------|---------|-------------|
| `TAGONA_HTTP_ADDR` | No | `:8080` | HTTP listen address |
| `TAGONA_PG_DSN` | Yes | — | Postgres DSN |
| `TAGONA_S3_ENDPOINT` | Yes | — | S3 endpoint URL |
| `TAGONA_S3_REGION` | No | `us-east-1` | S3 region |
| `TAGONA_S3_BUCKET` | Yes | — | S3 bucket name |
| `TAGONA_S3_ACCESS_KEY` | Yes | — | S3 access key |
| `TAGONA_S3_SECRET_KEY` | Yes | — | S3 secret key |
| `TAGONA_S3_FORCE_PATH_STYLE` | No | `true` | Use path-style S3 URLs |
| `TAGONA_TAG_ENGINE_URL` | Yes | — | Tagging engine base URL |
| `TAGONA_TAG_ENGINE_TIMEOUT` | No | `30s` | Tagging engine HTTP request timeout |
| `TAGONA_DEFAULT_LIMIT` | No | `5` | Default query limit |
| `TAGONA_MAX_LIMIT` | No | `100` | Hard cap on query limit |
| `TAGONA_DEFAULT_TTL` | No | `0` | Default TTL in seconds or duration string (`0` = none) |
| `TAGONA_MAX_TAGS_PER_QUERY` | No | `100` | Max tags in a query |
| `TAGONA_MAX_OBJECT_SIZE_BYTES` | No | `10485760` | Max payload size (`10 MB`) |
| `TAGONA_RETENTION_SWEEP_INTERVAL` | No | `60s` | Interval for retention sweeper |

---

## Build & Run

Standalone (requires Postgres and S3-compatible storage running):

```bash
cd storage
go mod download
go run ./cmd/storage
```

Build binary:

```bash
cd storage
go build -o storage ./cmd/storage
./storage
```

Run tests:

```bash
cd storage
go test ./...
```

---

## CLI Client

A reference CLI client is available at `cmd/client`:

```bash
go run ./cmd/client --url http://localhost:8080 <command> [options]
```

### Commands

```bash
# Collections
client --url http://localhost:8080 list-collections
client --url http://localhost:8080 create-collection --name jobs --data-type txt
client --url http://localhost:8080 delete-collection --collection jobs

# Objects
client --url http://localhost:8080 upload --collection jobs --data-type txt --file hello.txt
client --url http://localhost:8080 get --collection jobs --id <id>
client --url http://localhost:8080 data --collection jobs --id <id> --out hello.txt
client --url http://localhost:8080 tags --collection jobs --id <id> --tags golang,qa
client --url http://localhost:8080 query --collection jobs --tags '{"golang":true}' --limit 5 --timeout 30000
client --url http://localhost:8080 query --collection jobs --tags '{"golang":true}' --limit 5 --best-effort
client --url http://localhost:8080 delete --collection jobs --id <id>
```

---

## Internal Structure

```
storage/
├── cmd/storage/           # main entry point
├── internal/
│   ├── config/            # env parsing
│   ├── cursor/            # pagination cursor encode/decode
│   ├── db/                # Postgres queries and transactions
│   ├── models/            # shared struct types
│   ├── query/             # tag query + scan logic
│   ├── retention/         # TTL background sweeper
│   ├── server/            # HTTP handlers (chi router)
│   ├── storage/           # S3 client (AWS SDK v2)
│   └── validate/          # collection/tag/date validation
├── pkg/client/            # public Go client for the storage HTTP API
│   ├── client.go          # storage API client
│   └── tagger.go          # Tagger client interface
└── migrations/            # SQL schema files
```

## Public Client

The `pkg/client` package provides a reusable Go HTTP client for the storage service's public API. It is also consumed by the tagging engine to fetch object metadata and payloads.

| File | Description |
|------|-------------|
| `pkg/client/client.go` | Storage service HTTP client (collections, objects, queries) |
| `pkg/client/tagger.go` | `Tagger` interface abstracting the tagging engine client |

---

## Migrations

Migrations are applied automatically on startup using a simple file-based runner.

| File | Description |
|------|-------------|
| `000001_initial_schema.up.sql` | Creates `collections`, `objects`, `object_tags` tables with indexes |
| `000001_initial_schema.down.sql` | Drops tables |

---

## Design Decisions

- **Hash-based idempotency**: SHA-256 over raw payload bytes, stored as lowercase hex. Duplicate uploads within a collection return existing metadata.
- **Sparse tags**: tags are stored only when known (`true` or `false`). Absence means unknown and triggers on-demand evaluation.
- **Synchronous tagging**: queries block while the tagging engine evaluates missing tags. Retries are limited to 2 attempts with exponential backoff (`200ms`, `800ms`).
- **Hard deletes**: deleting an object removes metadata, tags, and S3 payload synchronously.
