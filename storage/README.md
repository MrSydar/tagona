# Storage Service

Go module: `mrsydar/tagona/storage`

The internal data service for Tagona, listening on `:8082`. It handles collections, object upload/download, metadata, tag queries, and on-demand tag evaluation via the tagging engine.

It is not exposed to the public: the [api service](../api/) (port `:8080`) is the public gateway and reverse-proxies all `/v1/*` requests verbatim to this service. The routes served here are identical to the public API paths — see [`api/README.md`](../api/) for the full API documentation.

---

## Responsibilities

- **Collections**: create, list, validate, and delete collections
- **Object Storage**: stream payloads to S3-compatible storage, compute SHA-256 content hashes
- **Metadata**: store object metadata and sparse tags in Postgres
- **Tag Queries**: evaluate missing tags on-demand to satisfy queries with limit/pagination
- **Retention**: background sweeper deleting expired objects by TTL

---

## API

The storage service answers the same routes it serves to the api gateway (no path rewriting happens in the proxy):

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Liveness (always `200` if up) |
| `GET` | `/readyz` | Readiness (checks DB + S3 connectivity) |
| `GET` | `/metrics` | Prometheus metrics (`storage_*`) |
| `GET` | `/v1/collections` | List all collections |
| `POST` | `/v1/collections` | Create a collection |
| `DELETE` | `/v1/collections/{collection}` | Delete a collection |
| `POST` | `/v1/collections/{collection}/objects` | Upload an object |
| `GET` | `/v1/collections/{collection}/objects/{id}` | Get metadata |
| `GET` | `/v1/collections/{collection}/objects/{id}/data` | Download payload |
| `GET` | `/v1/collections/{collection}/objects/{id}/tags` | Get tags |
| `POST` | `/v1/collections/{collection}/objects/query` | Query by tags |
| `DELETE` | `/v1/collections/{collection}/objects/{id}` | Hard delete |

### Internal API key endpoints

The storage service also owns API key persistence for the api gateway. These routes are **internal** — unauthenticated and not reachable through the public gateway (which only proxies `/v1/*`):

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/internal/v1/api-keys` | Create an API key. Body `{"name":"..."}` (non-empty, valid UTF-8, ≤128 bytes). Returns `201` `{"id","name","key","created_at"}` — the raw key (`tagona_` + 64 hex chars) appears only here; only its SHA-256 hash is stored |
| `GET` | `/internal/v1/api-keys` | List API keys. Returns `200` `{"keys":[{"id","name","key_prefix","created_at"}]}` |
| `DELETE` | `/internal/v1/api-keys/{id}` | Delete an API key. `204` on success, `404` if unknown |
| `POST` | `/internal/v1/api-keys/validate` | Validate a raw key. Body `{"key":"..."}`. Found → `200` `{"id","name"}`; unknown → `401` `{"error":{"code":"invalid_api_key","message":"invalid or unknown api key"}}` |

No auth is enforced here on purpose: storage is internal-only. The api service validates Bearer keys via `/internal/v1/api-keys/validate` and manages keys via the create/list/delete routes, exposing them publicly under `/v1/admin/api-keys` behind admin Basic auth.

---

## Configuration

| Env Var | Required | Default | Description |
|---------|----------|---------|-------------|
| `TAGONA_HTTP_ADDR` | No | `:8082` | HTTP listen address |
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

Standalone (requires Postgres, S3-compatible storage, and the tagging engine running):

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

## Internal Structure

```
storage/
├── cmd/storage/           # main entry point
├── cmd/client/            # reference CLI client for the Tagona API
├── internal/
│   ├── config/            # env parsing
│   ├── cursor/            # pagination cursor encode/decode
│   ├── db/                # Postgres queries and transactions
│   ├── keys/              # API key generation and hashing
│   ├── models/            # shared struct types
│   ├── query/             # tag query + scan logic
│   ├── retention/         # TTL background sweeper
│   ├── server/            # HTTP handlers (chi router)
│   ├── storage/           # S3 client (AWS SDK v2)
│   └── validate/          # collection/tag/date validation
├── pkg/client/            # public Go client + Tagger contract
│   ├── client.go          # collections, objects, queries
│   ├── tagger.go          # Tagger client interface
│   └── instrumented_tagger.go # metrics-instrumented Tagger wrapper
└── migrations/            # SQL schema files
```

The `pkg/client` package is the reusable Go HTTP client for the Tagona data API. Since the storage service serves the same routes the api gateway exposes publicly, the client works against both `:8082` (internal) and `:8080` (via the api gateway). It is consumed by the tagging engine to fetch object metadata and payloads.

### CLI Client

A reference CLI client is available at `cmd/client`. Every request requires an API key (sent as `Authorization: Bearer <api-key>`) — pass it with `--token` or the `API_TOKEN` env var:

```bash
go run ./cmd/client --url http://localhost:8080 --token "$API_TOKEN" <command> [options]
```

When pointing the client at the api gateway (`:8080`), use a key created via the admin endpoints (see [`api/README.md`](../api/)); when pointing it directly at storage (`:8082`), the header is accepted but not enforced.

### Commands

```bash
# Collections (all commands require --token or API_TOKEN)
client --url http://localhost:8080 --token "$API_TOKEN" list-collections
client --url http://localhost:8080 --token "$API_TOKEN" create-collection --name jobs --data-type txt
client --url http://localhost:8080 --token "$API_TOKEN" delete-collection --collection jobs

# Objects
client --url http://localhost:8080 --token "$API_TOKEN" upload --collection jobs --data-type txt --file hello.txt
client --url http://localhost:8080 --token "$API_TOKEN" get --collection jobs --id <id>
client --url http://localhost:8080 --token "$API_TOKEN" data --collection jobs --id <id> --out hello.txt
client --url http://localhost:8080 --token "$API_TOKEN" tags --collection jobs --id <id> --tags golang,qa
client --url http://localhost:8080 --token "$API_TOKEN" query --collection jobs --tags '{"golang":true}' --limit 5 --timeout 30000
client --url http://localhost:8080 --token "$API_TOKEN" query --collection jobs --tags '{"golang":true}' --limit 5 --best-effort
client --url http://localhost:8080 --token "$API_TOKEN" delete --collection jobs --id <id>
```

---

## Migrations

Migrations are applied automatically on startup using a simple file-based runner.

| File | Description |
|------|-------------|
| `000001_initial_schema.up.sql` | Creates `collections`, `objects`, `object_tags` tables with indexes |
| `000001_initial_schema.down.sql` | Drops tables |
| `000002_api_keys.up.sql` | Creates the `api_keys` table (hash, prefix, name) used for Bearer API key auth on the api gateway |
| `000002_api_keys.down.sql` | Drops `api_keys` |

---

## Design Decisions

- **Hash-based idempotency**: SHA-256 over raw bytes, stored as lowercase hex. Duplicate uploads in the same collection return the existing object; the newly uploaded S3 payload is not retained.
- **Sparse tags**: tags are stored only when known (`true` or `false`). Absence means unknown and triggers on-demand evaluation.
- **Synchronous tagging**: queries block while the tagging engine evaluates missing tags. Retries are limited to 2 attempts with exponential backoff.
- **Hard deletes**: deleting an object removes metadata, tags, and S3 payload synchronously.
- **Internal only**: no auth or RBAC here — that responsibility belongs to the api gateway.
