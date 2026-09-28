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
make all                 # builds bin/storage, bin/tagger, bin/tagona
```

### Running the Stack

```bash
make docker-up           # starts Postgres, Garage, storage, tagger
# wait ~15s for healthchecks
make e2e                 # runs end-to-end tests
make docker-down         # tears everything down
```

## Project Layout

This is a **Go workspace monorepo** (`go.work` at the root).

| Directory | Module | Description |
|-----------|--------|-------------|
| `storage/` | `mrsydar/tagona/storage` | HTTP API service, DB migrations, S3 client |
| `tagger/` | `mrsydar/tagona/tagger` | Tag-evaluation engine |
| `e2e/` | standalone module | End-to-end tests (run with `GOWORK=off`) |

> **Important:** `tagger` imports `mrsydar/tagona/storage/pkg/client`. This is resolved by the Go workspace, **not** by listing `storage` in `tagger/go.mod`. Building from a module directory works because Go automatically resolves sibling workspace modules.

## Workflow

1. **Create a branch** off the latest `main`:
   ```bash
   git checkout -b feature/your-feature-name
   ```
2. **Make your changes.** Follow existing code style.
3. **Add tests.** If you add code, consider adding unit tests in `*_test.go` files.
4. **Run checks locally:**
   ```bash
   make all                 # ensure everything compiles
   cd storage && go vet ./...
   cd tagger && go vet ./...
   cd e2e && GOWORK=off go test -v -count=1 .
   ```
5. **Commit** using clear messages. We prefer [Conventional Commits](https://www.conventionalcommits.org/):
   ```
   feat: add pagination cursor to tag queries
   fix(storage): handle nil tag map in query builder
   docs: update README with new env vars
   ```
6. **Push** and open a **Pull Request** against `main`.
7. **CI** will run automatically. Ensure it is green before requesting review.

## Pull Request Guidelines

*   Keep changes focused — one logical change per PR.
*   Explain the "why" in the PR description, not just the "what".
*   Update documentation (`README.md`, `AGENTS.md`, module READMEs) if behavior changes.
*   Be kind and constructive in review discussions.

## Code Style

*   Standard Go formatting (`gofmt`).
*   `go vet ./...` should pass without warnings in each module.
*   Keep exported APIs minimal and well-documented.

## Reporting Bugs

Use the [GitHub issue tracker](https://github.com/MrSydar/tagona/issues) and choose the "Bug report" template.

## Questions?

Feel free to open a GitHub Discussion or an issue with the "Question" label.
