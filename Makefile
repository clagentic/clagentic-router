## Makefile — clagentic-router build targets
BINARY     := clagentic-router
CMD        := ./cmd/clagentic-router
BUILD_DIR  := ./bin
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -ldflags "-X main.version=$(VERSION)"

# When GOPATH is unset, default to $(HOME)/go so the Go toolchain can find
# the module cache without requiring manual env setup.
ifeq ($(GOPATH),)
export GOPATH      := $(HOME)/go
export GOMODCACHE  := $(HOME)/go/pkg/mod
export GOCACHE     := $(HOME)/.cache/go
endif

.PHONY: all build test smoke verify-safe-mode lint clean install tidy docker

all: build

## build: compile the binary to bin/clagentic-router
build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) $(CMD)
	@echo "Built $(BUILD_DIR)/$(BINARY)"

## install: install to GOBIN (or ~/go/bin)
install:
	go install $(LDFLAGS) $(CMD)

## test: run all tests
test:
	go test ./...

## lint: run go vet (install golangci-lint for full checks)
lint:
	go vet ./...

## tidy: tidy go.mod and go.sum
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## smoke: end-to-end smoke test against a live daemon (requires Ollama)
smoke: build
	./scripts/smoke-test.sh

## verify-safe-mode: reproduce the --setting-sources user / permissions.allow
## evidence in README.md and claude_cli.go against a live claude CLI (also
## covers the --safe-mode cells, kept as the historical record of the flag
## that was tried first and rejected). NOT part of `make test` -- invokes a
## real CLI, costs tokens, and skips honestly if claude is
## absent/unauthenticated. See scripts/verify-safe-mode-permissions.sh.
verify-safe-mode:
	./scripts/verify-safe-mode-permissions.sh

## docker: build Docker image
docker:
	docker build -t clagentic-router:$(VERSION) -t clagentic-router:latest .
