# AGENTS.md

Tagona is a Go monorepo with two workspace modules. The key mental model: both modules exist, and `tagger` imports `storage/pkg/client` resolved by the workspace, not by `go.mod`.

## Monorepo layout

- Root uses `go.work` with Go 1.26.4.
- Modules: `storage/` (`mrsydar/tagona/storage`) and `tagger/` (`mrsydar/tagona/tagger`).
- Cross-module dependency: `tagger` imports `mrsydar/tagona/storage/pkg/client`.
  - `tagger/go.mod` **does not** list `storage` as a dependency; the Go workspace resolves the sibling module locally.
  - Docker builds always copy both modules so the workspace is functional inside the builder.
  - Building locally from a module directory works because Go workspace resolves the sibling module automatically.

## Running the full stack

```bash
make docker-up
# or: docker compose up --build -d
```

- Postgres (`:5432`), Garage (`:3900` S3 API, `:3903` admin API), tagger (`:8081`), and storage (`:8080`) all come up.
- Both services expose Prometheus metrics at `GET /metrics`.
- `compose.yaml` mounts `.env` into the tagger service. Set `TAGGER_EVALUATOR_IMPL` there (`grep` works offline; `openai`/`vercel` need a valid API key — current local keys are expired, so use `grep`). `vercel` additionally uses `TAGGER_VERCEL_API_KEY`, `TAGGER_VERCEL_MODEL`, `TAGGER_VERCEL_THRESHOLD`. Running tagger standalone without `.env` defaults to `grep`.
- Storage waits for tagger to be healthy before starting (`depends_on` with `condition: service_healthy`).
- Storage fails fast on startup if it cannot fetch supported types from tagger.
- Wait for healthy; then test via `README.md` Quick Start curl commands.

## Building / running a single module locally

Requires Postgres + S3-compatible storage running (e.g., the Docker Compose infra).

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
make all              # builds bin/storage, bin/tagger, bin/tagona-client
cd storage && go build -o /tmp/storage ./cmd/storage
cd tagger && go build -o /tmp/tagger ./cmd/tagger
```

## CLI client

A reference CLI client exists in the storage module:

```bash
make build-client     # builds bin/tagona-client
cd storage
go run ./cmd/client --url http://localhost:8080 <command>
```

See `storage/cmd/client/main.go` for available commands.

## Testing

**Unit tests:** `storage/` and `tagger/` have none currently. Add `*_test.go` files as usual — standard Go only.

**End-to-end tests:** In the `e2e/` directory (its own module, outside the workspace):

```bash
make e2e
# or: cd e2e && GOWORK=off go test -v -count=1 .
```

- `GOWORK=off` is required because the root `go.work` file would otherwise be picked up from the parent directory, interfering with the e2e module.
- Requires the Docker Compose stack running and `storage` readyz returning `200`.

## Important quirks

- **Custom migrations runner:** Storage applies migrations on startup by executing all `*.up.sql` files in `storage/migrations/` in lexicographic order. It is not using `golang-migrate`.
- **Tagger dogfoods storage public API:** The tagger only talks to storage via the public `pkg/client` HTTP client (no internal APIs, no DB access). Storage's `readyz` checks DB + S3; tagger's `readyz` always returns 200.
- **Startup ordering matters:** Storage must reach tagger on startup to fetch supported types. In Docker Compose this is enforced by `depends_on` + healthchecks. Running storage standalone without tagger causes a fatal error.
- **Hash-based idempotency:** Object upload computes SHA-256 over raw bytes. Duplicate uploads in the same collection return the existing object; the newly uploaded S3 object is not retained.
- **Prometheus metrics:** Both services expose metrics at `GET /metrics` via `github.com/prometheus/client_golang`.
  - Storage metrics: `storage_requests_total`, `storage_errors_total`, `storage_tagger_latency_seconds`.
  - Tagger metrics: `tagger_requests_total`, `tagger_errors_total`, `tagger_evaluator_latency_seconds`.

## Git workflow

- **Always create a new branch when working on a feature.** Do not commit directly to the default branch (`main`).

## References

- `README.md` — architecture, API shapes, quick start curls
- `storage/README.md` — env vars, CLI client usage, internal structure
- `tagger/README.md` — tag evaluation logic, env vars
