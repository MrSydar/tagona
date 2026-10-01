# Traefik edge proxy

Public entrypoint for the stack (`traefik:v3.6`, file provider; the Docker socket is not mounted). It sits in front of the api service, which is not published on the host.

| Port | Protocol | Notes |
|------|----------|-------|
| `:8080` | HTTP | Same routes as `:8443`, no TLS. Keep it on loopback/internal networks in production, since API keys travel in cleartext |
| `:8443` | HTTPS (HTTP/2) | Traefik's built-in self-signed cert unless you configure one (clients need `curl -k` locally) |

Routed to api: `/v1/*`, `/healthz`, `/readyz`. `/metrics` is not routed; Prometheus scrapes `api:8080` on the compose network.

## Responsibilities

| Here (edge) | In the api service |
|-------------|--------------------|
| TLS termination | API key / admin auth |
| Per-IP rate limit | Per-key rate limit |
| Read/write/idle timeouts | Route allowlist, path sanitization |
| | Request body limit, header scrubbing |

## Configuration

- `traefik.yml` — static config: entrypoints, timeouts, healthcheck ping, file provider.
- `dynamic.yml` — routers, services and the `rate-limit` middleware. It is a Go template: values like `{{ env "..." }}` come from the container environment, so avoid `{{` anywhere else in the file, comments included.

| Env var (compose) | Default | Description |
|-------------------|---------|-------------|
| `EDGE_RATE_LIMIT_AVERAGE` | `200` | Requests per second allowed per client IP |
| `EDGE_RATE_LIMIT_BURST` | `400` | Burst size per client IP |

No `forwardedHeaders.trustedIPs` is set, so Traefik treats every client as untrusted and overwrites `X-Forwarded-*` — correct while Traefik is the outermost proxy. If you put a load balancer in front of it, add that balancer's address range to `trustedIPs`, otherwise the per-IP limit sees only the balancer's IP.

## Real certificates

Mount a cert/key pair and add to `dynamic.yml`:

```yaml
tls:
  certificates:
    - certFile: /certs/tagona.crt
      keyFile: /certs/tagona.key
```

ACME (Let's Encrypt) is not configured: its HTTP-01/TLS-ALPN challenges need public `:80`/`:443`, which these ports don't provide without remapping.

## Editing config

Single-file bind mounts follow the file's inode. If an editor replaces the file (e.g. `sed -i`), recreate the container: `docker compose up -d --force-recreate traefik`.
