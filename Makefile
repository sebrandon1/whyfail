VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOBIN := $(shell go env GOPATH)/bin

.PHONY: build test test-live fixtures lint install clean plugin

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/whyfail ./cmd/whyfail

test:
	go test ./...

# Hits the real Prow/GCS bucket; not part of `make test`.
test-live:
	go test -tags live -count=1 -timeout 20m ./internal/run ./internal/health ./cmd/whyfail

# Re-record testdata/ (recert #2364 and cert-manager #533) from live Prow (network).
fixtures:
	./scripts/fetch-fixtures.sh

lint:
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; ran go vet only"; fi

install: build
	install -m 0755 bin/whyfail $(GOBIN)/whyfail

plugin:
	@echo "Install the plugin from ./plugin with your Claude Code plugin manager."

clean:
	rm -rf bin
