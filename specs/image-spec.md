# Image Specification — package-add image customization (GAP-064, TOOLS-B2)

**Status:** implemented (package-add surface GAP-064; delivered tool set TOOLS-B2) ·
**Date:** 2026-10-09 · **Author:** Hermes
**Source of truth:** `internal/imagespec` (the parser/registry/builder are
authoritative; this document describes them, it does not define them).
**Related:** [architecture.md](architecture.md) (component map),
[configuration.md](configuration.md) (`agent.image_spec.*` keys),
[agent-lifecycle.md](agent-lifecycle.md) (spawn-order validation),
[../docs/prd/SPEC-agent-tool-delivery.md](../docs/prd/SPEC-agent-tool-delivery.md)
(the delivery division of labour).

## 1. What an image spec is

An image spec is a small, strictly-constrained JSON document that customizes
the per-agent rootless image at spawn with **package-add directives only**:

```json
{
  "base": "docker.io/library/ubuntu:24.04",
  "packages": [
    {"manager": "apt", "packages": ["jq", "curl"]},
    {"manager": "go",  "packages": ["golang.org/x/tools/gopls@latest"]}
  ]
}
```

It is not Dockerfile text: arbitrary Dockerfile instructions (USER, ENV,
ENTRYPOINT, mounts, ...) are unrepresentable, the parser rejects unknown
fields, and every rendered package token is validated against a per-manager
grammar and single-quoted into the RUN line (GAP-148), so chaining,
substitution and redirection are structurally impossible. Hard bounds (spec
size, directive count, packages per directive, token length, build timeout)
live in `internal/imagespec/spec.go`. The validated spec is built once through
the agent's own rootless socket and cached per spec identity
(`agent.image_spec.cache_dir`, one build per `Spec.CacheKey`).

## 2. What every built image always contains (stock layer, DF-BUNKER-80)

A spec is a package-ADD, never an image replacement. The render always emits a
stock-userland apt layer between FROM and the spec's own lines —
`StockToolchainPackages`: **git, the docker client (docker.io), python3, make,
jq, ca-certificates**. A `go` directive additionally bootstraps the Go
toolchain (`golang-go`) before its install lines (DF-BUNKER-79) and pins
`GOBIN=/usr/local/bin` (the first component of the agent exec PATH), so
installed tools are reachable.

## 3. The delivered tool set (TOOLS-B2)

The remote editing verbs execute inside the agent, and the GAP-092 measurement
(three independent fresh spawns) found the verb dependencies split:

| Tool | Stock agent | Delivered by | Manager | Why this path |
|------|-------------|--------------|---------|---------------|
| git | present (2.47.3) | stock layer | apt | agent-tools REQUIRED; kept by the stock-userland guarantee |
| jq | present (1.7) | stock layer | apt | GAP-092-measured stock extra |
| toolsd | absent | **vendored copy** — `bunker agent-tools --install` | n/a (no registry) | self-built binary; a copy is the only honest mechanism (GAP-096) |
| rg | absent | **image-spec package-add** | apt → `ripgrep` | registry tool: apt keeps version pinning + signature verification |
| gopls (language server) | absent | **image-spec package-add** | go → `golang.org/x/tools/gopls@latest` | registry tool: go install; the directive's toolchain bootstrap + GOBIN pin put it on the agent PATH |

The package-add half is recorded canonically in
`internal/imagespec/parse.go` (`AgentToolPackages`, `AgentToolSpec`):

```json
{"packages":[{"manager":"apt","packages":["ripgrep"]},{"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}
```

`bunker agent-tools <id>` prints exactly this directive when its probe finds
rg or the language server missing, and refuses to copy them (a binary copy
would silently discard the pinning and signature verification that make the
package path the right one). The two delivery paths do not overlap: toolsd
never appears in a package-add directive, and rg/gopls are never copied.

**Deliberate boundary:** the delivered set is the opt-in spec surfaced by
tooling and docs, not yet an automatic spawn step — a spec change re-keys the
spec-derived image build (`Spec.CacheKey`), so defaulting it at spawn is a
live-E2E change (see the delivery spec's remaining work).

## 4. Acceptance pinning

`internal/imagespec/agent_tools_test.go` pins the set (membership, wire form
round-trip, single-line paste-ability, render installs both tools with the
toolchain-bootstrap ordering, cache-key derivability); `internal/cli`'s tests
pin that the printed remediation is the same canonical constant and that its
render is buildable. The set's closure criterion is a fresh provisioned agent
reporting rg and gopls present with versions via `bunker agent-tools`.
