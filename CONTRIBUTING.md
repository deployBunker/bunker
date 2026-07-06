# Contributing to Bunker

Thanks for your interest in contributing! Bunker is a multi-agent coding platform that gives each AI agent its own isolated Linux environment with rootless Docker.

## Before you start

1. **Read the CLA.** All contributors must sign the [Individual Contributor License Agreement](CLA.md). Our CLA bot checks PRs automatically.
2. **Check existing work.** Browse [open issues](https://github.com/deployBunker/bunker/issues) and the [task board](.coding-hermes/tasks.md) to avoid duplication.

## Development environment

```bash
# Prerequisites: Go 1.24+, Docker, systemd (Linux)
git clone https://github.com/deployBunker/bunker.git
cd bunker
go build ./...
go test ./... -short
```

For agent lifecycle tests (spawn/destroy/exec), you need root:
```bash
sudo go test ./internal/agent/... -count=1
```

## Quality gates

Every commit must pass:

| Gate | Command |
|------|---------|
| Build | `go build ./...` |
| Vet | `go vet ./...` |
| Tests | `go test ./... -short -count=1 -timeout 120s` |
| Format | `gofmt -w .` |
| GitReins | `gitreins guard run` |
| Hilo | `hilo graph impact <changed-file>` |

## Pull request process

1. Fork the repo and create a feature branch
2. Make your changes with tests
3. Run the quality gates above
4. Submit a PR — the CLA bot will prompt you to sign if you haven't
5. CI runs build + vet + test + GitReins Tier 1 automatically
6. A maintainer will review within 48 hours

## Code conventions

- Table-driven tests preferred
- Use `internal/` packages; avoid `pkg/` unless exporting for other repos
- CLI commands in `internal/cli/`, one file per command
- Use `connectrpc.com/connect` error codes
- Never commit secrets — use test fixtures and `.gitleaks.toml` allowlists
- Update the package's `SKILL.md` when changing public API

## Project layout

```
cmd/bunker          — CLI entrypoint
cmd/bunkerd         — daemon entrypoint
internal/agent      — spawn/destroy lifecycle, cgroups, rootless Docker
internal/auth       — JWT + mTLS auth
internal/cli        — cobra CLI commands
internal/config     — YAML configuration
internal/server     — connect-go service handlers
internal/systemd    — systemd service helpers
internal/tunnel     — Cloudflare tunnel manager
proto/bunker/v1     — Protobuf + connect-go generated code
```

## Getting help

- [Open an issue](https://github.com/deployBunker/bunker/issues) for bugs or feature requests
- Tag `@Bane` for architectural questions
