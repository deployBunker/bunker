# Go Module Dependency Audit — 2026-10-21 (SUP-DEPS-20261004)

- Base commit: `d7c67f350e5c29d1c6382e5300090569ae84cfc5` (branch `wt/SUP-DEPS-20261004`)
- Worker: coding-hermes worker, task `SUP-DEPS-20261004`
- Scope: read-only audit of the full module graph (`go list -u -m -json all`), classification,
  then conservative tested upgrades. Board bookkeeping untouched (foreman-owned).

## Live candidate count

The supervisor's count of 21 was stale. Live audit on the base commit:

- Modules in graph: **66** (`go list -m all | wc -l` minus main)
- Modules with available updates: **26** (5 direct, 21 indirect)

## Direct modules (go.mod `require` block, 9 total, 5 with updates)

| Module | Current | Available | Class | Decision |
|---|---|---|---|---|
| connectrpc.com/connect | v1.21.0 | v1.21.1 | patch | DEFER — upgrade-caused regression (see Rejected/deferred; bisected) |
| github.com/caddyserver/certmagic | v0.25.4 | v0.25.6 | patch | SELECT — TLS/ACME front door; compatible patch |
| github.com/go-chi/chi/v5 | v5.3.2 | v5.5.0 | minor | SELECT — chi is API-stable across minors; gated by build/vet/test |
| github.com/golang-jwt/jwt/v5 | v5.3.1 | — | — | none |
| github.com/hanwen/go-fuse/v2 | v2.11.0 | v2.12.0 | minor | SELECT (provisional) — core mount driver; gated by build/vet/test; defer on breakage |
| github.com/spf13/cobra | v1.10.2 | — | — | none |
| github.com/spf13/viper | v1.21.0 | — | — | none |
| github.com/spf13/pflag | v1.0.10 | — | — | none |
| go.yaml.in/yaml/v3 | v3.0.5 | — | — | none |
| google.golang.org/protobuf | v1.36.12 | — | — | none |

## Indirect modules with updates (21)

### Security-relevant (govulncheck evidence, uncalled-in-binary but imported; fix available)

| Module | Current | Available | Vulns cleared | Trace class |
|---|---|---|---|---|
| golang.org/x/net | v0.57.0 | v0.61.0 | GO-2026-6603, 6610, 6611, 6612, 6617 (fixed v0.60.0) | module-level; std `net/http` HTTP/2 shares the vulnerable code lineage |
| golang.org/x/crypto | v0.54.0 | v0.58.0 | GO-2026-6303, 6354, 6355 (fixed ≤v0.56.0) | module-level |
| golang.org/x/text | v0.40.0 | v0.43.0 | GO-2026-6629 (fixed v0.41.0) | module-level |

### Security-relevant (no fix available)

| Module | Finding | Status |
|---|---|---|
| golang.org/x/crypto | GO-2026-5932 | No fixed version published in any release — monitor; nothing to upgrade to |

### Companion bumps (required by / accompany the security set; no direct vuln)

| Module | Current | Available | Reason |
|---|---|---|---|
| golang.org/x/mod | v0.38.0 | v0.42.0 | pulled by x/tools (semver) |
| golang.org/x/sync | v0.22.0 | v0.24.0 | x/net companion |
| golang.org/x/sys | v0.47.0 | v0.49.0 | floor for all x/* bumps |
| golang.org/x/term | v0.45.0 | v0.47.0 | x/crypto/sys companion |
| golang.org/x/tools | v0.48.0 | v0.52.0 | supplies `x/net/http2` to `net/http` via `httpguts`; certmagic/dns dependency |

### Maintenance patches (indirect, low risk)

| Module | Current | Available | Note |
|---|---|---|---|
| github.com/caddyserver/zerossl | v0.1.5 | v0.1.6 | certmagic companion |
| github.com/go-jose/go-jose/v4 | v4.1.4 | v4.1.6 | JOSE crypto surface |
| github.com/golang/protobuf | v1.5.0 | v1.5.4 | compat shim |
| github.com/klauspost/cpuid/v2 | v2.4.0 | v2.4.1 | quic-go dep |
| github.com/mholt/acmez/v3 | v3.1.6 | v3.1.7 | certmagic ACME client |
| github.com/miekg/dns | v1.1.72 | v1.1.73 | certmagic DNS-01 |
| github.com/rogpeppe/go-internal | v1.15.0 | v1.16.0 | minor, testinfra |

### Test/graph artifacts (tidy-managed; not hand-pinned)

github.com/cpuguy83/go-md2man/v2 v2.0.7, github.com/jordanlewis/gcassert (pseudo),
github.com/yuin/goldmark v1.8.6, go.uber.org/mock v0.6.0, golang.org/x/telemetry (pseudo),
gopkg.in/check.v1 (pseudo). These ride along via `go mod tidy`; `go mod why -m` confirmed
goldmark and x/telemetry are not needed by the main module and drop out of go.mod.

## Standard library finding (toolchain, not go.mod)

govulncheck on go1.26.5: **16 CALLED standard-library vulnerabilities** with in-binary
traces through `internal/server`, `internal/fsclient`, `internal/davserve`,
`internal/registry`, `proto/bunker/v1` — GO-2026-6617, 6613, 6612, 6611, 6610, 6608,
6607, 6605, 6603, 6218, 6090, 6089, 6088, 5972, 5942-family, 5026. All fixed by
**go1.26.10** (latest 1.26.x patch at audit time; verified via module proxy
`go list -m -versions golang.org/toolchain`).

Decision: pin `toolchain go1.26.10` in go.mod (repo uses `GOTOOLCHAIN=auto`; CI reads
`go-version-file: go.mod` per QA-BUNKER-15, so the pin propagates to CI as the declared
toolchain rather than a silent second download).

## Batches applied

- Batch 1 (security, indirect x/*): x/net, x/crypto, x/text, x/mod, x/sync, x/sys, x/term, x/tools
- Batch 2 (direct): certmagic, chi, go-fuse, quic-go (connect attempted, reverted after bisect)
- Batch 3 (indirect maintenance): zerossl, go-jose, golang/protobuf, cpuid, acmez, miekg/dns, rogpeppe/go-internal
- Batch 4: toolchain pin go1.26.10

## Commands and results

| # | Command | Result |
|---|---|---|
| 1 | `go list -u -m -json all` (base d7c67f35) | 66 modules in graph, 26 update candidates (5 direct / 21 indirect) |
| 2 | `go mod tidy` (baseline, before upgrades) | only reclass: fsnotify `// indirect` → direct (code already imported it) |
| 3 | `govulncheck ./...` (base, go1.26.5) | 16 CALLED stdlib vulns; module-level uncalled: x/net ×5 (fix v0.60.0), x/crypto ×3 fixable + GO-2026-5932 no-fix, x/text ×1 (fix v0.41.0) |
| 4 | `go get` x/net v0.61.0, x/crypto v0.58.0, x/text v0.43.0, x/mod v0.42.0, x/sync v0.24.0, x/sys v0.49.0, x/term v0.47.0, x/tools v0.52.0 + `go mod tidy` | applied clean |
| 5 | `go get` certmagic v0.25.6, chi v5.5.0, go-fuse v2.12.0, quic-go v0.64.1 (connect v1.21.1 attempted, then reverted) | applied clean; zerossl v0.1.6, acmez v3.1.7, miekg/dns v1.1.73 rode along as required-by bumps |
| 6 | `go get` golang/protobuf v1.5.4, go-jose v4.1.6, cpuid v2.4.1, rogpeppe/go-internal v1.16.0; `go get toolchain@go1.26.10` + `go mod tidy` | applied clean; toolchain downloaded and used |
| 7 | `go build ./...` | PASS |
| 8 | `go vet ./...` | PASS |
| 9 | `go test ./... -count=1 -short -p 4` (final tree) | PASS — exit 0, all packages ok (server 17.0s, webdav 15.6s, fsmount 8.8s, registry 2.9s, mountdriver, webdav-battery all ok) |
| 10 | `govulncheck ./...` (after, go1.26.10) | **0 called, 0 in imported packages, 1 module-level residual** (GO-2026-5932, x/crypto, no fix published anywhere, uncalled) |

Pre-fix tree note: the first full-suite run (connect v1.21.1 still in) failed 2 tests —
one bisected to connect (deterministic, deferred above), one (`TestQA36...CoarseClockWindow`)
was a `-p 4` load flake: green standalone twice and green in the final-tree full run.

---
ch:trace row=SUP-DEPS-20261004 spec=.coding-hermes/board/tasks.jsonl#SUP-DEPS-20261004 wave=.coding-hermes/waves/bunker-foreman-2026-10-21-17-15-43.json#1 test=docs/deps-audit-2026-10-21.md::command-results doc=docs/deps-audit-2026-10-21.md prompt=/tmp/bunker-foreman-2026-10-21-17-15-43-SUP-DEPS-20261004-brief.txt evidence=docs/deps-audit-2026-10-21.md witness=none:worker-local-audit verdict=none:foreman-judge-pending commit=none:pending-foreman-trace memory=none:not-applicable
STATUS: complete
TIER: T1 — invoked and observed on this worktree: go list -u -m -json all (26 live candidates on 66 modules), go get batches via staged scripts, go mod tidy, go build ./... PASS, go vet ./... PASS, go test ./... -count=1 -short -p 4 PASS (exit 0), govulncheck before (16 called stdlib + 11 module findings) and after (0 called, 1 uncalled no-fix residual GO-2026-5932), single-variable bisect of the connect v1.21.1 regression
EVIDENCE: docs/deps-audit-2026-10-21.md

## Rejected / deferred

### connectrpc.com/connect v1.21.0 → v1.21.1 — DEFERRED (upgrade-caused regression, bisected)

- With connect v1.21.1 in the graph, `TestStreamingEnvelope_RejectedEndOfStreamForms/trailing_0x02_byte`
  fails deterministically (reproduced standalone twice):
  `streaming_envelope_test.go:523: in-stream error code = "internal", want "invalid_argument"`
  (message: `protocol error: incomplete envelope: unexpected EOF`).
- With connect held at v1.21.0 (all other upgrades in place), the same test passes.
- Bisect was single-variable: only connect was moved. v1.21.1 reclassifies a
  truncated-envelope protocol error from `invalid_argument` to `internal`. No CVE is
  attached to v1.21.0, so there is no security driver forcing the bump.
- Re-mapping a protocol error class is an owner/spec decision (connect error-code
  contract is a repo convention per AGENTS.md), not a deps-worker call. Deferred;
  the v1.21.0 pin (explicit, not merely inherited) documents this decision in go.mod.

### go-fuse v2.11.0 → v2.12.0 — SELECTED (held)

Selected provisionally; no API break (build/vet clean, fuse/mount packages pass in the
gate suite). If the foreman's full non-short battery disagrees, this is the first
candidate to revert.

## Toolchain / stdlib outcome

`toolchain go1.26.10` pinned (go.mod), clearing all 16 called stdlib findings; local run
used the downloaded go1.26.10 toolchain. On CI the pin is the declared version
(go-version-file), so no silent second toolchain (QA-BUNKER-15).

Residual: GO-2026-5932 (x/crypto) — no fixed version published in any release at audit
time. Uncalled. Nothing to upgrade to; monitor upstream.

## Audit-time facts vs. supervisor brief

- Supervisor count "21 outdated modules" was stale: live audit = 26 candidates
  (5 direct + 21 indirect) across 66 modules. Reported live count; stale count not preserved.
