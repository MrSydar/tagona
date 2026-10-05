# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial open-source release preparation (LICENSE, CONTRIBUTING.md, CI, issue templates).
- API gateway hardening: cached key validation, request body limit (`API_MAX_BODY_BYTES`).
- `GET /v1/collections/{collection}/tags`: object count of a collection plus every registered tag with true/false/unknown object counts (prefix filter, keyset pagination). Backed by trigger-maintained counters (migration `000003`) and available in the Go client and CLI (`collection-tags`).
- `evaluate` option (default `true`) for tag queries (request body field) and the object tags endpoint (query parameter). With `evaluate=false` the tagging engine is never called: queries return only objects whose requested tags are already known and matching, and the tags endpoint returns `null` for tags not yet evaluated. Available in the Go client (`TagsQueryRequest.Evaluate`, `GetObjectTagsWithOptions`) and CLI (`--evaluate=false`).

- API documentation: the OpenAPI 3 contract of `/v1` is served at `GET /v1/openapi.json` and `/v1/openapi.yaml`, with interactive Swagger UI at `GET /v1/docs` (all public). Tests keep the spec in sync with the routes and CI lints it.

- Release pipeline (`.github/workflows/release.yml`): pushing a `vX.Y.Z` tag builds the api, storage and tagger images for amd64 and arm64, runs the end-to-end tests against the pushed images, and (after an approval on the `release` environment) publishes them to GHCR as `X.Y.Z`, `X.Y`, `X` and `latest`, signed with cosign and with provenance and an SBOM, and creates the GitHub Release. See `RELEASING.md`.
- CI jobs `docker-build` (builds the three images and checks they run as non-root) and `release-scripts` (tests the release scripts).

### Fixed

- Paging a tag query that has both a `cursor` and a `date` filter failed with a SQL placeholder error; the cursor condition in `ScanCandidateObjects`, `QueryObjectsKnownTags` and `queryObjectsByDate` now numbers its arguments correctly (and the by-date query no longer has an ambiguous `id` column).

### Changed

- The api, storage and tagger Docker images now run as an unprivileged user (uid 10001), carry OCI labels, and cross-compile the Go build natively instead of under emulation. The compose services are named `ghcr.io/mrsydar/tagona-<service>:${TAGONA_VERSION:-dev}`; a plain `docker compose up --build` still builds from source and tags them `:dev`.
- OpenAPI: documented `ttl_seconds` (omit it for the server default; sending it always enables expiry, so `0` expires the object immediately), how expired objects behave, and the retention sweeper.
- Retention sweeper: each run now drains the whole backlog of expired objects (it removed at most 100 per run, about 100 per minute by default, so it could never catch up with a large burst of expiries). The batch size is configurable with `TAGONA_RETENTION_BATCH_SIZE` (default `100`). The S3 payload is now deleted before the database row, so a failed S3 delete is retried on the next run instead of leaving an orphaned payload, and each run logs a single summary line instead of one line per object.
- The api service proxies an explicit allowlist of storage routes instead of every `/v1/*` path, rejects unsafe paths, and no longer forwards `Authorization`/`X-Forwarded-*` headers to storage.

### Removed

- The Traefik edge proxy (`traefik/`) and the per-API-key rate limiting in the api service (`API_RATE_LIMIT_RPS`, `API_RATE_LIMIT_BURST`, `429 rate_limited`). The api container publishes `:8080` on the host again. TLS and rate limiting are expected to come from a proxy in front of the api. OpenAPI `info.version` is now 1.2.0.
