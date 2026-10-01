# syntax=docker/dockerfile:1

FROM golang:1.26.4 AS builder
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
RUN CGO_ENABLED=0 go build -o /bin/api ./api/cmd/api

FROM debian:12-slim
RUN apt-get update && apt-get install -y ca-certificates curl && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/api /usr/local/bin/api
WORKDIR /app
EXPOSE 8080
CMD ["/usr/local/bin/api"]
