# syntax=docker/dockerfile:1

# The build stage always runs on the builder's own architecture and cross-compiles
# (instead of running the Go toolchain under emulation for arm64 images).
FROM --platform=$BUILDPLATFORM golang:1.26.4 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.work ./
COPY api/go.mod api/go.sum api/
COPY storage/go.mod storage/go.sum storage/
COPY tagger/go.mod tagger/go.sum tagger/
WORKDIR /app/api
RUN go mod download
WORKDIR /app
COPY api ./api
COPY storage ./storage
COPY tagger ./tagger
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /bin/api ./api/cmd/api

FROM debian:12-slim
# The source label links the published GHCR package to this repository.
LABEL org.opencontainers.image.title="tagona-api" \
      org.opencontainers.image.description="Tagona API gateway" \
      org.opencontainers.image.source="https://github.com/MrSydar/tagona" \
      org.opencontainers.image.licenses="Apache-2.0"
# curl is used by the compose healthchecks. The service runs as an unprivileged
# user (numeric, so orchestrators can verify it is not root).
RUN apt-get update && apt-get install -y ca-certificates curl && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin tagona
COPY --from=builder /bin/api /usr/local/bin/api
WORKDIR /app
USER 10001:10001
EXPOSE 8080
CMD ["/usr/local/bin/api"]
