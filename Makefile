.PHONY: all build test test-race test-scripts lint fmt install clean assets css model-prices

all: build

build:
	go build -o kontora ./cmd/kontora

# Re-download all vendored web assets and rebuild app.css. Run after bumping a
# version in hack/vendor-assets.sh. Not a build dependency: the outputs are
# committed and embedded, so plain `go build`/`go install` stays offline.
assets:
	./hack/vendor-assets.sh

# Rebuild only internal/web/static/app.css. Run after changing Tailwind classes
# in static/index.html or any module under internal/web/ui/.
css:
	./hack/build-css.sh

test:
	go test -timeout 5m ./...

test-race:
	go test -race -timeout 5m ./...

test-scripts:
	./hack/changelog-for-release.test.sh
	./hack/update-model-prices.test.sh

# Refresh the committed catalog explicitly; normal builds and tests only read
# its embedded snapshot and do not contact OpenRouter.
model-prices:
	./hack/update-model-prices.py --output internal/pricing/catalog.json

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
