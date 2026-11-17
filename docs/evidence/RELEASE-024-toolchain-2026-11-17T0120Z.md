# RELEASE-024 — pin Go toolchain go1.26.11 and rebuild artifacts for GO-2026-6859

- **Date (UTC):** 2026-11-17T01:20Z
- **Worktree:** `/home/kara/worktrees/bunker-RELEASE-024`
- **Branch:** `wt/RELEASE-024`
- **Base:** `origin/main` @ `91b453dd`
- **Advisory:** GO-2026-6859 — "HTTP/1 server crash due to MaxBytesReader header race in net/http"
  (<https://pkg.go.dev/vuln/GO-2026-6859>)
- **Ticket:** board row RELEASE-024 (P1), v0.4.0 prep

## 1. Change

One line in `go.mod`:

```diff
 go 1.26.5
 
-toolchain go1.26.10
+toolchain go1.26.11
```

The language version (`go 1.26.5`) is deliberately unchanged; only the
`toolchain` directive moves. `go.sum` and every `require` line are untouched.

The pin is single-source by construction: every workflow step that selects a
Go toolchain reads it from `go.mod` via `go-version-file`, so no workflow file
needs (or gets) a hardcoded version:

```
$ grep -n "go-version" .github/workflows/ci.yml .github/workflows/release.yml
.github/workflows/ci.yml:116:          go-version-file: "go.mod"
.github/workflows/ci.yml:298:          go-version-file: "go.mod"
.github/workflows/ci.yml:417:          go-version-file: "go.mod"
.github/workflows/ci.yml:543:          go-version-file: "go.mod"
.github/workflows/release.yml:80:          go-version-file: "go.mod"
```

Upstream publishes `go1.26.11` (and `go1.27.4`) — confirmed on the host:

```
$ curl -s https://go.dev/dl/?mode=json | grep -o '"version": *"go1\.2[67]\.[0-9]*"' | sort -u
"version": "go1.26.11"
"version": "go1.27.4"

$ GOTOOLCHAIN=go1.26.11 go version
go version go1.26.11 linux/amd64
```

`go1.26.11` is the minimal in-line fix for this advisory; moving to the
`go1.27.x` minor is out of scope for a patch prep.

## 2. Version surfaces already at 0.4.0 (verified, not redone)

Verified in-tree at the prep base (`608e9fa2` landed these on `main`); this
tick does not re-cut them:

| Surface | Value |
|---|---|
| `CHANGELOG.md` | first released section `## 0.4.0 (2026-11-16)` |
| `internal/version/version.go` | `Version = "0.4.0"` |
| `Makefile` | `VERSION ?= 0.4.0` |
| version test | `go test ./internal/version/...` green (see §5) |

The repo's own gate corroborates the parity independently:

```
$ ./scripts/pre-release-check.sh
OK: no top-level vendor/ directory (go stays in module mode)
WARN: between-tags bump state: newest tag v0.3.0 is older than version 0.4.0 — this is the bump commit, so cut the tag next: git tag v0.4.0 HEAD && git push --tags (docs/release-checklist.md); the tag-side gate is CI's Tag build check
OK: version parity — internal/version/version.go=0.4.0 CHANGELOG.md=0.4.0 Makefile=0.4.0; newest tag v0.3.0 (untagged bump to 0.4.0 — tag it next); first CHANGELOG heading: ## Unreleased
```

The `WARN` is the documented between-tags bump state (INT-CI-072): the tag is
cut *after* merge, not by the worker. Exit status is 0.

## 3. AC2a — BEFORE: source scan on the pre-change tree (go1.26.10)

Run with the old pin still in `go.mod`, i.e. the unmodified base tree:

```
$ GOTOOLCHAIN=go1.26.10 govulncheck ./...
rc=3
```

Relevant output:

```
=== Symbol Results ===

Vulnerability #1: GO-2026-6859
    HTTP/1 server crash due to MaxBytesReader header race in net/http
  More info: https://pkg.go.dev/vuln/GO-2026-6859
  Standard library
    Found in: net/http@go1.26.10
    Fixed in: net/http@go1.26.11
    Example traces found:
      #1: internal/server/server.go:24:2: server.init calls middleware.init, which calls http.CanonicalHeaderKey
      #2: internal/fsclient/client.go:424:22: fsclient.Client.do calls http.Client.Do
      #3: internal/cli/metrics.go:106:25: cli.writePrometheusTextfile calls http.Client.Get
      ...
      #60: internal/fsclient/errors.go:176:28: fsclient.OpError.Error calls http.transportReadFromServerError.Error

Your code is affected by 1 vulnerability from the Go standard library.
This scan also found 0 vulnerabilities in packages you import and 3
vulnerabilities in modules you require, but your code doesn't appear to call
these vulnerabilities.
```

The vulnerability is **reachable**, not merely present: 60 example traces
including `http.Server.ListenAndServe`/`ListenAndServeTLS` in
`internal/server/server.go` and the WebDAV handler path.

## 4. AC2b — BEFORE: the shipped v0.3.0 daemon binary

The v0.3.0 assets were built with the affected toolchain, so the released
artifact is vulnerable too. Asset pulled from the public release (local copy
only — nothing uploaded or replaced):

```
$ gh release view v0.3.0 --repo deployBunker/bunker --json tagName,assets --jq '.tagName, (.assets[] | "\(.name) \(.size)")'
v0.3.0
bunker-linux-amd64 20792951
bunker-linux-arm64 19362381
bunkerd-linux-amd64 26596754
bunkerd-linux-arm64 24651013
install.sh 33108
SHA256SUMS 419

$ gh release download v0.3.0 --repo deployBunker/bunker --pattern 'bunker-linux-amd64' --dir ./v030-assets
```

Provenance of the downloaded asset:

```
$ go version -m ./v030-assets/bunker-linux-amd64
./v030-assets/bunker-linux-amd64: go1.26.10
	path	github.com/deployBunker/bunker/cmd/bunker
	mod	github.com/deployBunker/bunker	v0.3.0
```

Binary-mode scan:

```
$ govulncheck -mode binary ./v030-assets/bunker-linux-amd64
rc=3
```

```
=== Symbol Results ===

Vulnerability #1: GO-2026-6859
    HTTP/1 server crash due to MaxBytesReader header race in net/http
  More info: https://pkg.go.dev/vuln/GO-2026-6859
  Standard library
    Found in: net/http@go1.26.10
    Fixed in: net/http@go1.26.11
    Vulnerable symbols found:
      #1: http.CanonicalHeaderKey
      #2: http.Client.Do
      #3: http.Client.Get
      #4: http.Cookie.String
      #5: http.HandleFunc
      Use '-show traces' to see the other 41 found symbols

Your code is affected by 1 vulnerability from the Go standard library.
This scan found no other vulnerabilities in packages you import or modules you
require.
```

## 5. AC2c — AFTER: source scan with the go1.26.11 pin

```
$ GOTOOLCHAIN=go1.26.11 go env GOVERSION
go1.26.11

$ GOTOOLCHAIN=go1.26.11 govulncheck ./...
rc=0
```

```
=== Symbol Results ===

No vulnerabilities found.

Your code is affected by 0 vulnerabilities.
This scan also found 0 vulnerabilities in packages you import and 3
vulnerabilities in modules you require, but your code doesn't appear to call
these vulnerabilities.
```

**Found/fixed pair, stated explicitly:**

- `GO-2026-6859` — **found** in `net/http@go1.26.10` on both the source tree
  (60 reachable traces) and the shipped v0.3.0 `bunker-linux-amd64` binary
  (46 vulnerable symbols), **fixed** by `net/http@go1.26.11`, i.e. by the
  `toolchain go1.26.11` pin in this commit. Verified absent (rc 0) on the
  pinned source tree.

The residual "3 vulnerabilities in modules you require" are *unreachable*
(govulncheck reports them as not called); they are pre-existing, unrelated to
this advisory, and out of scope for a patch prep.

## 6. AC4 — gates on this branch

| Gate | Command | rc | Output tail |
|---|---|---|---|
| Pre-release check (tree mode) | `./scripts/pre-release-check.sh` | 0 | `OK: version parity — internal/version/version.go=0.4.0 CHANGELOG.md=0.4.0 Makefile=0.4.0; ... first CHANGELOG heading: ## Unreleased` (WARN: between-tags bump state, documented) |
| Build | `go build ./...` | 0 | (no output) |
| Vet | `go vet ./...` | 0 | (no output) |
| Tests | `go test ./... -count=1` | 0 | 38 `ok` packages, 7 `[no test files]`, **0** `FAIL`; incl. `ok internal/version 0.040s`, `ok internal/releasecheck 0.667s` |

The build/vet/test runs above execute under the pinned toolchain
(`go.mod` selects `go1.26.11`; `go env GOVERSION` = `go1.26.11`).

## 7. AC3 — release binaries at the prep commit

Built from a clean detached checkout of the single prep commit, so the
binaries carry that commit's identity (`vcs.modified=false`):

```
$ git checkout --detach HEAD
$ make release-binaries
```

`make release-binaries` compiles `bunker` and `bunkerd` for
`linux/amd64` and `linux/arm64` with `CGO_ENABLED=0` (static), into `dist/`
(git-ignored, so the worktree stays clean), and writes `dist/SHA256SUMS`.

Assertions verified for each of the four artifacts:

1. `go version -m dist/<artifact>` → `vcs.revision` equals the prep commit SHA,
   `vcs.modified=false`, toolchain `go1.26.11`.
2. `sha256sum dist/<artifact>` — four values recorded.
3. `govulncheck -mode binary dist/<artifact>` → `GO-2026-6859` absent.

The measurements for 1–3 are recorded at build time in:

- `dist/SHA256SUMS` (four artifact digests + `install.sh`), and
- `dist/BUILDINFO.txt` (full `go version -m` output for all four artifacts
  plus the four binary-mode scan results),

kept outside version control (`dist/` is git-ignored by design — artifacts are
not committed). **Nothing was uploaded, published, or used to replace an
existing release asset.**

## 8. AC4 — fresh-install / version smoke

One built artifact is copied to a scratch directory (a "fresh install" from
the release payload's point of view) and exercised through both version paths:

```
$ cp dist/bunker-linux-amd64 /tmp/smoke-XXXX/bunker
$ ./bunker --version
$ ./bunker version
```

Both must report `0.4.0` with the prep commit SHA. Results recorded in
`dist/BUILDINFO.txt` alongside the artifact measurements.

## 9. AC5 — releng handoff

Hosted CI at the exact prep SHA runs when the foreman merges this branch; the
worker does not push, merge, tag, or cut anything. Remaining releng steps
after merge:

1. **Confirm CI at the merged SHA** — the `Version authority check` and the
   full matrix must be green at the exact post-merge `main` SHA. CI reads the
   toolchain from `go.mod`, so no workflow change is required; expect the logs
   to show `Setup Go` resolving `go1.26.11`.
2. **Cut v0.4.0** — `git tag v0.4.0 <merged-signing-target> && git push
   origin v0.4.0`. Tag only at a green merged SHA. This worker creates no tags.
3. **Publisher step** — publish the release with the complete asset set:
   `bunker-linux-{amd64,arm64}`, `bunkerd-linux-{amd64,arm64}`, `install.sh`,
   and `SHA256SUMS`. Every asset must come from the tag's own build (tag CI),
   not from this worktree's local `dist/`; the local digests in §7 exist to
   cross-check, not to publish.
4. **Post-publication re-scan** — after the assets are public, re-run
   `govulncheck -mode binary` against each published asset and
   `govulncheck ./...` against the tag's source tree, and confirm
   `GO-2026-6859` is absent and rc=0. Also confirm `go version -m` on each
   published asset reports `go1.26.11` (a stale-upload check: a mistyped or
   reused asset would show `go1.26.10`).
5. **Do not** override a red CI run to force the tag; a red tag run means the
   build is not the one that was verified here.

## 10. Notes / residuals

- The pin is deliberately minimal (`go1.26.11`, not `go1.27.4`) to keep the
  v0.4.0 prep a patch-level change.
- `GOTOOLCHAIN` resolution: with `toolchain go1.26.11` in `go.mod` and the
  host's `GOTOOLCHAIN=auto`, every `go` command selects `go1.26.11`
  automatically; the scans above pin it explicitly for auditability.
- No `go.sum`, dependency, or workflow change accompanies this fix.
