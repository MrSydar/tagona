# AGENTS.md

Tagona is a Go monorepo with four workspace modules. The key mental model: all modules exist, `tagger` imports `mrsydar/tagona/storage/pkg/client`, and `api` is a standalone thin gateway — all resolved by the workspace, not by `go.mod`.

## Monorepo layout

- Root uses `go.work` with Go 1.26.4.
- Modules: `api/` (`mrsydar/tagona/api`), `keystorage/` (`mrsydar/tagona/keystorage`), `storage/` (`mrsydar/tagona/storage`) and `tagger/` (`mrsydar/tagona/tagger`).
- Cross-module dependencies:
  - `tagger` imports `mrsydar/tagona/storage/pkg/client` — both the storage-API Go client it uses to fetch object metadata/data, and the `Tagger` interface it implements (the cross-module contract the storage service uses to call the tagger).
  - `api` has no cross-module imports; it is a standalone thin gateway. The reference CLI client lives in the storage module (`storage/cmd/client`).
  - `tagger/go.mod` and `storage/go.mod` **do not** list the sibling modules as dependencies; the Go workspace resolves sibling modules locally.
  - Docker builds always copy all modules so the workspace is functional inside the builder.
  - Building locally from a module directory works because Go workspace resolves sibling modules automatically.

## Running the full stack

```bash
make docker-up
# or: docker compose up --build -d
```

- Postgres (`:5432`), Garage (`:3900` S3 API, `:3903` admin API), tagger (`:8081`), storage (`:8082`, internal — not exposed on the host), keystorage (`:8083`, internal), and api (`:8080`, the public entrypoint) all come up. A one-shot `db-init` service runs first and creates the per-service database roles (`postgres/roles.sql`).
- All services expose Prometheus metrics at `GET /metrics`.
- `compose.yaml` reads an **optional** `.env` for the tagger service (`required: false`; the stack starts without it and the tagger defaults to `grep`). Set `TAGGER_EVALUATOR_IMPL` there (`grep` works offline; `completions/openai`, `decisions/openai` and `decisions/vercel` need a valid API key — current local keys are expired, so use `grep`). The LLM-backed evaluators read `TAGGER_<KIND>_<DIALECT>_*` settings (base URL, path, auth header and scheme, extra headers, query and body params, ...), listed in `tagger/.env.example` and `tagger/README.md`; the part after the slash names the API dialect, not the vendor. Evaluators are created by `evaluator.New` (`tagger/pkg/evaluator/factory.go`); add one there. Running tagger standalone without `.env` defaults to `grep`.
- The public entrypoint is the **api** service on `:8080` (plain HTTP; `/v1/*` including the docs, `/healthz`, `/readyz`, `/metrics`). TLS, edge rate limiting and similar concerns are left to whatever you put in front of it.
- Startup ordering: postgres → db-init (completes) → keystorage / storage; garage → tagger → storage; api waits for storage and keystorage (`depends_on` with `condition: service_healthy`, or `service_completed_successfully` for db-init).
- Storage fails fast on startup if it cannot fetch supported types from tagger.
- Wait for healthy; then test via `README.md` Quick Start curl commands (public API on `:8080`).

## Building / running a single module locally

Requires Postgres + S3-compatible storage running (e.g., the Docker Compose infra).

**API service:**

```bash
cd api
go mod download
go run ./cmd/api
```

**Storage service:**

```bash
cd storage
go mod download
go run ./cmd/storage
```

**Keystorage service** (needs Postgres with the keys schema and role; see `keystorage/README.md`):

```bash
cd keystorage
go run ./cmd/keystorage
```

**Tagger service:**

```bash
cd tagger
go mod download
go run ./cmd/tagger
```

**Build binaries:**

```bash
make all              # builds bin/api, bin/keystorage, bin/storage, bin/tagger, bin/tagona
cd api && go build -o /tmp/api ./cmd/api
cd storage && go build -o /tmp/storage ./cmd/storage
cd tagger && go build -o /tmp/tagger ./cmd/tagger
```

## CLI client

A reference CLI client exists in the storage module:

```bash
make build-client     # builds bin/tagona
cd storage
go run ./cmd/client --url http://localhost:8080 <command>
```

See `storage/cmd/client/main.go` for available commands.

## Testing

**Unit tests:** standard Go `*_test.go` files in each module (`go test ./...`). The database tests in `storage/internal/db` and `keystorage/internal/db` need Postgres and are skipped unless `TAGONA_TEST_PG_DSN` is set (they run the real migrations in a throwaway schema; CI provides a Postgres service).

**Release script tests:** `.github/scripts/test-release-scripts.sh` tests the version, tag and changelog logic of the release workflow (CI job `release-scripts`). Change `release-meta.sh` / `release-notes.sh` and their tests together.

**End-to-end tests:** In the `e2e/` directory (its own module, outside the workspace):

```bash
make e2e
# or: cd e2e && GOWORK=off go test -v -count=1 .
```

- `GOWORK=off` is required because the root `go.work` file would otherwise be picked up from the parent directory, interfering with the e2e module.
- Requires the Docker Compose stack running and `http://localhost:8080/readyz` returning `200`.
- `TAGONA_URL` points the tests at another api (default `http://localhost:8080`) and `TAGONA_E2E_PG_ADDR` at the stack's Postgres (default `localhost:5432`, used by the database-role isolation test).
- Tests authenticate with admin HTTP Basic auth (`API_ADMIN_USERNAME`/`API_ADMIN_PASSWORD`, defaults `admin`/`tagona` matching compose) to mint an API key via `POST /v1/admin/api-keys`, then send `Authorization: Bearer <key>` on every `/v1/*` request.

## Important quirks

- **Auth:** the api service requires `Authorization: Bearer <api key>` on every `/v1/*` request (401 `missing_api_key` when missing/malformed, 401 `invalid_api_key` when unknown, 503 `not_ready` if keystorage is unreachable during validation). **API keys belong to the `keystorage` service**, the only one whose database role (`tagona_keys`) can open the `keys` schema where the `api_keys` table lives; storage's role (`tagona_storage`) has no access to it, and keystorage cannot read the data tables. Keys can expire (`ttl_seconds` on creation, `expires_at` everywhere a key is shown; expiry is judged by the database clock in the validation query, an expired key answers `401 expired_api_key` and not `invalid_api_key`, cached verdicts in the api are capped at the key's expiry, and a sweeper deletes expired keys after `KEYSTORAGE_EXPIRED_KEY_RETENTION`). Keystorage serves `POST /api-keys/validate` (open to anything on the compose network: the api, and in Tagona Plus the translator) and key management at `/api-keys` and `/api-keys/{id}` (create/list/delete), which **authenticates the admin HTTP Basic credentials itself** (`KEYSTORAGE_ADMIN_USERNAME`/`KEYSTORAGE_ADMIN_PASSWORD`; 403 `admin_disabled` when unset, 403 `forbidden` for an API key). The public api exposes `/v1/admin/api-keys` through three handlers that validate the request and make a new one to keystorage (the admin `Authorization` header is passed on, ids must be UUIDs); keystorage's validation endpoint is never reachable through it, and `TestAdminSurfaceIsStrictlyAllowlisted` plus the e2e `TestKeyValidationAndOtherRoutesAreNotExposed` guard that. Storage and tagger remain auth-free because they are internal-only. `/healthz`, `/readyz`, `/metrics` stay unauthenticated on the api service for Docker healthchecks and Prometheus scraping.
- **Custom migrations runner:** Storage applies migrations on startup by executing all `*.up.sql` files in `storage/migrations/` in lexicographic order, **on every startup**, each file as one transaction. It is not using `golang-migrate`, so migrations must be idempotent (`IF NOT EXISTS`, `CREATE OR REPLACE`, guarded one-off backfills like `000003`). There is no `000002`: it created `api_keys`, which moved to keystorage. Keystorage does the same with its embedded `keystorage/migrations/` in the `keys` schema (its connection's `search_path` is that schema only, so names are unqualified).
- **Database roles:** each service connects with its own Postgres role, created by `postgres/roles.sql`, which the one-shot `db-init` compose service runs as the bootstrap superuser before the services start and on every `up` (idempotent). `tagona_storage` owns the `public` schema's objects (storage migrates its own tables, so it must own them); `tagona_keys` owns the `keys` schema and nothing else. The script also upgrades a database created earlier: it moves `public.api_keys` to `keys` and re-owns the storage tables, so existing keys keep working. New objects storage creates need no changes here, but a new service touching the database needs its own role in that file. The compose passwords are demo defaults (`TAGONA_STORAGE_DB_PASSWORD`, `TAGONA_KEYS_DB_PASSWORD`). The e2e `TestDatabaseRolesAreIsolated` asserts the boundary.
- **Collection tag statistics:** `collections.object_count` and `collection_tags` (per-tag true/false counters, exposed at `GET /v1/collections/{collection}/tags`) are maintained by database triggers, not application code — do not update them from Go. Any code path that writes `object_tags` must go through `UpsertTags` (it keeps the lock order that prevents deadlocks; see `storage/README.md`).
- **Tagger talks to storage directly:** The tagger fetches object metadata and payloads from the internal storage service (`:8082`) via the `storage/pkg/client` HTTP client (no DB access). Storage's `readyz` checks DB + S3; api's `readyz` checks storage readiness; tagger's `readyz` always returns 200.
- **Startup ordering matters:** Storage must reach tagger on startup to fetch supported types; api must reach storage and keystorage (its `readyz` reports both). In Docker Compose this is enforced by `depends_on` + healthchecks: db-init → keystorage and storage; tagger → storage; storage + keystorage → api. Running storage standalone without tagger causes a fatal error.
- **api is a sanitizing gateway, not a proxy:** It owns no DB, no S3, no tagger access. It never forwards a client request. Each public route is a handler (`api/cmd/api/handlers.go`) that checks the request against what the route accepts (`sanitize.go`: only the documented query parameters, each at most once; strict JSON with no unknown fields or trailing data decoded into typed structs; no body on bodiless routes; collection names, UUIDs, cursors, dates, numbers and booleans checked for form and normalized) and rejects anything else with a 400, then builds a **new** request for storage or keystorage from the validated values (`upstream.go`). No inbound header is copied (only the admin `Authorization` goes to keystorage, and nothing else), uploads are streamed with a fixed `Content-Type`, and only `Content-Type`, `Content-Length` (plus keystorage's `WWW-Authenticate`) come back from the service. Limits that are configurable in storage (max page size, max tags) stay there; the gateway checks shape, not policy. A new route is a new handler plus a test for what it must reject. Unsafe paths (`..`, `//`, encoded slashes) get 400, bodies over `API_MAX_BODY_BYTES` get 413 (256 KiB for JSON, 4 KiB for key management), and key validation is cached (`API_KEY_CACHE_TTL`; a successful delete through the gateway purges it).
- **API docs / OpenAPI:** `api/openapi/v1.yaml` is the hand-written contract of the `/v1` API, embedded in the api binary and served publicly (no key needed) at `GET /v1/openapi.json`, `/v1/openapi.yaml` and `/v1/docs` (Swagger UI from a pinned, SRI-protected CDN URL). Any route you add to `proxiedRoutes` or the admin routes must also be added to the spec — `TestOpenAPIMatchesRoutes` fails otherwise — and `info.version` should be bumped for contract changes. The spec is YAML, so quote descriptions that contain `: `. Lint with `npx @redocly/cli lint api/openapi/v1.yaml` (CI does this).
- **Docker images:** `api`, `keystorage`, `storage` and `tagger` each have a `Dockerfile` that builds from the **repo root** (they need `go.work` and every module, so each Dockerfile copies every module's `go.mod`; add a new module to all of them). The Go build stage runs on `$BUILDPLATFORM` and cross-compiles (`GOOS/GOARCH=$TARGETOS/$TARGETARCH`), and the runtime stage is `debian:12-slim` plus `curl` (used by the compose healthchecks) running as **uid 10001**, not root. Keep the `org.opencontainers.image.source` label: it links the GHCR package to this repo. The `docker-build` CI job builds all four and fails if an image runs as root or lacks the label.
- **Compose image names:** the compose services have `image: ghcr.io/mrsydar/tagona-<service>:${TAGONA_VERSION:-dev}` **and** `build:`. `docker compose up --build` builds from source and tags `:dev`; to run a published version use `TAGONA_VERSION=<version> docker compose pull ... && docker compose up -d --no-build`.
- **Releases:** pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml` (verify → build ×4 for amd64+arm64 as `:sha-<commit>` → e2e against those pushed images → approval on the `release` environment → retag `X.Y.Z`/`X.Y`/`X`/`latest` without rebuilding + cosign signature → GitHub Release). All four services share one version. **Never push a release tag unless the user asks for a release.** Full flow, one-time setup and rollback notes are in `RELEASING.md`; a manual run of the workflow is a dry run that never pushes.
  - Third-party actions in `release.yml` are pinned to commit SHAs (version in a comment); bump them deliberately and run `actionlint`.
  - To read an image digest use `docker buildx imagetools inspect <ref> --format '{{json .Manifest}}' | jq -r .digest`; `{{.Manifest.Digest}}` prints the whole descriptor block.
  - The registry, signing and GitHub Release steps only run on GitHub, so they are first exercised by a release candidate (`v0.1.0-rc.1`).
- **Hash-based idempotency:** Object upload computes SHA-256 over raw bytes. Duplicate uploads in the same collection return the existing object; the newly uploaded S3 object is not retained.
- **Prometheus metrics:** All services expose metrics at `GET /metrics` via `github.com/prometheus/client_golang`.
  - Storage metrics: `storage_requests_total`, `storage_errors_total`, `storage_tagger_latency_seconds`.
  - Keystorage metrics: `keystorage_requests_total`, `keystorage_errors_total`.
  - API metrics: `api_requests_total`, `api_errors_total`.
  - Tagger metrics: `tagger_requests_total`, `tagger_errors_total`, `tagger_evaluator_latency_seconds`.

## Git workflow

- **Always create a new branch when working on a feature.** Do not commit directly to the default branch (`main`).
- **New CI jobs become required checks only after they exist on `main`.** When you add a job to `ci.yml` or `unit-tests.yml`, add its check name (matrix jobs appear as `job (value)`) to the `main` ruleset after the PR merges, otherwise it is never enforced.

## References

- `README.md` — architecture, API shapes, quick start curls
- `api/README.md` — public API docs, env vars
- `storage/README.md` — internal data service env vars, CLI client usage, migrations, design decisions
- `keystorage/README.md` — API key service: endpoints, database role and schema, env vars
- `tagger/README.md` — tag evaluation logic, env vars
- `RELEASING.md` — how the Docker images are released: flow, one-time GitHub setup, verification, rollback
