# REV-BUNKER-005 / 006 / 007 — the security trio: decisions, numbers, evidence

Graded revision: the battery below was run against **`dbd9e14`**; worktree HEAD is now **`3587219`**. Every
source blob the battery mutates and restores is **byte-identical at both revisions** (sha256 compared per
file, §0a), and the commits in between touch `docs/evidence/` only — so the verdict below covers the current
HEAD.

A second writer's independent live-wire battery also lives under `docs/evidence/` as
`REV-BUNKER-005-006-007-live-*.{sh,md,txt,patch}` (see §6); it is a different measurement of the same
revision, not a duplicate of this one.

Companion artifacts, all under `docs/evidence/`:

| artifact | what it is |
|---|---|
| `REV-BUNKER-005-006-007-controls.sh` | the battery that produces the RED and CONTROL phases |
| `REV-BUNKER-005-006-007-controls.txt` | its output: 6 phases, each with blob hash before/mutated/after + the restore proof |
| `REV-BUNKER-005-006-007-red.txt` | the three RED phases, extracted verbatim |
| `REV-BUNKER-005-006-007-green.txt` | the GREEN run on the clean revision + the blob hashes |
| `REV-BUNKER-005-006-007-suites.txt` | `go test ./... -count=1` on the graded revision |
| `REV-BUNKER-005-006-007-graph-consumers.txt` | the `/graph` consumer sweep, with the commands |

---

## 0. Headline

**005 — DECISION: the `/graph` routes are CREDENTIALED, not flag-gated.** They now demand the daemon's own
credential — the **master-only** derivation of the shared validator instance, i.e. the same credential the
Bunkerd RPC plane demands. Measured: unauthenticated `GET /graph/stats` **401**, wrong token **401**,
agent-scoped sub-key **401**, master token **200** with the documented four-field JSON body unchanged, and
with `auth.enabled:false` the routes stay open exactly as the RPC plane is.

**006 — DECISION: the throttle is armed whenever AUTH IS ENABLED, independent of the audit sink.** Audit
stays OPTIONAL; the sink still arms the throttle (no existing caller loses anything) but a nil sink no longer
DISARMS it, and the `New*AuthInterceptor` factories arm it whenever auth is on. Measured with the audit sink
**absent**, in two different configurations, the backoff is shown moving:
`2^1 = 1.999825777s` then `2^2 = 3.999841076s`.

**007 — DECISION: both compares use constant time.** `TokenAuth.authenticate` now calls
`ConstantTimeCompare(token, a.token)`; `apikey.Manager.Validate` now calls
`subtle.ConstantTimeCompare` on the digest. **Honest severity: this is defence in depth and a consistency
fix, NOT a remotely exploitable bug** — see §4.

Nothing in this row changes the wire behaviour of any RPC: no proto change, no new required field, no new
status for an authenticated caller. The only new refusals are on an HTTP surface that had no credential
check at all.

---

### 0a. Revision pin — the battery's blobs are byte-identical at both revisions

| file | sha256 while the battery ran (`dbd9e14`) | sha256 at HEAD (`3587219`) |
|---|---|---|
| `internal/server/server.go` | `8e3b880a679722bcc62a7e721576f0655556707bf3a7873aca5cc014e8089850` | same |
| `internal/auth/httpware.go` | `0e76ec955b0fe0208e8528d66dbb43af7a04ecb60f2bba49a1faa7f22246e657` | same |
| `internal/auth/throttle.go` | `25d1282909427ffeeaa589d39a43dcc613a961666223a73f323cd5f24f8f8a27` | same |
| `internal/auth/interceptor.go` | `490c7f351a62dc70525dabb45b2915e94e3132dc017d121eec5c0b9000fb5a9c` | same |
| `internal/auth/jwt.go` | `bff96df5cd6c6e46c83ba654a53a59bc1a29303eaeafb05c6d73fb3a71ca586b` | same |
| `internal/apikey/manager.go` | `15e5de4f16c9c707a69f58260cf97f1c073852686959c52f47043e593b4cb937` | same |

Commands: `sha256sum <file>` on the clean tree while the battery ran; `git show HEAD:<file> | sha256sum`
after HEAD moved. No source file differs, so the RED/GREEN/control verdicts are pinned to the code at HEAD.

---

## 1. The defects, as filed, verified in source

* **005** — `server.go` registered `/graph/stats`, `/graph/related`, `/graph/impact` on the raw chi router.
  That router carries `RequestID/RealIP/Logger/Recoverer/Timeout` and nothing else; the RPC planes are behind
  the auth interceptors. The surface therefore published the daemon HOST's code graph — file paths and their
  dependency edges — to anyone who could reach the port. `/graph/impact?path=` reflects daemon-CWD paths.
  This had already been named in this repo's own security panel extract
  (`.coding-hermes/evidence/security-panel-2026-09-20/extract-seat-S3-Moonshot-Kimi.md`), which is what the
  row cites.
* **006** — `if s.auditLog != nil { … AttachDenySink … }` gated the deny sink **and** (through
  `SetDenySink`) the throttle. With `audit.enabled:false`, or an audit path that had become unwritable, the
  daemon lost the brute-force backoff AND the denial records together — the throttle disappeared exactly when
  an operator was least able to see it.
* **007** — `if token != a.token` (`internal/auth/interceptor.go`) and `if key.TokenHash == tokenHash`
  (`internal/apikey/manager.go:136`), while `internal/auth/jwt.go` already used
  `subtle.ConstantTimeCompare` in the static-token fallback and the retired-secret check.

---

## 2. 005 — the `/graph` consumer sweep (the row asked what would break)

Run from the worktree root; full output in `REV-BUNKER-005-006-007-graph-consumers.txt`.

| probe | result |
|---|---|
| `grep -rn "graph/stats\|graph/related\|graph/impact"` over `*.go *.sh *.md *.yaml *.yml *.json` | **no consumer.** Only the definitions (`server.go`), the tests added here, the row's own criterion text in `.gitreins/tasks.yaml`, the prior security-panel extract that FILED the defect, and one self-reference (this evidence file's own earlier generation) |
| `grep -rn "graph\|Graph" internal/cli/ --include=*.go` | **no match** — the CLI never calls these routes |
| `grep -rn "NewGraph\|BlastRadius\|hilo\." internal/ \| grep -v _test` | only `server.go` (the registration) and `internal/hilo/hilo.go` (the definitions). No in-process consumer |
| `grep -rln "graph/stats" docs/ README.md` | **no doc instructs a client to call them** |

So no CLI, no in-process caller, no script and no documentation depends on these routes. Stated explicitly
because the row requires it: **credentialing them does not break any local CLI consumer** — there is none.
The credential required is the one every existing client already holds for the RPCs.

**Chosen over a default-off config flag** because (a) nothing reads the routes, so a flag would leave a dead
surface while removing no capability; (b) a flag's "on" state would still need the credential, so the flag
adds a knob without adding safety; (c) the gate is additive — a stock authenticated client is unaffected
(`Authorization: Bearer <token>`). With `auth.enabled:false` there is no credential check anywhere on the
daemon, so these routes are no more open than the RPC plane; the daemon now says so on the structured logger
AND stderr instead of leaving it silent.

---

## 3. The RED / GREEN / NEGATIVE-CONTROL matrix

Every phase mutates the COMMITTED revision, records the blob hash before/mutated/after, and restores with
`git checkout HEAD -- …` followed by `git diff --quiet HEAD` **and** a sha256 comparison. All six phases
turned the cell RED; all six restores are byte-identical.

| finding | RED (the defect reproduced) | cell result | NEGATIVE CONTROL (the fix neutered at a different layer) | cell result | restore |
|---|---|---|---|---|---|
| 005 | the gate object handed to `registerGraphRoutes` is never applied (`gr.Use(gate)` → no-op middleware): the surface is served without credentials, i.e. the pre-fix behaviour | **FAIL** — `unauthenticated is refused on every route`, `a wrong token is refused`, `an agent-scoped sub-key is refused` | the gate IS applied but the middleware admits anything (`if !enabled \|\| jwa == nil` → `if true`): gated by name, open in fact | **FAIL** — same three arms | `server.go` / `httpware.go` byte-identical to `dbd9e14` |
| 006 | both arming sites reverted to their pre-fix form (`return jwtAuth`, `return NewMasterOnlyJWTAuthFromAuth(jwtAuth)`): with the audit sink absent the interceptor carries no throttle | **FAIL** — both arms: `audit disabled in config`, `audit enabled but the path is unwritable` | the throttle is armed but never CONSULTED (`if now.Before(st.backoffUntil)` → `if false && …`): a counter that moves yet never refuses | **FAIL** — both arms, plus the positive control | `interceptor.go` / `throttle.go` byte-identical to `dbd9e14` |
| 007 | both compares back to the pre-fix byte-wise form (incl. dropping the then-unused `crypto/subtle` import = the base blob's exact shape) | **FAIL** — `TokenAuthComparesInConstantTime` (auth) and `ValidateComparesInConstantTime` (apikey) | the call sites still SAY `ConstantTimeCompare` while the shared helper returns `a == b`: the cell must reach THROUGH the helper to `crypto/subtle` rather than trust the identifier | **FAIL** — `TokenAuthComparesInConstantTime` | `interceptor.go`, `manager.go`, `jwt.go` byte-identical to `dbd9e14` |

Why each fix needs two neuters: the RED proves the *fix* is load-bearing; the control proves the *layer the
cell names* is. A registration that never applies its gate and a gate that never validates are different
failures, and a cell that only detects the first would pass a wired-but-inert middleware.

**GREEN** (`REV-BUNKER-005-006-007-green.txt`, same revision, blob hashes equal to the ones the battery
restored):

```
--- PASS: TestREVBUNKER005_GraphRoutesRequireCredentials
    (unauthenticated 401 x3 routes, wrong token 401, agent-scoped 401,
     master 200 + body parses as the four documented fields, related/impact 200,
     auth-disabled arm stays open)
--- PASS: TestREVBUNKER006_ThrottleArmedWithoutAuditSink (4.87s)
    audit sink absent (audit disabled in config):            throttled at 2^1=1.999825777s then 2^2=3.999841076s
    audit sink absent (audit enabled but path unwritable):   throttled at 2^1=1.999845013s then 2^2=3.998771306s
--- PASS: TestREVBUNKER006_AuditSinkStillRecordsWhenPresent
--- PASS: TestREVBUNKER007_TokenAuthComparesInConstantTime  + TestREVBUNKER007_TokenAuthAdmissionUnchanged
--- PASS: TestREVBUNKER007_ValidateComparesInConstantTime  + TestREVBUNKER007_ValidateAdmissionUnchanged
```

### The throttle-moves proof (the row asked for the counter moving, not a code path)

Driven through the daemon's OWN composition (`buildAuthInterceptors`, the function `Run` calls) over a real
connect stack, with `s.auditLog == nil` asserted first:

1. failures 1..5 from one source → `CodeUnauthenticated` (ordinary denials);
2. the 6th → `CodeUnavailable`, with the engine's own log line carrying `retry_in=1.999825777s` (= the 2^1
   step, allowing for the microseconds elapsed between arming and reading);
3. wait out the backoff; the next failure is a plain denial again (backoff expired) **and the counter is
   still over the threshold**, so the authority re-arms;
4. the next request → `CodeUnavailable` with `retry_in=3.999841076s` = the 2^2 step.

Two different numbers from two different requests is the counter moving. A code path that merely existed
would report the same figure forever. A legitimate client from a different source is unaffected
(`Bearer <master>` → 200) in the same run.

Both sink-less configurations were driven separately, because they are reached differently:
`audit.enabled:false` in config, and `audit.enabled:true` with a path whose parent is a regular file (so the
audit log cannot be opened and the daemon warns and runs without one — the "unwritable audit path" case).

---

## 4. 007 — the honest severity note

**This is defence in depth and an internal-consistency fix. It is not a remotely exploitable bug, and it was
not filed as one.**

Both sites are compared in constant time as of this row, but neither comparison leaked a usable secret:

* `apikey.Manager.Validate` compares the **SHA-256 hex digest** of the presented token. The digest is a
  one-way function of the secret, and a caller cannot steer the compared value toward a digest it does not
  already know — observing the byte position of the first difference in a comparison of two one-way digests
  does not disclose the token.
* `TokenAuth` is the **static-token migration fallback** (the JWT path already compared in constant time).
* `subtle.ConstantTimeCompare` itself returns immediately on a length mismatch, so it does not hide the
  LENGTH of a presented credential; that is inherent to the primitive and is unchanged by this row.

The reason to fix it is the one the row gives: this codebase already applies the discipline in `internal/auth`
(jwt.go's static fallback, its retired-secret check) and in the WebDAV handler, and a fourth site that compares
byte-wise trains the next reader to copy the wrong one. The cell asserts the SOURCE, because the two forms are
**behaviourally identical by design** — which is why each 007 test also carries an admission-equivalence table
(correct token admitted; equal-length wrong, one-byte-shorter, one-byte-longer, matching-prefix and empty
tokens all denied with `CodeUnauthenticated`; revoked still wins over a digest match) proving the swap changed
no decision.

---

## 5. What this row did NOT do (residuals, no rows filed per the brief)

* **No config flag** was added for the `/graph` surface — that was the rejected alternative, deliberately
  (§2). If an operator wants the routes gone entirely, the gate is one registration call in `Run`.
* **The throttle is per-process and in memory.** A bunkerd restart resets the per-source counters; only the
  audit trail keeps the history. That is pre-existing (GAP-133) and out of this row's scope.
* **`/healthz` stays uncredentialed**, deliberately: it is a liveness probe, not host data.
* **No remote/live E2E battery** was run on `bunker-mvp`: this row changes the router's middleware for one
  route group and the auth constructors; the local full suite (`REV-BUNKER-005-006-007-suites.txt`) plus the
  connect-stack cells above are the evidence. Nothing here touches spawn/destroy/exec/docker/SSH, so the
  AGENTS.md E2E trigger does not apply.
* **Test-file naming**: the cells live in `internal/server/rev_bunker_005_006_test.go`,
  `internal/auth/rev_bunker_007_test.go` and `internal/apikey/rev_bunker_007_test.go`.

---

## 5a. One unrelated failure in the full suite, attributed and NOT filed

`REV-BUNKER-005-006-007-suites.txt` records `go test ./... -count=1` on the graded revision with exactly one
FAIL:

```
--- FAIL: TestSpawnRollbackSurvivesStuckCleanupStep (1.09s)   [internal/agent]
    spawn_rollback_budget_test.go:579: unexpected notice shape:
      "userdel attempt failed: userdel bunker-dfb21-stuck-77912: signal: killed (output: )"
FAIL    github.com/deployBunker/bunker/internal/agent    99.956s
```

**It is not this row's.** Three independent measurements:

1. **No diff**: the trio's commits (`f8a883c`, `dbd9e14`) touch `internal/server`, `internal/auth`,
   `internal/apikey` and `docs/` — `git diff --name-only f8a883c~1 HEAD -- internal/agent/` is EMPTY.
2. **No dependency**: `grep -rn "internal/auth\|internal/apikey" internal/agent/` is EMPTY, so the agent
   package's test binary is not compiled against anything this row changed — reverting the trio cannot
   change its behaviour.
3. **Identical rate on both sides**: `-count=6` of that single test gives **1 FAIL / 6** with the trio applied
   and **1 FAIL / 6** with the trio's five blobs reverted to their base revision (restored byte-identically
   afterwards). The same test also passed on the earlier full-suite run of the same trio content.

Mechanism: this host was at `loadavg 102` (concurrent fleet workers) and the failing notice is a `userdel`
child killed by signal — a timing/resource-sensitive path, not a logic change. Reported here rather than
filed as a row, per the brief. Left alone: the fix belongs to whichever lane owns `internal/agent`, and an
unrelated repair inside this worktree would collide with the other writers in it.

---

## 6. Provenance — a second live session was writing this same worktree

Stated because it changes how the revision should be read, not because it changes the code.

A **second live session** (`hermes chat -q` with this row's identical brief, PID 3809581, started 10:42:55,
47 s after this session's PID 3800699) was working in **this same worktree** for the whole of this session,
identified from the process table (`/proc/<pid>/cwd` = this worktree, `/proc/<pid>/cmdline` = the same brief).

Consequences, all verified rather than assumed:

* its 007 source edits were already in the tree, uncommitted, when this session started → committed by this
  session as part of the intermediate commit;
* its 005 gate wiring (`graphAuthMiddleware`) and its 006 arming/idempotence design landed in the tree while
  this session was writing; the tree was briefly non-compiling because the two writers assumed different
  signatures for `registerGraphRoutes`. This session **converged** onto the sibling's call shape instead of
  re-asserting its own, added the one definition its call site already expected, and deleted its own duplicate
  call site;
* the sibling then **rebased** the intermediate commit onto a newer `main` (`f8a883c`) and committed the whole
  converged tree as `dbd9e14` ("RESCUED WORK … recovered from a worker that died before committing"), having
  judged this session dead from the size of its `-Q` log. That judgement is wrong for `-Q` sessions: the log
  stays at the startup bytes until the process exits because output is buffered. The work in that commit is
  correct and complete; the "died" framing is not.

So `dbd9e14` is the converged revision of **two** writers, and the RED/GREEN/control battery above was run by
this session against that exact revision (`git diff --quiet HEAD` clean before and after every phase).
