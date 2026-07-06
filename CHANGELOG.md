# Changelog

All notable changes to Bunker will be documented in this file.

## [0.1.0] — 2026-07-07

### Added
- `bunkerd` daemon with connect-go gRPC+REST API
- `bunker` CLI (cobra): connect, spawn, list, exec, destroy, metrics, mount, tunnel, info, env
- Agent isolation via per-user Linux accounts with rootless Docker
- Resource limits: CPU, memory, disk, processes, open files enforced via cgroups (systemd user slices)
- JWT auth with master tokens and agent-scoped sub-keys
- mTLS support via certmagic (self-signed, Let's Encrypt, or mutual TLS)
- Cloudflare TryCloudflare tunnels for per-agent public URLs
- Tailscale integration for agent networking
- SSHFS mount support for local filesystem access
- Docker socket tunneling via SSH
- Multi-server CLI support
- TTL-based agent expiry with automatic reaping
- systemd service installation for bunkerd
- GitReins Tier 1 + Tier 2 quality gates
- Hilo dependency graph and blast radius analysis
- Per-package SKILL.md files for AI agent documentation
- README with hero image, architecture diagram, and full CLI reference
- Apache 2.0 license with individual CLA
- CI pipeline: build, vet, test, GitReins guard

[0.1.0]: https://github.com/deployBunker/bunker/releases/tag/v0.1.0
