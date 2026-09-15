.PHONY: build test dev

PLUGINS := github

build:
	@mkdir -p bin
	@for p in $(PLUGINS); do go build -o bin/$$p ./$$p || exit 1; done

test:
	go test -race ./...

# Play every plugin's scenarios against an in-memory core.
dev: build
	@for p in $(PLUGINS); do for s in $$p/scenarios/*.json; do \
		echo "== $$s"; go run github.com/onegator/gator/cmd/gator-plugin dev $$s -- ./bin/$$p || exit 1; done; done
