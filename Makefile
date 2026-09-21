SHELL := /bin/bash
.DEFAULT_GOAL := help

LDFLAGS = -s -w

# go.mod pins the minimum patched toolchain. Some official Go and development
# images export GOTOOLCHAIN=local, which turns a stale patch release into a hard
# failure instead of letting Go fetch the required toolchain. A command-line
# override (for example `make test GOTOOLCHAIN=local`) still takes precedence.
GOTOOLCHAIN = auto
export GOTOOLCHAIN

CMDS = agent

.PHONY: help build clean test race cover vet lint fmt

help:
	@echo "agent - an automated software factory in a single binary"
	@echo
	@echo "  make build      Build agent for Linux"
	@echo "  make test       Run the test suite"
	@echo "  make race       Run the test suite under the race detector"
	@echo "  make cover      Report per-package test coverage"
	@echo "  make vet        Run go vet, and refuse code go fix would modernise"
	@echo "  make fmt        Format the tree"
	@echo "  make lint       Alias for vet"
	@echo "  make clean      Remove built binaries"

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

# go fix carries the modernizers: a loop, a min/max or a helper that the
# standard library now spells is reported here rather than left to review.
vet:
	go vet ./...
	@out="$$(go fix -diff ./... 2>&1)"; if [ -n "$$out" ]; then echo "$$out"; echo "go fix would change the tree: run 'go fix ./...'"; exit 1; fi

lint: vet
	@echo "lint ok"

clean:
	rm -f $(CMDS)
