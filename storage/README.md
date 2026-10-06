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
| `GET` | `/v1/collections/{collection}/tags` | Object count and registered tags with per-tag counts (`?prefix=&limit=&cursor=`; see [`api/README.md`](../api/README.md#public-api)) |
| `POST` | `/v1/collections/{collection}/objects` | Upload an object |
| `GET` | `/v1/collections/{collection}/objects/{id}` | Get metadata |
| `GET` | `/v1/collections/{collection}/objects/{id}/data` | Download payload |
| `GET` | `/v1/collections/{collection}/objects/{id}/tags` | Get tags |
| `POST` | `/v1/collections/{collection}/objects/query` | Query by tags |
| `DELETE` | `/v1/collections/{collection}/objects/{id}` | Hard delete |

API keys are not stored here: the [keystorage](../keystorage/README.md) service owns them, under its own database role. This service's role has no access to the keys schema, and it neither validates nor manages keys.

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
| `TAGONA_RETENTION_SWEEP_INTERVAL` | No | `60s` | How often the retention sweeper runs |
| `TAGONA_RETENTION_BATCH_SIZE` | No | `100` | Expired objects the sweeper fetches and removes per batch. A run keeps going batch after batch until no expired object is left, so this only bounds the work per query, not the throughput |

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

The trigger/statistics tests in `internal/db` need a real Postgres and are skipped unless `TAGONA_TEST_PG_DSN` is set. Each test applies the real migrations in a throwaway schema and drops it afterwards, so it is safe to point at the compose database:

```bash
TAGONA_TEST_PG_DSN='postgres://tagona:tagona@localhost:5432/tagona?sslmode=disable' go test -race ./internal/db
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
client --url http://localhost:8080 --token "$API_TOKEN" collection-tags --collection jobs [--prefix lang] [--limit 100] [--cursor <next>]

# Objects
client --url http://localhost:8080 --token "$API_TOKEN" upload --collection jobs --data-type txt --file hello.txt
client --url http://localhost:8080 --token "$API_TOKEN" get --collection jobs --id <id>
client --url http://localhost:8080 --token "$API_TOKEN" data --collection jobs --id <id> --out hello.txt
client --url http://localhost:8080 --token "$API_TOKEN" tags --collection jobs --id <id> --tags golang,qa
client --url http://localhost:8080 --token "$API_TOKEN" tags --collection jobs --id <id> --tags golang,qa --evaluate=false   # null for tags not yet evaluated
client --url http://localhost:8080 --token "$API_TOKEN" query --collection jobs --tags '{"golang":true}' --limit 5 --timeout 30000
client --url http://localhost:8080 --token "$API_TOKEN" query --collection jobs --tags '{"golang":true}' --limit 5 --best-effort
client --url http://localhost:8080 --token "$API_TOKEN" query --collection jobs --tag golang=true --evaluate=false   # known tags only, no tagger call
client --url http://localhost:8080 --token "$API_TOKEN" delete --collection jobs --id <id>
```

---

## Migrations

Migrations are applied automatically on startup using a simple file-based runner.

| File | Description |
|------|-------------|
| `000001_initial_schema.up.sql` | Creates `collections`, `objects`, `object_tags` tables with indexes |
| `000001_initial_schema.down.sql` | Drops tables |
| `000003_collection_tag_stats.up.sql` | Adds `collections.object_count` and the `collection_tags` registry (per-tag true/false counters), the triggers that maintain them, and a one-off backfill for existing data |
| `000003_collection_tag_stats.down.sql` | Drops the triggers, functions, `collection_tags` and `object_count` |

There is no `000002`: it created `api_keys`, which moved to the keystorage service.

---

## Design Decisions

- **Hash-based idempotency**: SHA-256 over raw bytes, stored as lowercase hex. Duplicate uploads in the same collection return the existing object; the newly uploaded S3 payload is not retained.
- **Sparse tags**: tags are stored only when known (`true` or `false`). Absence means unknown and triggers on-demand evaluation.
- **Synchronous tagging**: queries block while the tagging engine evaluates missing tags. Retries are limited to 2 attempts with exponential backoff.
- **Trigger-maintained collection statistics**: `collections.object_count` and the `collection_tags` registry are updated by statement-level triggers (transition tables) on `objects` and `object_tags`, inside the same transaction as the write. They also cover cascading deletes (object delete, retention sweep, collection delete), so nothing in the application can forget to update them. Statements are aggregated per `(collection, tag)`, so a bulk change costs one counter update per tag. Reading is O(tags in the collection) and subtracts objects that have expired but are not yet swept, so numbers match what reads return.
- **Counter lock order**: concurrent writers on the same tags contend on counter rows. To stay deadlock-free every writer follows one order — object row, then counter rows in sorted tag order, then the collection row (`UpsertTags` pre-locks its counters; triggers sort theirs) — and deadlock victims are retried (`retryOnDeadlock`). Migration `000003` runs its backfill only once (guarded by the existing column) under a write lock, because migrations are re-run on every startup.
- **Optional evaluation (`evaluate`)**: tag queries and the object tags endpoint accept `evaluate=false`, which skips the tagging engine entirely. Queries then run as one indexed SQL query over `object_tags` (`QueryObjectsKnownTags`) and return only objects whose requested tags are all known and matching; the object tags endpoint returns `null` for requested tags that are not known. The default stays `true`.
- **Retention sweeper**: expired objects (`expires_at` in the past) are already invisible to every read; the sweeper reclaims their space. Each run drains the whole backlog in batches (`TAGONA_RETENTION_BATCH_SIZE`), walking it with a keyset cursor over `(expires_at, id)` so an object that cannot be removed never blocks the ones behind it. For each object the payload is deleted from S3 first and the row second: if the S3 delete fails the row stays and the object is retried on the next run, instead of leaving an orphaned payload nothing refers to (deleting a missing S3 key is a no-op, so retries are safe). Each run logs one summary line (`deleted`, `failed`) rather than a line per object. The sweep is deliberately not one long transaction: deleting an object cascades to its tags and updates the collection counters, and holding those locks across a whole batch would bring back the deadlocks described above.
- **Hard deletes**: deleting an object removes metadata, tags, and S3 payload synchronously.
- **Internal only**: no auth or RBAC here — that responsibility belongs to the api gateway.
