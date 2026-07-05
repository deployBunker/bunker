# Bunker

Multi-agent coding platform daemon/CLI. Spins up isolated Linux user environments with rootless Docker per agent, controlled through gRPC+REST or a single CLI.

## What it does

- `bunkerd` runs on a Linux host and exposes gRPC + REST via connect-go.
- `bunker` is the CLI that registers one or more servers and manages agents.
- Each agent is an isolated Linux user with:
  - Its own rootless Docker daemon
  - Dedicated SSH keypair and port range
  - Resource limits (CPU, memory, disk, processes, open files, max containers)
  - Optional public networking via Cloudflare tunnels or Tailscale

## Quick start

```bash
go build -o bunkerd ./cmd/bunkerd
go build -o bunker ./cmd/bunker
./bunkerd --config /etc/bunkerd/config.yaml
```

```bash
bunker connect http://localhost:8080 --token <master-token>
bunker spawn --cpu 2.0 --memory 4294967296 --ttl 6h
bunker exec <agent-id> -- docker run --rm alpine echo hello
bunker destroy <agent-id>
```

## Build & test

```bash
go build ./...
go test ./...
bash e2e-full-battery.sh
```

## Quality gates

- `gitreins guard` — secrets, build, lint, tests
- `hilo graph impact <file>` — blast radius before changes
- Server-side E2E battery on `bunker-mvp` for agent/docker lifecycle changes

## Project layout

- `cmd/bunker` — CLI entrypoint
- `cmd/bunkerd` — daemon entrypoint
- `internal/agent` — spawn/destroy lifecycle, cgroups, rootless Docker
- `internal/auth` — JWT + mTLS auth
- `internal/cli` — cobra CLI commands
- `internal/config` — YAML configuration
- `internal/server` — connect-go service handlers
- `internal/systemd` — systemd service installation helpers
- `internal/tunnel` — Cloudflare tunnel manager
- `proto/bunker/v1` — Protobuf + connect-go generated code

## Tech stack

Go 1.24+, connect-go, chi, cobra, viper, golang-jwt, certmagic, rootless Docker.

## License

Apache 2.0 — see [LICENSE](LICENSE).
