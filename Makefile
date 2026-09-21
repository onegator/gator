LDFLAGS := -X github.com/onegator/gator/internal/version.Version=$(shell git describe --tags --always --dirty) \
           -X github.com/onegator/gator/internal/version.Commit=$(shell git rev-parse --short HEAD) \
           -X github.com/onegator/gator/internal/version.Date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

.PHONY: build plugins plugins-dev test lint sqlc openapi generate migrate db-up db-down run-server run-runner

build:
	go build -ldflags "$(LDFLAGS)" -o bin/gator-server ./cmd/gator-server
	go build -ldflags "$(LDFLAGS)" -o bin/gator-runner ./cmd/gator-runner

# Plugins live next to the SDK they are written against, so a protocol change and the plugins
# that use it land in one commit. Each directory under plugins/ is one plugin program.
PLUGINS := $(notdir $(patsubst %/,%,$(dir $(wildcard plugins/*/main.go))))

plugins:
	@mkdir -p bin/plugins
	@for p in $(PLUGINS); do go build -o bin/plugins/$$p ./plugins/$$p || exit 1; done

# Play every plugin's scenarios against an in-memory core.
plugins-dev: plugins
	@for p in $(PLUGINS); do for s in plugins/$$p/scenarios/*.json; do \
		echo "== $$s"; go run ./cmd/gator-plugin dev $$s -- ./bin/plugins/$$p || exit 1; done; done

test:
	go test ./...

lint:
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

openapi:
	cd internal/server/api/gen && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.5.0 -config config.yaml ../../../../docs/openapi.yaml

generate: sqlc openapi

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
