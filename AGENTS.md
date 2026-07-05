# AGENTS.md — Bunker

Guidelines for AI agents working on this repo.

## Build & test

- `go build ./...` must pass.
- `go test ./...` must pass.
- `go vet ./...` must pass.
- `gofmt -w` must be run on any new Go file.

## Quality gates

1. Run `hilo graph impact <file>` before changing any file.
2. Run `gitreins guard` before every commit (Tier 1: secrets, build, lint, tests).
3. For tasks that change agent spawn, destroy, exec, docker, or SSH behavior, run the live-server E2E battery on `bunker-mvp` (`78.46.173.180`) and confirm `VERIFY-PASS`.
4. Update `.gitreins/tasks.yaml` and `.coding-hermes/tasks.md` when completing work items.

## Code conventions

- Prefer table-driven tests.
- Use `internal/` packages; avoid `pkg/` unless exporting for other repos.
- Keep CLI command implementations in `internal/cli/`, one file per command.
- Use `connectrpc.com/connect` error codes (`CodeUnauthenticated`, `CodeNotFound`, etc.).
- Do not commit real secrets, tokens, or keys; use test fixtures and `.gitleaks.toml` allowlists.

## E2E verification (server-side)

Build binaries locally, push via git, pull on `bunker-mvp`, restart `bunkerd`, and run `bash e2e-full-battery.sh`.

Do not mark tasks complete unless the E2E battery outputs `VERIFY-PASS`.
