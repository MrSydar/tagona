.PHONY: all build-api build-client build-storage build-tagger e2e docker-up docker-down help

all: build-api build-client build-storage build-tagger

## Build

build-api:
	cd api && go build -o ../bin/api ./cmd/api

build-client:
	cd storage && go build -o ../bin/tagona-client ./cmd/client

build-storage:
	cd storage && go build -o ../bin/storage ./cmd/storage

build-tagger:
	cd tagger && go build -o ../bin/tagger ./cmd/tagger

## Docker

docker-up:
	docker compose up --build -d

docker-down:
	docker compose down

## Tests

e2e:
	cd e2e && GOWORK=off go test -v -count=1 .

help:
	@echo "Available targets:"
	@echo "  build-api      Build the api service binary (bin/api)"
	@echo "  build-client   Build the storage CLI client (bin/tagona-client)"
	@echo "  build-storage  Build the storage service binary (bin/storage)"
	@echo "  build-tagger   Build the tagger service binary (bin/tagger)"
	@echo "  all            Build api, client, storage, and tagger binaries"
	@echo "  docker-up      Start the full Docker Compose stack"
	@echo "  docker-down    Stop the Docker Compose stack"
	@echo "  e2e            Run end-to-end tests (requires stack running)"
