# Dogfood run 27 — bunker-fs / WebDAV surface (2026-10-16)

First real use of the opt-in WebDAV client (`bunker fs`), the E-4 delegated op
catalogue, and `bunker iobattery` against a mount. Setup: HEAD CLI (d760923)
on the control host, scratch bunkerd (14f0ca5) on bunker-mvp with the WebDAV
surface enabled and FULL state isolation, reached through `ssh -L 18181`.

## How to use it for real (what worked)

1. Enable the surface on the daemon: `server.webdav_enabled: true` +
   `server.webdav_root: <absolute path>` (refused at load if relative/missing —
   verified in specs/configuration.md and honored).
2. Reach it: `ssh -f -N -L 18181:127.0.0.1:18180 <host>` (or the daemon's own
   listener address; Basic-auth password = the daemon token).
3. `bunker fs snapshot --url http://127.0.0.1:18181/dav --password <token>`
   — one server call, node count + call count printed. THE headline claim
   (1 call vs PROPFIND walk) is real: requests_total=1 in the output.
4. `bunker fs mount /mnt/x --url ... --password ...` — prints the banner and
   BLOCKS (it is the serve loop). Run it in background; a naive caller kills
   it at the tool timeout and the mount dies with it.
5. Reads through the mount are excellent once the snapshot is warm: ls/stat
   ~2.8ms, byte-identical file copies at 82KB and 287KB. Writes are
   WHOLE-FILE-ONLY: cp/tee-style publish works, shell redirection (`>`, `>>`)
   and git's chmod dance are refused EOPNOTSUPP. Plan workloads accordingly:
   generate content locally, publish as whole files.
6. `bunker fs status` reads the mount dir's status.json — transport verdict,
   cache accounting, conflict log. Healthy throughout this run, 0 refusals
   on a whole-file workload.
7. `bunker iobattery --target <dir>`: on local disk all 6 families run and
   the JSON report carries the repro command per family. Against a bunker-fs
   mount, 4 of 6 families error (whole-file write shape again); throughput
   and cpu_per_byte still return.

## The write-shape contract — what a mount can and cannot do (DF-BUNKER-100)

The one finding of this run that a READER of the docs could not have anticipated,
and the one that stopped the advertised workflow: **a bunker-fs mount publishes
whole files only, and git cannot run on it.** The mount's help text and its
moment-of-mount banner now say so, and so do `docs/mount-drivers.md` §4, the
BFS-005 spec §5.5 and the README's mount note. Contract as measured here:

| shape | measured verdict |
|---|---|
| whole-file publish (`cp`, `tee`, editor save) | **works** — 82 KB and 287 KB copies byte-identical on read-back |
| `>` / `>>` onto an existing path | **refused** `EOPNOTSUPP` (`Operation not supported`); status.json `write_shape` counts it, `last` names the path (`size=0`) |
| in-place edit through an open handle | **refused** `EOPNOTSUPP` |
| `chmod`/`chown`/`utimes` (what git does to its lockfiles) | **refused** `EOPNOTSUPP` — `git init` dies with `chmod on .git/config.lock failed: Operation not supported` |

**Remediation** (now stated by the CLI, the docs and every refusal message): run
git IN the agent, or clone there and publish results onto the mount as whole
files; for a git question with no working copy use the delegated verbs
(`bunker fs op`). Do not clone a repository onto the mount and expect to work in
it — the repo half of git lives on the mount, and the mount cannot serve it.

## The delegated op catalogue (E-4): what a released build actually serves

| op | live verdict |
|---|---|
| capabilities, events, snapshot | 200 ok |
| status, diff, rev-parse, ls-files | 501 capability_unavailable (slice C5) |
| watch | 501 (server-side watcher not in this deployment; poll via events) |

Refusal texts are excellent (named slice + remediation). Gap: no served-vs-
build matrix in help/docs (DF-BUNKER-103).

## The incident every scratch-daemon user must know (DF-BUNKER-102)

A second bunkerd on a host that already runs one MUST isolate state or it
replays the production registry and reconciles (destroys) live agents:

```yaml
agent:
  base_data_dir: <scratch>/base     # REQUIRED
  registry:
    enabled: true
    path: <scratch>/base/agents.jsonl  # REQUIRED
  ssh_dir: <scratch>/ssh
audit:
  path: <scratch>/audit/audit.log      # REQUIRED
```

`--config` alone changes ports and auth but NOT the compiled-default state
paths. This run's first attempt replayed /var/lib/bunkerd/agents.jsonl
(11497 events, 5 live agents) and began destroying a live agent within 60s.
Fix direction filed; until a boot-time refusal exists, treat the three keys
above as mandatory.

## Numbers (Step 2b)

- fs snapshot: 398.5ms ±1.1 warm (hyperfine --warmup 2 --runs 10), 392-587ms
  across tree churn; 91 nodes, 1 call.
- cached ls / cat: 2.8ms ±0.6.
- iobattery local NVMe control: throughput 4891-5392 MB/s, metadata 58-64k
  ops/s, IOPS 530-582k.
- iobattery through mount: throughput 3.09 MB/s (~1600x under local), rest
  error.

## Errors hit and their meaning

- `chmod ... Operation not supported` in git-on-mount → write-shape contract
  (whole-file only), DF-BUNKER-100.
- iobattery EIO on mount → same root cause, DF-BUNKER-101.
- First scratch daemon replaying production registry → missing isolation
  keys, DF-BUNKER-102.
- `fs mount` blocking after banner → serve-loop design; background it.
- fusermount3 -u "Device or resource busy" while a shell cwd sits in the
  mount → cd out first; killing the mount process leaves "Transport endpoint
  is not connected" until fusermount3 -u succeeds.

## Install leg (fresh machine proof)

Ephemeral agent df27-install on bunker-mvp (las-01..04 offline; mvp only).
Spawn 18.5s → https clone → `go build ./cmd/bunker` 7.2s total → `bunker
--version` self-reports 8e67672 → `fs --help` + `iobattery` smoke rc=0 →
destroyed, user + key verified absent. All other bunker hosts answered
nothing (tailscale offline) — leg ran on mvp, not the named las-bunker-03.
