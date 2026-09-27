# REV-BUNKER-005/006/007 — independent verification of the landed trio

**This is the second worker's verification, not the implementation.** The row was
worked by another session in the same worktree; that session is *still alive* and
was wrongly reported dead by the driver, which committed its working tree as
`dbd9e14` ("RESCUED WORK"). Re-implementing the three fixes would have destroyed
its in-flight work, so this bundle does the other half of a duplicate dispatch:
it proves the landed commit on the **wire**, with its own RED, its own GREEN and
its own negative controls, and it names what is still open.

## The decision and the numbers

**005 — DECISION: gate the routes with the daemon's own credential model; do not
add a default-off config flag.** A flag would leave a *dead* surface (nothing
reads these routes — see the grep below) while removing no capability; the gate
removes host reconnaissance and needs no new operator action, because a client
that can call any Bunkerd RPC already holds the credential it needs. Auth-off
deployments keep the RPC plane's own posture (open) and say so on both streams.

| arm | tree | cells | result |
|---|---|---|---|
| RED | `11001d3` (filed) | 14 | **5 defective** — 4 `/graph` cells (no credentials ×3, wrong token ×1) + the 6th-auth cell |
| GREEN | `dbd9e14` (landed) | 14 | **14/14 as accepted** |
| control 005 | `dbd9e14` + gate neutered | 14 | 4 `/graph` cells RED, 10 others green → control PASSED |
| control 006 | `dbd9e14` + `ArmThrottle` no-op | 14 | 1 throttle cell RED, 13 others green → control PASSED |
| control 007 | filed blobs of the two compare sites | 2 source cells | 2 RED, both package suites still green → control PASSED |

Numbers on the wire, not in the code:

- **RED (005):** unauthenticated `GET /graph/stats`, `/graph/impact?path=…` and
  `/graph/related?path=…` returned **200** with
  `{"total_edges":3,"unique_files":4,"unique_deps":2,"files_with_edges":2}` — the
  daemon host's dependency graph, readable by anyone who can reach the port.
- **GREEN (005):** the same three routes return **401** without credentials and
  **200** with a valid token (`{"code":"unauthenticated","message":"valid credentials
  required"}` otherwise). Authenticated clients are unaffected.
- **RED (006):** six bad tokens on **one connection**, `audit.enabled: false` →
  `401 401 401 401 401 401`. No throttle.
- **GREEN (006):** same six, same single connection (`local_port 46882`, six
  times) → `401 401 401 401 401 503`. The five failures before the threshold are
  still ordinary denials; only the sixth changes code — the throttle *moves*,
  with the audit sink absent.
- **Wire parallelism:** the RPC plane is untouched — `POST /bunker.v1.Bunkerd/ServerInfo`
  is 200 with a valid token and 401 without it, in both arms.
- **Non-vacuity:** `/healthz` stays 200 throughout, so a 401 above is the gate and
  not a daemon that answers 401 to everything.

## The three negative controls

Each is a *mutation of the fix on the frozen landed tree*, applied as a patch,
proven to have landed by sha256, and restored with a re-checked sha256.

| control | mutation | must go RED | must stay GREEN | restore |
|---|---|---|---|---|
| 005 | register the routes with no gate (`func(next http.Handler) http.Handler { return next }`) | the three unauthenticated `/graph/*` cells (200) | throttle cell, `/healthz`, both RPC cells | byte-identical, `server.go` sha256 `8e3b880a…` |
| 006 | `auth.ArmThrottle` becomes a no-op | the 6th-failed-auth cell on the audit-off daemon (401) | all three `/graph` cells, `/healthz`, both RPC cells | byte-identical, `interceptor.go` sha256 `490c7f35…` |
| 007 | the **filed blobs** of the two compare sites (base `4879343`) | both source cells | `internal/auth` + `internal/apikey` suites | byte-identical, `52a4af0c…` / `15e5de4f…` |

Cross-attribution is the point: neutering 005 reddens **only** 005's cells, and
neutering 006 reddens **only** 006's cell. The rest of the probe stays green in
both, so the reddening is this row's defect and not a broken harness.

## 007 — the honest severity note

`internal/auth/interceptor.go:168` (`if !ConstantTimeCompare(token, a.token)`) and
`internal/apikey/manager.go:143` (`subtle.ConstantTimeCompare([]byte(key.TokenHash),
[]byte(tokenHash)) == 1`) are the only two credential comparisons in the tree that
were still byte-wise; `internal/auth/jwt.go` already used `subtle` at three sites
(including its own `ConstantTimeCompare` helper, which the interceptor now reuses).

**This is defence in depth and an internal-consistency fix, not a remotely
exploitable bug**, and the control says so out loud: with the *filed* blobs
restored, both packages' suites still pass (`internal/auth ok`, `internal/apikey
ok`). The byte-position timing signal is real in principle — `!=` returns at the
first differing byte — but the apikey site compares SHA-256 hex digests of the
secret (already one-way) while iterating a Go map whose order is randomised per
iteration, and the static-token site is a migration fallback over a
config-supplied shared secret. Removing the signal costs nothing and makes the
three surfaces agree; it is not claimed as a vulnerability closure.

## The /graph consumer grep (before touching anything)

`grep -rn 'graph/stats|graph/related|graph/impact|registerGraphRoutes|graphAuthMiddleware'`
over `*.go *.sh *.yaml *.md *.ts`, excluding `.gitreins/`, `.coding-hermes/` and
`docs/evidence/`, returns **only `internal/server/server.go`** (the registration,
its gate, and the auth-off warning) plus the new test file and the httpware
comment. There is **no CLI command, no in-process caller and no script** that
reads these routes — the repo's own tooling stops at `/healthz`. Gating therefore
removes no capability, which is what makes option (b) the wrong choice here.

## Provenance, stated plainly

- This worktree (`wt/REV-BUNKER-SEC-TRIO`) was written by **another live session**
  while I read it: `server.go` mtime 10:44:19, `heartbeat_test.go` ~10:45:25, a
  `go test ./internal/server` run at ~10:45:00, and `gitreins guard` + commit
  `11001d3` at 10:46:56. I confirmed it was alive by process census (two
  `hermes` PIDs, `cwd` = this worktree) and by continuing file writes — **never
  by log size**; its own rescue message records the same lesson.
- The two 007 source edits at mtimes 10:44:53/10:44:56 were **mine**; the other
  session committed them inside `11001d3` with an honest provenance note.
- The driver then judged that session dead (a 43-byte `-Q` log means *running*,
  not dead) and committed its tree as `dbd9e14`. I did not re-implement anything;
  I verified `dbd9e14` from two **frozen clones** (`git clone --shared` +
  `git checkout <sha>`) so my numbers never race the live tree.

## Reproducing

```
D=$(mktemp -d); git clone --shared <repo> $D/repo; cd $D/repo; git checkout dbd9e14
sh docs/evidence/REV-BUNKER-005-006-007-live-probe.sh $D/repo /tmp/green.txt   # expect exit 0
sh docs/evidence/REV-BUNKER-005-006-007-live-controls.sh $D/repo 005 /tmp/c5.txt  # expect exit 0
sh docs/evidence/REV-BUNKER-005-006-007-live-controls.sh $D/repo 006 /tmp/c6.txt  # expect exit 0
```

The probe boots the real `bunkerd` binary twice in a `mktemp -d` scratch tree
(loopback ports, generated token, `registry.enabled:false`, tunnel/tailscale off),
so it measures the shipped binary and the real config loader, not a stub. Its
exit code is `0` only when every cell matches the row's acceptance.

## Residuals this verification does NOT close

1. **The throttle's accounting unit is the full peer address (`IP:port`).** A
   client that opens a **fresh TCP connection per attempt** gets a fresh key and
   is never throttled: this is why the 006 arm had to issue its six requests in
   ONE `curl` invocation (proven in the transcript — all six on `local_port
   46882`), and why the "fresh connection is not throttled" cell is a documented
   *expected* 401. Real gap, unchanged by this row, worth its own row.
2. **With `auth.enabled: false` the `/graph` routes are open by design** (the
   RPC plane is too) and now warn on both streams. A deployment running auth off
   is not made safer by this row.
3. **With audit disabled, denials are throttled but not recorded anywhere** —
   that is the row's chosen shape (throttle mandatory, records optional), not an
   oversight, but it is the one visibility gap that remains.
4. **Not verified here:** the remote live E2E battery on `bunker-mvp` (needs the
   box), and any edits the live session made after `dbd9e14` — at the time of
   writing, `internal/auth/throttle.go` and `internal/auth/httpware.go` were
   dirty in the worktree, so the *landed* commit is what is verified.

## Files in this bundle

| file | what it is |
|---|---|
| `…-live-probe.sh` | boots the real daemon twice and measures 005/006 on the wire |
| `…-live-controls.sh` | applies one mutation per arm, asserts the cell that must redden, restores with a sha256 re-check |
| `…-live-red.txt` | filed behaviour, frozen at `11001d3` |
| `…-live-green.txt` | accepted behaviour, frozen at `dbd9e14` |
| `…-live-control-005.txt`, `…-live-control-006.txt` | the two mutations, with `[control RED]` / `[still ok]` attribution |
| `…-live-007.txt` | source assertions, package suites, filed-blob control, byte-identical restore |
| `…-live-negative-control-005.patch`, `…-live-negative-control-006.patch` | the exact mutations |
