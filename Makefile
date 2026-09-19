SHELL := /bin/bash
.DEFAULT_GOAL := help

LDFLAGS = -s -w

# go.mod pins the minimum patched toolchain. Some official Go and development
# images export GOTOOLCHAIN=local, which turns a stale patch release into a hard
# failure instead of letting Go fetch the required toolchain. A command-line
# override (for example `make test GOTOOLCHAIN=local`) still takes precedence.
GOTOOLCHAIN = auto
export GOTOOLCHAIN

CMDS = zot

# Cross-compilation defaults to the host, so `make cross` with no arguments
# builds something predictable rather than whatever was last exported.
GOOS   ?= $(shell go env GOHOSTOS)
GOARCH ?= $(shell go env GOHOSTARCH)

.PHONY: help build clean test race cover vet lint fmt cross

help:
	@echo "zot - an automated software factory in a single binary"
	@echo
	@echo "  make build      Build zot ($(GOOS)/$(GOARCH))"
	@echo "  make test       Run the test suite"
	@echo "  make race       Run the test suite under the race detector"
	@echo "  make cover      Report per-package test coverage"
	@echo "  make vet        Run go vet"
	@echo "  make fmt        Format the tree"
	@echo "  make lint       Alias for vet"
	@echo "  make cross      Cross-compile: make cross GOOS=darwin GOARCH=arm64"
	@echo "  make clean      Remove built binaries"
	@echo
	@echo "Overrides: GOOS=$(GOOS) GOARCH=$(GOARCH)"

build:
	@set -e; for cmd in $(CMDS); do \
		echo "Building $$cmd..."; \
		CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $$cmd ./cmd/$$cmd; \
	done

fmt:
	go fmt ./...

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

cover:
	@go test -cover ./... -count=1 | grep coverage | sed 's|github.com/openzot/openzot||'

vet:
	go vet ./...

lint: vet
	@echo "lint ok"

clean:
	rm -f $(CMDS)

# Cross-compile a specific platform: make cross GOOS=darwin GOARCH=arm64
cross:
	@set -e; for cmd in $(CMDS); do \
		echo "Building $$cmd for $(GOOS)/$(GOARCH)..."; \
		CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $$cmd ./cmd/$$cmd; \
	done
