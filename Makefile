VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test install release-local clean

build:
	go build -ldflags "$(LDFLAGS)" -o mplex ./cmd/mplex

test:
	go vet ./...
	go test -race ./...

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/mplex

release-local:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/mplex-darwin-arm64  ./cmd/mplex
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/mplex-darwin-amd64  ./cmd/mplex
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/mplex-linux-amd64   ./cmd/mplex
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/mplex-linux-arm64   ./cmd/mplex

clean:
	rm -rf mplex dist
