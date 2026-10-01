# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial open-source release preparation (LICENSE, CONTRIBUTING.md, CI, issue templates).
- Traefik edge proxy in front of the api service: plain HTTP on `:8080`, TLS on `:8443`, per-IP rate limiting and timeouts.
- API gateway hardening: per-API-key rate limiting, cached key validation, request body limit (`API_MAX_BODY_BYTES`).

### Changed

- The api service proxies an explicit allowlist of storage routes instead of every `/v1/*` path, rejects unsafe paths, and no longer forwards `Authorization`/`X-Forwarded-*` headers to storage.
- The api container no longer publishes a host port; use Traefik on `:8080`/`:8443`. `/metrics` is no longer reachable through the public port.
