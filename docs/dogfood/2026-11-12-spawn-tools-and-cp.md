# Dogfood run 2026-11-12 — spawn tools, real-use loop, and copy cost

Tick: `bunker-dogfood-2026-11-12-13-39-15`
Lane: `bunker-dogfood` (skill-driven, no driver script)
Target: `/home/kara/deployBunker/bunker` @ `1308efa0` (branch `main`, clean tree before this run)
Host clock during measurement: `Thu Nov 12 09:01:47 AM -05 2026`
Server: `bunker-las-03` — agent `b32fdf11` (plain spawn, `--ttl 2h`)
Binary under test: built from this tree with `go build -o <scratch>/bunker ./cmd/bunker` (out of repo, tree untouched)

## Verdict

**Works, with a broken headline feature.** The core loop a user actually performs — spawn, run a
command, copy a file in, run it, inspect the agent, tear down — worked on every attempt once the
agent existed. The one command that promises to hand a user a ready-to-work agent,
`bunker spawn --tools-at-spawn`, failed twice out of two, each time after a silent 5-minute wait.
The published release installer works on a fresh machine.

Rows filed: `DOGFOOD-015` (P1), `DOGFOOD-016` (P2), `DOGFOOD-017` (P2), `PERF-020` (P3).

## What a real user does, and what happened

| step | command | result |
|---|---|---|
| 1. build from source | `go build ./cmd/bunker && ./bunker --version` | OK |
| 2. see the fleet | `bunker status`, `bunker list` | OK |
| 3. get an agent | `bunker spawn --ttl 2h` | OK in **19 s** (a first attempt failed earlier at slice resource limits and was rolled back by the daemon; the retry succeeded) |
| 4. run something | `bunker exec <id> -- 'bash -lc "whoami; id; uname -a"'` | OK, **0.50 s** warm |
| 5. copy a file in | `bunker cp real_task.py <id>:~/real_task.py` | OK, **3.9–4.1 s** for 60 bytes |
| 6. run the file | `bunker exec <id> -- 'python3 real_task.py'` | OK, printed `5` |
| 7. inspect | `bunker info <id>`, `bunker env <id>` | OK, 0.38 s / 0.01 s |
| 8. get tools | `bunker agent-tools <id> --install` | **partial**: toolsd + ripgrep delivered, gopls failed (row `DOGFOOD-016`) |
| 9. tools in one step | `bunker spawn --tools-at-spawn` | **FAILED twice**, killed at 300 s (row `DOGFOOD-015`) |
| 10. reap | `bunker reap` (report-only) | OK |

Tool census *inside* a plain-spawned agent: present `git node npm python3 pip3 docker`; missing
`rg toolsd go make gcc jq`. That contradicts GAP-B2's PASS criterion and is row `DOGFOOD-017`.

## Install leg — PROVEN, not skipped

Run inside the ephemeral agent container (fresh user, fresh filesystem, no repo checkout, no
credentials minted), against the published release artifact:

```
$ bunker exec b32fdf11 -- 'curl -fsSL https://github.com/deployBunker/bunker/releases/latest/download/install.sh -o /tmp/install.sh'
CURL_RC=0  elapsed=1s  size=33108 bytes

$ bunker exec b32fdf11 -- 'sh /tmp/install.sh; export PATH=$HOME/.local/bin:$PATH; bunker --version'
/home/bunker-b32fdf11/.local/bin/bunker
bunker 0.3.0
  commit:     de286fdc
  built:      2026-11-09T05:16:34Z
  go version: go1.26.10
  platform:   linux/amd64
sha256 /tmp/install.sh = 96fd9ba25c8ca501f67dfca05406b7cfa78e7f1a04bde15e6215f030b605a930
```

So: installability from the release artifact is **verified end to end on a machine that did not
have bunker**. This is not `SKIPPED-install-bunker`; no skip row is owed. The installed release is
dated 2026-11-09 while the tree under test is 2026-11-12 — expected lag, not a defect.

## The P1, in the daemon's own words

`bunker spawn --ttl 30m --tools-at-spawn`, client side, twice:

```
Creating agent...
exit=124 after 300s      (attempt 1)
exit=124 after 150s      (attempt 2)
```

Nothing else is printed — no stage, no progress, no error. Daemon side, same window:

- attempt 1, stage **image-build**: rootless `docker build` of the tool image (apt `ripgrep`,
  pinned `golang.org/x/tools/gopls`), killed by the daemon's own 300 s request timeout
  (`context canceled`), agent rolled back.
- attempt 2, stage **rootless-install**: the rootless docker static bundle (82.3 MB) plus 11.4 MB of
  extras downloaded at ~500 KB/s for **2 m 42 s**, then killed at the same 300 s budget.

Two unbounded stages share one fixed 300 s server-side budget and the client is told nothing about
either. A plain spawn on the same host takes 19 s, so this is specific to the flagged path.

## Numbers

| operation | runs (ms) | note |
|---|---|---|
| `exec 'echo hi'` warm | 513 / 499 / 500 | in-process SSH client |
| `info` | 396 / 363 | control RPC |
| `env` | 12 / 10 | local config |
| `cp` 60 B warm | 4127 / 3937 | spawns `scp` |
| raw `scp` same file, agent key | 2243 / 2233 | transport floor |
| raw `ssh 'echo hi'` | 1327 / 1329 | WAN round trip |
| `spawn` (plain) | 19 s | second attempt |
| `spawn --tools-at-spawn` | ≥300 s ×2 | never completed |

`cp` decomposition: 0.38 s control RPC + 2.24 s for the spawned `scp` child's own WAN handshake
+ ~1.3 s of client work. The cost is the subprocess and its second handshake, not the WAN — `exec`
does the same round trip in 0.50 s. Hot path: `internal/cli/cp.go:195`, with the same pattern at
`internal/cli/deploy.go:174` and `internal/cli/pull.go:158` (`PERF-020`).

Judgement on what is *felt*: `exec` 0.5 s, `info` 0.4 s and `env` 0.01 s are not user-noticeable
and got no row. A single 4 s copy is borderline; five small files in a loop (~20 s against a ~3 s
floor) is not, and `deploy` inherits the pattern — that is why `PERF-020` exists at P3, not P1.
`spawn --tools-at-spawn` is a hard stop for the whole workflow, so it is P1.

## What was not done

No source code was modified. No board row was closed or re-statused; the four rows above are new.
No visibility or permission change. No credentials minted. The probe binary and logs live outside
the repo in the tick's scratch directory.
