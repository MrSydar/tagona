# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial open-source release preparation (LICENSE, CONTRIBUTING.md, CI, issue templates).
- Traefik edge proxy in front of the api service: plain HTTP on `:8080`, TLS on `:8443`, per-IP rate limiting and timeouts.
- API gateway hardening: per-API-key rate limiting, cached key validation, request body limit (`API_MAX_BODY_BYTES`).
- `GET /v1/collections/{collection}/tags`: object count of a collection plus every registered tag with true/false/unknown object counts (prefix filter, keyset pagination). Backed by trigger-maintained counters (migration `000003`) and available in the Go client and CLI (`collection-tags`).
- `evaluate` option (default `true`) for tag queries (request body field) and the object tags endpoint (query parameter). With `evaluate=false` the tagging engine is never called: queries return only objects whose requested tags are already known and matching, and the tags endpoint returns `null` for tags not yet evaluated. Available in the Go client (`TagsQueryRequest.Evaluate`, `GetObjectTagsWithOptions`) and CLI (`--evaluate=false`).

- API documentation: the OpenAPI 3 contract of `/v1` is served at `GET /v1/openapi.json` and `/v1/openapi.yaml`, with interactive Swagger UI at `GET /v1/docs` (all public). Tests keep the spec in sync with the routes and CI lints it.

### Fixed

- Paging a tag query that has both a `cursor` and a `date` filter failed with a SQL placeholder error; the cursor condition in `ScanCandidateObjects`, `QueryObjectsKnownTags` and `queryObjectsByDate` now numbers its arguments correctly (and the by-date query no longer has an ambiguous `id` column).

### Changed

- OpenAPI: documented `ttl_seconds` (omit it for the server default; sending it always enables expiry, so `0` expires the object immediately), how expired objects behave, and the retention sweeper.
- The api service proxies an explicit allowlist of storage routes instead of every `/v1/*` path, rejects unsafe paths, and no longer forwards `Authorization`/`X-Forwarded-*` headers to storage.
- The api container no longer publishes a host port; use Traefik on `:8080`/`:8443`. `/metrics` is no longer reachable through the public port.
