LDFLAGS := -X github.com/onegator/gator/internal/version.Version=$(shell git describe --tags --always --dirty) \
           -X github.com/onegator/gator/internal/version.Commit=$(shell git rev-parse --short HEAD) \
           -X github.com/onegator/gator/internal/version.Date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: build test lint sqlc migrate db-up db-down run-server run-runner

build:
	go build -ldflags "$(LDFLAGS)" -o bin/gator-server ./cmd/gator-server
	go build -ldflags "$(LDFLAGS)" -o bin/gator-runner ./cmd/gator-runner

test:
	go test ./...

lint:
	go vet ./...

sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate

migrate:
	go run ./cmd/gator-server migrate

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

run-server:
	go run ./cmd/gator-server serve

run-runner:
	go run ./cmd/gator-runner run
