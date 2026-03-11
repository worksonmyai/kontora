.PHONY: all build test test-race lint fmt install clean

all: build

build:
	go build -o kontora ./cmd/kontora

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
