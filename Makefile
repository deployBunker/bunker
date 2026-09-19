# Bunker — build, test, and release automation

# Default target
.PHONY: all
all: build

# Binaries
BIN_DIR := .
BUNKERD := $(BIN_DIR)/bunkerd
BUNKER := $(BIN_DIR)/bunker

# Release artifacts (DF-BUNKER-25). dist/ is gitignored; the GitHub release
# workflow publishes exactly what `make release-binaries` writes here.
DIST_DIR := dist
RELEASE_PLATFORMS := linux/amd64 linux/arm64
RELEASE_BINARIES := bunker bunkerd
# sha256sum on Linux, `shasum -a 256` elsewhere — the same choice install.sh makes.
SHA256SUM ?= $(shell if command -v sha256sum >/dev/null 2>&1; then echo sha256sum; else echo shasum -a 256; fi)

# Build metadata injected via -ldflags into internal/version.
# VERSION is overridable on the command line (make build VERSION=v0.2.0).
VERSION ?= 0.1.4
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDDATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/deployBunker/bunker/internal/version.Version=$(VERSION) \
           -X github.com/deployBunker/bunker/internal/version.Commit=$(COMMIT) \
           -X github.com/deployBunker/bunker/internal/version.BuildDate=$(BUILDDATE)

# ── Go toolchain guard (DF-BUNKER-25) ────────────────────────────────────────
# A stock Debian/Ubuntu cloud image has make and docker but no Go, so a bare
# `go build` recipe ends in `sh: 1: go: not found` (exit 127) — a message that
# tells a new user nothing. Every target that compiles depends on check-go,
# which refuses first and names the README Install section plus the exact
# commands that fix it.
GO_MIN := $(shell awk '/^go[[:space:]]/ { print $$2; exit }' go.mod 2>/dev/null)
ifeq ($(GO_MIN),)
GO_MIN := 1.26.5
endif
# Pure make (no sed/uname pipeline) so a Go-less PATH cannot emit tool-missing
# noise of its own ahead of the refusal below.
HOST_MACHINE := $(shell uname -m 2>/dev/null)
HOST_ARCH := $(if $(filter x86_64 amd64,$(HOST_MACHINE)),amd64,$(if $(filter aarch64 arm64,$(HOST_MACHINE)),arm64,))
ifeq ($(HOST_ARCH),)
HOST_ARCH := amd64
endif
GO_TARBALL := https://go.dev/dl/go$(GO_MIN).linux-$(HOST_ARCH).tar.gz

.PHONY: check-go
check-go:
	@command -v go >/dev/null 2>&1 || { \
	  echo '' >&2; \
	  echo 'make: no Go toolchain on PATH — nothing was built.' >&2; \
	  echo '' >&2; \
	  echo '  These targets compile Go source, so they need the Go toolchain. Install it,' >&2; \
	  echo '  then re-run. The Install section of README.md documents this' >&2; \
	  echo '  ("Option 2 — build from source" → "Installing Go on a stock Debian/Ubuntu host"):' >&2; \
	  echo '' >&2; \
	  echo '    curl -fsSL $(GO_TARBALL) -o /tmp/go.tar.gz' >&2; \
	  echo '    sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tar.gz' >&2; \
	  echo '    export PATH=/usr/local/go/bin:$$PATH' >&2; \
	  echo '' >&2; \
	  echo '  Extract into /usr/local/go, NOT into $$HOME: a tarball unpacked in $$HOME makes' >&2; \
	  echo '  GOPATH == GOROOT and every go command warns about it.' >&2; \
	  echo '' >&2; \
	  echo '  No toolchain wanted? scripts/install.sh installs the prebuilt release binaries:' >&2; \
	  echo '    curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh | sh' >&2; \
	  echo '' >&2; \
	  exit 1; \
	}

# Build both CLI binaries
.PHONY: build
build: check-go
	go build -ldflags "$(LDFLAGS)" -o $(BUNKERD) ./cmd/bunkerd
	go build -ldflags "$(LDFLAGS)" -o $(BUNKER) ./cmd/bunker

# Build only the daemon
.PHONY: build-daemon
build-daemon: check-go
	go build -ldflags "$(LDFLAGS)" -o $(BUNKERD) ./cmd/bunkerd

# Build only the CLI
.PHONY: build-cli
build-cli: check-go
	go build -ldflags "$(LDFLAGS)" -o $(BUNKER) ./cmd/bunker

# ── Release binaries (DF-BUNKER-25) ──────────────────────────────────────────
# The single source of truth for the prebuilt assets that scripts/install.sh
# downloads: linux/amd64 + linux/arm64 for both commands, stamped with the same
# LDFLAGS as `build`, plus a SHA256SUMS covering them and a copy of the
# installer. .github/workflows/release.yml publishes this directory and calls
# nothing else, so a local `make release-binaries` and a published release can
# not drift.
.PHONY: release-binaries
release-binaries: check-go
	@set -eu; \
	rm -rf $(DIST_DIR); mkdir -p $(DIST_DIR); \
	for platform in $(RELEASE_PLATFORMS); do \
	  os=$${platform%%/*}; arch=$${platform#*/}; \
	  for name in $(RELEASE_BINARIES); do \
	    out=$(DIST_DIR)/$$name-$$os-$$arch; \
	    echo "release-binaries: GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -o $$out ./cmd/$$name"; \
	    GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $$out ./cmd/$$name; \
	  done; \
	done; \
	for platform in $(RELEASE_PLATFORMS); do \
	  os=$${platform%%/*}; arch=$${platform#*/}; \
	  for name in $(RELEASE_BINARIES); do \
	    ( cd $(DIST_DIR) && $(SHA256SUM) "$$name-$$os-$$arch" >> SHA256SUMS ); \
	  done; \
	done; \
	cp scripts/install.sh $(DIST_DIR)/install.sh; \
	( cd $(DIST_DIR) && $(SHA256SUM) install.sh >> SHA256SUMS ); \
	echo ''; echo "release-binaries: $(DIST_DIR)/ is ready:"; ls -l $(DIST_DIR); \
	echo ''; echo "  $(DIST_DIR)/SHA256SUMS:"; cat $(DIST_DIR)/SHA256SUMS

# Run all unit tests
.PHONY: test
test:
	go test ./... -count=1

# Run short unit tests (CI friendly)
.PHONY: test-short
test-short:
	go test ./... -count=1 -short

# Shell tests: the install.sh suite (network-free, no Go needed) — the same
# command ci.yml runs.
.PHONY: test-sh
test-sh:
	sh scripts/install_test.sh

# Run go vet across the project
.PHONY: vet
vet:
	go vet ./...

# Format all Go source files
.PHONY: fmt
fmt:
	gofmt -w .

# Lint: go vet + build + format check
.PHONY: lint
lint: vet build
	@test -z "$$(gofmt -l .)" || (echo "gofmt required on:" && gofmt -l . && exit 1)

# Regenerate protobuf code
.PHONY: proto
proto:
	buf generate

# Clean built binaries and release artifacts
.PHONY: clean
clean:
	rm -f $(BUNKERD) $(BUNKER)
	rm -rf $(DIST_DIR)

# Run the live-server E2E battery (requires configured bunker-mvp host)
.PHONY: e2e
e2e:
	bash ./e2e-full-battery.sh

# Verify the documented CLI surface against the newest release tag (GAP-081):
# README commands must exist in that tag or be marked "requires a build from
# HEAD", no older release tag may be named, and the CHANGELOG must carry an
# `## Unreleased` section while commits sit after the tag. Offline; skips with a
# warning in a tag-less checkout (act/shallow/fork).
.PHONY: docs-check
docs-check:
	go run ./cmd/docs-drift

# Install binaries to /usr/local/bin (requires root; a make-free, non-root
# alternative is ./scripts/install.sh, which falls back to ~/.local/bin)
.PHONY: install
install: check-go build
	install -m 0755 $(BUNKERD) /usr/local/bin/bunkerd
	install -m 0755 $(BUNKER) /usr/local/bin/bunker

# Full CI-quality check
.PHONY: ci
ci: lint test-short docs-check
