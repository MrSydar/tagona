# syntax=docker/dockerfile:1

# The build stage always runs on the builder's own architecture and cross-compiles
# (instead of running the Go toolchain under emulation for arm64 images).
FROM --platform=$BUILDPLATFORM golang:1.26.4 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.work ./
COPY api/go.mod api/go.sum api/
COPY tagger/go.mod tagger/go.sum tagger/
COPY storage/go.mod storage/go.sum storage/
WORKDIR /app/tagger
RUN go mod download
WORKDIR /app
COPY api ./api
COPY tagger ./tagger
COPY storage ./storage
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /bin/tagger ./tagger/cmd/tagger

FROM debian:12-slim
# The source label links the published GHCR package to this repository.
LABEL org.opencontainers.image.title="tagona-tagger" \
      org.opencontainers.image.description="Tagona tag evaluation service" \
      org.opencontainers.image.source="https://github.com/MrSydar/tagona" \
      org.opencontainers.image.licenses="Apache-2.0"
# curl is used by the compose healthchecks. The service runs as an unprivileged
# user (numeric, so orchestrators can verify it is not root).
RUN apt-get update && apt-get install -y ca-certificates curl && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin tagona
COPY --from=builder /bin/tagger /usr/local/bin/tagger
WORKDIR /app
USER 10001:10001
EXPOSE 8081
CMD ["/usr/local/bin/tagger"]
