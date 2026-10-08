# Keystorage

Stores and validates API keys. It is the only service that can reach the `api_keys` table: it connects to Postgres with its own role (`tagona_keys`), which owns the `keys` schema and has no privilege on the data tables, and the storage role (`tagona_storage`) has none on `keys`. Keys are stored as SHA-256 hashes; the raw key (`tagona_` + 64 hex characters) is returned once, when it is created.

Keystorage listens on `:8083` on the compose network and is not published on the host.

## Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/api-keys/validate` | none | Body `{"key":"..."}`. Valid → `200` `{"id","name","expires_at"}` (`expires_at` is `null` for a key that never expires); unknown → `401` `invalid_api_key`; **expired → `401` `expired_api_key`**; empty → `400` `missing_key` |
| `POST` | `/api-keys` | admin Basic | Create a key. Body `{"name":"...","ttl_seconds":14400}`: `name` is 1–128 bytes of valid UTF-8, `ttl_seconds` is optional (see [Expiry](#expiry)). `201` `{"id","name","key","created_at","expires_at"}` |
| `GET` | `/api-keys` | admin Basic | List keys (never the raw key), newest first, expired ones included until swept: `200` `{"keys":[{"id","name","key_prefix","created_at","expires_at"}],"next":"..."}`. Paged: `?limit=` (1–1000, default 100; else `400` `invalid_limit`) and `?cursor=` (the previous `next`; else `400` `invalid_cursor`). `next` is present only when more keys follow |
| `DELETE` | `/api-keys/{id}` | admin Basic | `204`; `404` for an unknown id or one that is not a UUID |
| `GET` | `/healthz`, `/readyz`, `/metrics` | none | Liveness; readiness (database ping); Prometheus metrics |

**Validation is open to the network on purpose.** Its callers (the api gateway, and in Tagona Plus the translator) hold the key they check, not a service credential, so there is nothing to authenticate with. It can only answer yes or no for a key the caller already has. **Key management checks the admin credentials itself**, so reaching the network is not enough to mint, list or delete a key: a request without valid Basic credentials gets `401 invalid_admin_credentials` (with `WWW-Authenticate`), an API key in the `Authorization` header gets `403 forbidden`, and unset admin credentials disable the endpoints with `403 admin_disabled`.

The public api exposes the three management routes only (as `/v1/admin/api-keys`...). It validates each request and makes a new one to keystorage, passing on only the caller's `Authorization` header. The validation endpoint is not reachable through it. Like the other internal services, keystorage's own routes have no `/v1` prefix.

## Configuration

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `KEYSTORAGE_PG_DSN` | Yes | — | Postgres DSN for the `tagona_keys` role |
| `KEYSTORAGE_PG_SCHEMA` | No | `keys` | Schema holding `api_keys`; the connection's `search_path` is this schema only |
| `KEYSTORAGE_HTTP_ADDR` | No | `:8083` | Listen address |
| `KEYSTORAGE_ADMIN_USERNAME` | No | — | Admin username for key management; both admin variables must be set |
| `KEYSTORAGE_ADMIN_PASSWORD` | No | — | Admin password for key management |
| `KEYSTORAGE_DEFAULT_KEY_TTL` | No | `0` | Lifetime of a key created without `ttl_seconds`; `0` means it never expires (unless a maximum is set). A Go duration, e.g. `24h` |
| `KEYSTORAGE_MAX_KEY_TTL` | No | `0` | Largest `ttl_seconds` that may be asked for; `0` means no cap. With a cap and no default, an omitted `ttl_seconds` gets the cap, so no key outlives it |
| `KEYSTORAGE_EXPIRED_KEY_RETENTION` | No | `24h` | How long an expired key stays in the list before the sweeper deletes it |
| `KEYSTORAGE_SWEEP_INTERVAL` | No | `1m` | How often expired keys past their retention are deleted (at least `1s`) |

## Expiry

A key may expire. `ttl_seconds` on creation makes it stop working that many seconds from now (at least 1; at most the configured cap, and never more than 100 years). Leaving it out uses `KEYSTORAGE_DEFAULT_KEY_TTL`, which is no expiry unless configured. `0` is refused (`invalid_ttl`): it is not a way to say "never".

- **Judged by the database's clock**, in the validation query itself, so there is one clock and the API cannot disagree with it. The key stops working at `expires_at`, exactly.
- **An expired key is not an unknown one:** validation answers `401 expired_api_key`, so a client that holds one knows to get a new key. Unknown and malformed keys keep `invalid_api_key`.
- **Cached verdicts never outlive the key.** The api caches validation answers briefly; `expires_at` in the answer caps each cached verdict, so an expiry takes effect when it happens and not when a cache entry would have run out.
- **Expired keys are swept:** they stay listed (with `expires_at`) for `KEYSTORAGE_EXPIRED_KEY_RETENTION`, then a background sweeper deletes them. It also runs at start, for keys that expired while the service was down.

## Database

The role and the schema are created by [`postgres/roles.sql`](../postgres/roles.sql), which the one-shot `db-init` compose service runs as the bootstrap superuser before the services start (and on every `up`; it is idempotent). It also upgrades a database from before keystorage existed by moving `public.api_keys` into `keys`, so existing keys keep working. Keystorage itself creates only the table, from the embedded [`migrations/`](migrations/), on every start (the migrations are idempotent).

## Develop

```bash
cd keystorage && go test ./...                 # the database tests are skipped without a database
TAGONA_TEST_PG_DSN='postgres://tagona:tagona@localhost:5432/tagona?sslmode=disable' go test -race ./...
```

The database tests apply the real migrations in a throwaway schema and drop it afterwards, so they are safe to point at the compose database.
