# Bunker

Multi-agent coding platform daemon/CLI. Spins up isolated Linux user environments with rootless Docker per agent, controlled through gRPC+REST or a single CLI.

## What it does

- `bunkerd` runs on a Linux host and exposes gRPC + REST via connect-go.
- `bunker` is the CLI that registers one or more servers and manages agents.
- Each agent is an isolated Linux user with:
  - Its own rootless Docker daemon
  - Dedicated SSH keypair and port range
  - Resource limits (CPU, memory, disk, processes, open files, max containers)
  - An ENFORCED private `/tmp` (pam_namespace per AGENT SSH session, scoped by
    the reserved `bunker-*` NAME pattern and then verified against the
    `bunker-agents` group, so ordinary operator sessions keep the host /tmp and
    a broken boundary denies instead of sharing; `PrivateTmp=yes` per transient
    unit) and one bounded cross-agent exchange directory (`/srv/bunker-share`);
    install the host half with `bunker host-provision` — see
    specs/agent-tmp-isolation.md
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

- `gitreins guard` — secrets, build, lint, tests. It has TWO verdicts (exit 0 / 1).
- `bash scripts/gitreins-guard.sh` (or `make guard`) — the same guard, **three** outcomes:
  PASS (0) / TEST-FAILURE (1) / NOT-FINISHED (3) / GUARD-ERROR (4). A budget exhaustion under
  load is reported as NOT-FINISHED — exit 3, which still refuses the commit, but says that no
  test verdict was reached instead of blaming the code. Nothing is retried.
- `make hooks` — point the pre-commit hook at that gate.
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
- `internal/hostsetup` — host provisioning for the isolation boundary (GAP-075): private /tmp, bounded shared scratch, host /tmp tmpfs cap
- `internal/tunnel` — Cloudflare tunnel manager
- `proto/bunker/v1` — Protobuf + connect-go generated code

## Tech stack

Go 1.24+, connect-go, chi, cobra, viper, golang-jwt, certmagic, rootless Docker.

## License

Apache 2.0 — see [LICENSE](LICENSE).
