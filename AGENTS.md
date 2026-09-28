# AGENTS.md

Tagona is a Go monorepo with three workspace modules. The key mental model: all modules exist, `tagger` imports `mrsydar/tagona/storage/pkg/client`, and `api` is a standalone thin gateway — all resolved by the workspace, not by `go.mod`.

## Monorepo layout

- Root uses `go.work` with Go 1.26.4.
- Modules: `api/` (`mrsydar/tagona/api`), `storage/` (`mrsydar/tagona/storage`) and `tagger/` (`mrsydar/tagona/tagger`).
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

- Postgres (`:5432`), Garage (`:3900` S3 API, `:3903` admin API), tagger (`:8081`), storage (`:8082`, internal — not exposed on the host), and api (`:8080`) all come up.
- All services expose Prometheus metrics at `GET /metrics`.
- `compose.yaml` mounts `.env` into the tagger service. Set `TAGGER_EVALUATOR_IMPL` there (`grep` works offline; `openai`/`systemone` need a valid API key — current local keys are expired, so use `grep`). `systemone` (backend `vercel`) additionally uses `TAGGER_VERCEL_API_KEY`, `TAGGER_VERCEL_MODEL`, `TAGGER_VERCEL_THRESHOLD`. Running tagger standalone without `.env` defaults to `grep`.
- Startup ordering: postgres/garage → tagger → storage → api. Storage waits for tagger to be healthy, and api waits for storage (`depends_on` with `condition: service_healthy`).
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

**Tagger service:**

```bash
cd tagger
go mod download
go run ./cmd/tagger
```

**Build binaries:**

```bash
make all              # builds bin/api, bin/storage, bin/tagger, bin/tagona
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

**Unit tests:** `api/`, `storage/` and `tagger/` have none currently. Add `*_test.go` files as usual — standard Go only.

**End-to-end tests:** In the `e2e/` directory (its own module, outside the workspace):

```bash
make e2e
# or: cd e2e && GOWORK=off go test -v -count=1 .
```

- `GOWORK=off` is required because the root `go.work` file would otherwise be picked up from the parent directory, interfering with the e2e module.
- Requires the Docker Compose stack running and `http://localhost:8080/readyz` returning `200`.

## Important quirks

- **Custom migrations runner:** Storage applies migrations on startup by executing all `*.up.sql` files in `storage/migrations/` in lexicographic order. It is not using `golang-migrate`.
- **Tagger talks to storage directly:** The tagger fetches object metadata and payloads from the internal storage service (`:8082`) via the `storage/pkg/client` HTTP client (no DB access). Storage's `readyz` checks DB + S3; api's `readyz` checks storage readiness; tagger's `readyz` always returns 200.
- **Startup ordering matters:** Storage must reach tagger on startup to fetch supported types; api must reach storage (its `readyz` reports storage availability). In Docker Compose this is enforced by `depends_on` + healthchecks: tagger → storage → api. Running storage standalone without tagger causes a fatal error.
- **api is a thin reverse proxy:** It owns no DB, no S3, no validation, no tagger access. It proxies `/v1/*` verbatim to storage on `:8082` and serves `/healthz`, `/readyz`, `/metrics` locally. Storage serves the same routes on `:8082` — no path rewriting.
- **Hash-based idempotency:** Object upload computes SHA-256 over raw bytes. Duplicate uploads in the same collection return the existing object; the newly uploaded S3 object is not retained.
- **Prometheus metrics:** All services expose metrics at `GET /metrics` via `github.com/prometheus/client_golang`.
  - Storage metrics: `storage_requests_total`, `storage_errors_total`, `storage_tagger_latency_seconds`.
  - API metrics: `api_requests_total`, `api_errors_total`.
  - Tagger metrics: `tagger_requests_total`, `tagger_errors_total`, `tagger_evaluator_latency_seconds`.

## Git workflow

- **Always create a new branch when working on a feature.** Do not commit directly to the default branch (`main`).

## References

- `README.md` — architecture, API shapes, quick start curls
- `api/README.md` — public API docs, env vars
- `storage/README.md` — internal data service env vars, CLI client usage, migrations, design decisions
- `tagger/README.md` — tag evaluation logic, env vars
