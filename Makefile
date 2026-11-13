# Orion — Makefile
#
#   make build     build orion + oriond into ./bin (with version ldflags)
#   make install   build and install to /usr/local/bin or ~/.local/bin
#                  (same rules as install.sh; prefer ./install.sh for
#                  the full flow including default config + --systemd)
#   make test      run the full test suite
#   make vet       go vet ./...
#   make fmt       gofmt -l (fails if any file needs formatting)
#   make version   print the version the next build will report
#   make clean     remove ./bin
#
# Build info (commit + date) is baked in with -ldflags -X so
# `orion version` reports the real build.

GO ?= go
# Fall back to the per-user Go installs this VM/container setup is
# known to use when `go` is not on PATH (/usr/local/go does not
# survive restores here, so it is only a last resort).
ifeq ($(shell command -v $(GO) 2>/dev/null),)
GO := $(HOME)/go1.27.4/bin/go
endif
ifeq ($(shell command -v $(GO) 2>/dev/null),)
GO := /usr/local/go/bin/go
endif
VERSION_PKG := github.com/irfan/orion/internal/version
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).BuildDate=$(DATE)

BIN_DIR ?= bin

.PHONY: build install test vet fmt version clean

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orion ./cmd/orion
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/oriond ./cmd/oriond
	# The seccomp launcher MUST be built by the normal build: with
	# the sandbox ON by default, a missing orion-seccomp makes
	# every shell_exec fail closed (F-A3-1), so a build that
	# forgets the launcher ships a broken agent. CGO_ENABLED=0
	# keeps it static, so it runs inside the chroot (see
	# internal/sandbox/seccomp.go).
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/orion-seccomp ./internal/sandbox/cmd/launcher

install:
	./install.sh

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l cmd internal)" || { echo "gofmt -l:"; gofmt -l cmd internal; exit 1; }

version:
	@echo "orion $(shell grep -E '^const Version' internal/version/version.go | sed -E 's/.*"([^"]+)".*/\1/') commit $(COMMIT) built $(DATE)"

clean:
	rm -rf $(BIN_DIR)
