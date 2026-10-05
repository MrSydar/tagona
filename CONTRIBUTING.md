# Contributing to Tagona

Thank you for your interest in contributing! This document explains the workflow and conventions we use.

## Development Setup

### Prerequisites

*   [Go 1.26+](https://go.dev/dl/)
*   [Docker](https://docs.docker.com/get-docker/) + Docker Compose
*   Make

### Clone & Build

```bash
git clone git@github.com:MrSydar/tagona.git
cd tagona
make all                 # builds bin/api, bin/storage, bin/tagger, bin/tagona
```

### Running the Stack

```bash
make docker-up           # starts Postgres, Garage, tagger, storage, api, Traefik, Prometheus, Grafana
# wait ~15s for healthchecks
make e2e                 # runs end-to-end tests
make docker-down         # tears everything down
```

The public entrypoint is Traefik: `http://localhost:8080` (plain HTTP) and `https://localhost:8443` (TLS, self-signed — use `curl -k`). The `api` and `storage` containers do not publish host ports.

Optionally create a `.env` in the repo root to configure the tagger (compose reads it if present; without it the tagger uses the `grep` evaluator). Use `TAGGER_EVALUATOR_IMPL=grep`, which works offline; `openai` and `systemone` need a valid API key. See `tagger/.env.example`.

## Project Layout

This is a **Go workspace monorepo** (`go.work` at the root).

| Directory | Module | Description |
|-----------|--------|-------------|
| `api/` | `mrsydar/tagona/api` | Public API gateway: auth, per-key rate limiting, allowlisted reverse proxy to storage |
| `storage/` | `mrsydar/tagona/storage` | Internal data service, DB migrations, S3 client, Go client (`pkg/client`) and CLI |
| `tagger/` | `mrsydar/tagona/tagger` | Tag-evaluation engine |
| `traefik/` | — | Edge proxy config (TLS, per-IP rate limiting); see [`traefik/README.md`](traefik/README.md) |
| `e2e/` | standalone module | End-to-end tests (run with `GOWORK=off`) |

> **Adding a public route:** the api service proxies an explicit allowlist. A new storage route is **not** reachable publicly until you add it to `proxiedRoutes` in `api/cmd/api/gateway.go`. Also document it in `api/openapi/v1.yaml` (the OpenAPI contract served at `/v1/docs`) and `api/README.md`; `go test ./api/...` fails if the spec and the routes disagree, and CI lints the spec (`npx @redocly/cli lint api/openapi/v1.yaml`).

> **Important:** `tagger` imports `mrsydar/tagona/storage/pkg/client`. This is resolved by the Go workspace, **not** by listing `storage` in `tagger/go.mod`. Building from a module directory works because Go automatically resolves sibling workspace modules.

## Workflow

1. **Create a branch** off the latest `main`:
   ```bash
   git checkout -b feature/your-feature-name
   ```
   Don't commit directly to `main`.
2. **Make your changes.** Follow existing code style.
3. **Add tests.** If you add code, add unit tests in `*_test.go` files (standard Go `testing` only). The `api` module has examples in `api/cmd/api/*_test.go`.
4. **Run checks locally:**
   ```bash
   make all                 # ensure everything compiles
   gofmt -l .               # should print nothing
   for m in api storage tagger; do (cd $m && go vet ./... && go test ./...); done
   # database tests (skipped without a DSN); needs `make docker-up` or any Postgres
   TAGONA_TEST_PG_DSN='postgres://tagona:tagona@localhost:5432/tagona?sslmode=disable' go test -race ./storage/internal/db
   make e2e                 # needs the stack up and /readyz on :8080 returning 200
   .github/scripts/test-release-scripts.sh   # tests of the release scripts
   ```
   CI also builds the three Docker images (`docker-build`) and checks that they run as a non-root user; `docker compose up --build` is the local equivalent.
5. **Commit** using clear messages. We prefer [Conventional Commits](https://www.conventionalcommits.org/):
   ```
   feat: add pagination cursor to tag queries
   fix(storage): handle nil tag map in query builder
   docs: update README with new env vars
   ```
6. **Push** and open a **Pull Request** against `main`.
7. **CI** will run automatically. Ensure it is green before requesting review.

## Releases

Maintainers cut releases by pushing a `vX.Y.Z` tag; CI then builds, tests, signs and publishes the three service images to GHCR. Contributors do not need to do anything, other than describing user-facing changes under `## [Unreleased]` in `CHANGELOG.md`. The whole flow is described in [RELEASING.md](RELEASING.md).

## Pull Request Guidelines

*   Keep changes focused — one logical change per PR.
*   Explain the "why" in the PR description, not just the "what".
*   Update documentation (`README.md`, `AGENTS.md`, `CHANGELOG.md`, module READMEs) if behavior changes.
*   Be kind and constructive in review discussions.

## Code Style

*   Standard Go formatting (`gofmt`).
*   `go vet ./...` should pass without warnings in each module.
*   `traefik/dynamic.yml` is a Go template: don't put `{{` in comments there.
*   Keep exported APIs minimal and well-documented.

## Reporting Bugs

Use the [GitHub issue tracker](https://github.com/MrSydar/tagona/issues) and choose the "Bug report" template.

## Questions?

Feel free to open a GitHub Discussion or an issue with the "Question" label.
