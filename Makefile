.PHONY: all build test test-race lint fmt install clean assets css

all: build

build:
	go build -o kontora ./cmd/kontora

# Re-download all vendored web assets and rebuild app.css. Run after bumping a
# version in hack/vendor-assets.sh. Not a build dependency: the outputs are
# committed and embedded, so plain `go build`/`go install` stays offline.
assets:
	./hack/vendor-assets.sh

# Rebuild only internal/web/static/app.css. Run after changing Tailwind classes
# in static/index.html or static/app.js.
css:
	./hack/build-css.sh

test:
	go test -timeout 5m ./...

test-race:
	go test -race -timeout 5m ./...

lint:
	golangci-lint run
	go mod tidy -diff
	go tool govulncheck ./...
	go tool deadcode -test ./...

fmt:
	golangci-lint fmt

install:
	go install ./cmd/kontora

clean:
	rm -f kontora
	go clean ./...
