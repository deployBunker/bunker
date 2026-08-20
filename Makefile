# Bunker — build, test, and release automation

# Default target
.PHONY: all
all: build

# Binaries
BIN_DIR := .
BUNKERD := $(BIN_DIR)/bunkerd
BUNKER := $(BIN_DIR)/bunker

# Build metadata injected via -ldflags into internal/version.
# VERSION is overridable on the command line (make build VERSION=v0.2.0).
VERSION ?= 0.1.3
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILDDATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/deployBunker/bunker/internal/version.Version=$(VERSION) \
           -X github.com/deployBunker/bunker/internal/version.Commit=$(COMMIT) \
           -X github.com/deployBunker/bunker/internal/version.BuildDate=$(BUILDDATE)

# Build both CLI binaries
.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o $(BUNKERD) ./cmd/bunkerd
	go build -ldflags "$(LDFLAGS)" -o $(BUNKER) ./cmd/bunker

# Build only the daemon
.PHONY: build-daemon
build-daemon:
	go build -ldflags "$(LDFLAGS)" -o $(BUNKERD) ./cmd/bunkerd

# Build only the CLI
.PHONY: build-cli
build-cli:
	go build -ldflags "$(LDFLAGS)" -o $(BUNKER) ./cmd/bunker

# Run all unit tests
.PHONY: test
test:
	go test ./... -count=1

# Run short unit tests (CI friendly)
.PHONY: test-short
test-short:
	go test ./... -count=1 -short

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

# Clean built binaries
.PHONY: clean
clean:
	rm -f $(BUNKERD) $(BUNKER)

# Run the live-server E2E battery (requires configured bunker-mvp host)
.PHONY: e2e
e2e:
	bash ./e2e-full-battery.sh

# Install binaries to /usr/local/bin (requires root)
.PHONY: install
install: build
	install -m 0755 $(BUNKERD) /usr/local/bin/bunkerd
	install -m 0755 $(BUNKER) /usr/local/bin/bunker

# Full CI-quality check
.PHONY: ci
ci: lint test-short
