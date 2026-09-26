# syntax=docker/dockerfile:1

FROM golang:1.26.4 AS builder
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
RUN CGO_ENABLED=0 go build -o /bin/tagger ./tagger/cmd/tagger

FROM debian:12-slim
RUN apt-get update && apt-get install -y ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/tagger /usr/local/bin/tagger
WORKDIR /app
EXPOSE 8081
CMD ["/usr/local/bin/tagger"]
