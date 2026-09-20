# Dogfood run 2026-09-20 — agent isolation, and the mount command that never worked

**Target:** `bunker` at `/home/kara/bunker`, CLI HEAD `93d7a53` (v0.1.4, built with `make build`)
**Live hosts:** `bunker-las-03` (daemon 0.1.4 `6a6ad20`, reports `isolation-grant`) as the work host; read-only
probes against las-01/02/04/mvp/cube-las-00.
**Verdict:** 🔴 **DOES-NOT-DELIVER** — one documented flagship feature is 100% broken at HEAD, one documented
isolation feature is unusable as shipped, and the install leg passed. See §7 for the split verdict.
**Angle (14th run in the series):** prior runs covered the CLI lifecycle, the REST protocol, fresh-machine
install and the durability/trust surface. This run took the **agent isolation boundary** — the `/tmp` promises
and the cross-agent exchange point — plus the one flagship command those docs lean on, `bunker mount`.

---

## 1. The promise (null hypothesis)

From README §Agent isolation (`/tmp` and cross-agent exchange) and `specs/agent-tmp-isolation.md`:

> After host provisioning and starting a matching current daemon, agent execution paths have an **enforced
> private `/tmp`** … Cross-agent file exchange is **opt-in and explicitly bounded**: the only sanctioned shared
> location is `/srv/bunker-share`, whose root is root-owned, setgid to the agent group `bunker-agents` and NOT
> writable by it or by the world (mode `2750`) … where the daemon creates one directory per agent with the
> setgid bit set to the agent group and a kernel-enforced per-agent size cap (a tmpfs).

```bash
# agent A hands a file to agent B   (README, verbatim)
bunker exec a -- sh -c 'cp build.tar /srv/bunker-share/a/build.tar'
bunker exec b -- cp /srv/bunker-share/a/build.tar ./build.tar
```

And README §Features:

> **SSHFS native mount** — Mount any agent's filesystem locally: `bunker mount <id> /mnt/agent`

**So the claim is:** a user can isolate agents from each other and from the host, hand files between exactly
two agents through one bounded directory, and mount any agent's filesystem locally.

## 2. What actually held

Two full agents (`df1017a`, `df1017b`) plus a third (`df1017c`) and a fourth (`df1017u`) on `bunker-las-03`.
Spawn 40s / 29s / ~35s / ~40s; `bunker exec` into every one worked first try.

| Check | Result |
|---|---|
| spawn → exec → destroy lifecycle | ✅ works, ~40s cold |
| agent file isolation within a session | ✅ B's `cat /tmp/df1017-secret.txt` (A's file) → Permission denied |
| cross-agent /tmp mutation | ✅ refused (`cannot create ... Permission denied`); A re-reads its own value intact |
| **detached unit gets its own /tmp (G3)** | ✅ **CONFIRMED** — `run --detach` sees `ls /tmp` = **1 entry** and *cannot see the host marker the ssh session could read* |
| per-agent scratch dir shape | ✅ `drwxrws--- agent:bunker-agents`, tmpfs `size=262144k` |
| scratch cap enforcement | ✅ writing 300 MiB into a 256 MiB agent dir = exactly 268435456 bytes, `df` 100% full |
| stop / start lifecycle | ✅ stop returns the CPU, keeps user/home/env/cp payload/`/tmp`; `exec` while stopped fails with the documented `agent_stopped` token |
| audit attribution | ✅ `bunker audit list --server` shows `Caller=master` + `Agent=<id>` per record, `--agent` filter works; `not_found` calls are recorded |
| metrics existence check (DF-BUNKER-28) | ✅ **FIXED** — `metrics/info/heartbeat` on a never-spawned id all return `not_found`, no fabricated record |
| fresh-machine install (one-command) | ✅ release installer: download → SHA256 verify both binaries → install → smoke. No sudo, no `make` |
| fresh-machine install (source) | ✅ clone → `make build` refuses cleanly with recovery steps → Go 1.26.5 → `make build` 49s → `--version` OK |
| destroy hygiene | ✅ all four agents destroyed, zero users/homes/keys/mounts left, no repo permissions touched |

**The transport is fine.** With `BUNKER_SKIP_MOUNT_PREFLIGHT=1` and an explicit `--path`, `bunker mount` mounts,
`ls` lists the real agent home, a `md5sum` matches through the mount, and a write through the mount is readable
inside the agent by `bunker exec`. sshfs, the key rewrite, the option filtering — all working.

## 3. Finding 1 (P0) — `bunker mount` cannot mount anything

**Every** documented mount form fails:

```
$ bunker mount df1017b                                          # documented default
bunker: mount preflight: no remote path to check                 (rc=1)
$ bunker mount df1017b /tmp/df1017/mnt4
bunker: mount preflight: no remote path to check                 (rc=1)
$ bunker mount df1017b /tmp/x --path /home/bunker-df1017b        # agent home, proven to exist
bunker: mount preflight: remote path "/home/bunker-df1017b" does not exist or is not a directory  (rc=1)
```

The path exists — same agent, same key, same second:

```
$ printf 'test -d /home/bunker-df1017b && echo DIR_OK' | ssh -i ~/.bunker/keys/df1017b bunker-df1017b@... 
DIR_OK
$ bunker exec df1017a -- ls -la /home/bunker-df1017a/dep
drwxrwxr-x ... a.txt  b.txt
```

### Root cause, isolated

`internal/cli/mount_preflight.go:441`:

```go
func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) (string, error) {
	done := make(chan result, 1)
	if err := cmd.Start(); err != nil { return "", err }
	go func() {
		out, err := cmd.CombinedOutput()   // <-- CombinedOutput calls Start() itself
		done <- result{out, err}
	}()
	...
```

`CombinedOutput` on an already-`Start`ed command returns `exec: already started` **immediately**, so `out` is
empty and `err` is non-nil. The caller's `details == ""` branch then reports "does not exist or is not a
directory". A deliberate `ssh` shim on `PATH` logging argv/stdin/exit proves the CLI execs ssh with the correct
argv and an **empty stdin** — the probe script is never delivered. A 20-line Go program mirroring the function
reproduces it exactly:

```
Start+CombinedOutput (the shipped code path): out="" err=exec: already started
CombinedOutput only (correct usage):          out="echo hello-from-stdin\n__BUNKER_DIR__\n" err=<nil>
```

### Why the suite is green

Both tests route around the broken function by design:
`mount_test.go:230` replaces `remotePathCheck` with a stub ("the mount preflight shells out to a real host;
stub it here"), and `proc_lifecycle_test.go:120` runs the CLI with `BUNKER_SKIP_MOUNT_PREFLIGHT=1`. The real
function has zero test coverage. The gated live proof for this area, `.coding-hermes/evidence/gap112-live/
session.log`, also shows `mount rc=1` and a failed baseline write — GAP-112 ("MOUNT LIVE PROOF") never reached
a working mount either.

### Same bug, second command

`internal/cli/umount.go:209-241` uses the same `runWithTimeout` for `fusermount3` / `umount`:

```
$ bunker umount /tmp/df1017/mntU
bunker: unmount /tmp/df1017/mntU failed (normal: exec: already started; lazy: exec: already started, output: )  (rc=1)
```

The kernel unmount still happens as a side effect of teardown, so this reads like a cosmetic warning — but the
CLI never sees the tool's exit status, and a genuinely busy mountpoint would be reported as
"exec: already started".

## 4. Finding 2 (P1) — the shared scratch exchange point is unreachable

`/srv/bunker-share` on `bunker-las-03`, measured as root:

```
root:root        drwxr-x---   750            <-- the exchange ROOT
bunker-df1017a:bunker-agents  drwxrws--- 2770 tmpfs size=262144k
bunker-df1017b:bunker-agents  drwxrws--- 2770 tmpfs size=262144k
```

The per-agent directories match the spec exactly. The root does not (spec: `2750 root:bunker-agents`). A mode
`750 root:root` directory is not traversable by an agent, so every documented exchange command fails:

```
$ bunker exec df1017c -- sh -c 'ls /srv/bunker-share'
ls: cannot open directory '/srv/bunker-share': Permission denied
$ bunker exec df1017c -- sh -c 'cp /etc/hostname /srv/bunker-share/df1017c/build.tar'
cp: cannot stat '/srv/bunker-share/df1017c/build.tar': Permission denied
```

…while the daemon logged, for every spawn, `"msg":"shared scratch ready"`. Three separate agents reproduced it.

**Mechanism.** `EnsureAgentScratch` (the per-spawn path, `internal/agent/isolation.go:209`) creates the agent
directory under whatever root exists. The function that creates the root correctly —
`EnsureSharedScratch` (chown root:group, chmod 2750, **stat-back as a hard error**) — is called from exactly one
place: `internal/hostsetup/status.go:219`, i.e. `bunker host-provision --apply`. The per-spawn path never calls
it, and the comment on `EnsureAgentScratch` claims a root it does not provision.

**Fleet control.** The correct layout exists **only** on `bunker-mvp` (`2750 root:bunker-agents`). It is wrong
on `bunker-las-01` (`750 root:root`) and the directory does not exist at all on `bunker-las-02` and
`bunker-las-04`. So on most of the fleet the exchange point is inert, silently.

## 5. Findings 3-5 (P2) — flag grammar and exit codes

* **`run` never got the DF-BUNKER-31 treatment.** `bunker run --server X <id> -- cmd` → `no target bound`
  (rc=1); `bunker --server X run <id> -- cmd` → same; `bunker run <id> --server X -- cmd` → works. The
  unification commit (`5b72ceb`) covered `exec` only.
* **`env set/get` accept `--server` only before the subcommand.** `env set <id> K=V --server X` and
  `env set --server X <id> K=V` both fail with `requires exactly one KEY=VALUE argument` — the flag is consumed
  as the positional. Only `bunker env --server X set <id> K=V` works, the one position the README never shows.
  Verified against the agent: `K1=v1 K2= K3=`.
* **Failures exit 0 when stdout is piped.** `bunker audit list --server X` prints
  `bunker: open /var/log/bunkerd/audit.log: permission denied`, and reports exit 0; so does
  `bunker status --json` (unknown flag). README §Exit codes promises `1`. This harness was fooled by it
  (`| head -8` → `RC=0` next to the error line). The audit `--server` also silently fell back to the local
  path instead of querying the daemon, per a second message inconsistency in the same area.

## 6. Install leg — fresh machine, proven

Fresh state: an ephemeral agent on `bunker-las-03` (`bunker-df1017i`, bare Debian 13, non-root, **no Go, no
sshfs**, `sudo` password-locked), reached with the agent's own key.

* **One-command installer (documented Option 1)**: `curl … /install.sh | sh` → downloaded both linux/amd64
  binaries, **verified both against the release `SHA256SUMS` before writing anything**, fell back to
  `~/.local/bin` because `/usr/local/bin` is not writable (no sudo escalation), smoke-checked
  `bunker --version` → `0.1.4`. **PASS.** `--dry-run` prints the plan and writes nothing. **PASS.**
* **Source path (documented Option 2)**: `git clone https://github.com/deployBunker/bunker.git` → 2s, HEAD
  `93d7a53`. `make build` without Go → the `check-go` guard refuses with the README section, the tarball URL
  and the `$HOME` extraction pitfall — **not** `sh: 1: go: not found`. **PASS.** Go 1.26.5 tarball into a
  user-writable prefix (the README's `/usr/local` route needs sudo, unavailable) → `make build` → `bunker
  0.1.4 commit 93d7a53`. **PASS.** `bunkerd --version` reports `caps: isolation-grant`. **PASS.**
* **`scripts/install.sh --build`** (the documented make-free path): builds, installs, smoke-checks. **PASS** —
  but note it printed `bunker 1.26.5` as the version (see finding in the log: `bd_version` falls through to
  `GO_FALLBACK` when the checkout has no tags, so the binary advertises the Go version as its own — cosmetic,
  filed as an observation, not a row).
* **Daemon as a non-root user** behaves exactly as documented: starts, answers reads, logs
  `agent registry unavailable … permission denied`. Matches the README's "spawn needs root" section.

**Install leg verdict: PASS.** Fresh-machine installability is proven for both documented paths; the
documented failure modes are the ones the docs describe. `bunker-df1017i` destroyed afterwards.

## 7. Verdict, and how to read it

| Surface | Verdict |
|---|---|
| fresh-machine install (both documented paths) | ✅ SHIPPABLE |
| agent lifecycle (spawn/exec/env/cp/deploy/stop/start/heartbeat/destroy) | ✅ SHIPPABLE |
| isolation: private `/tmp` for sessions and detached units | ✅ SHIPPABLE — G3 confirmed live for the first time |
| isolation: cross-agent exchange point | 🔴 DOES-NOT-DELIVER on every host but bunker-mvp |
| `bunker mount` (documented flagship) | 🔴 DOES-NOT-DELIVER — 100% broken at HEAD |
| CLI flag grammar / exit codes | 🟡 PROMISING-BUT-ROUGH — a family of traps around `--server` placement |

**Overall: 🔴 DOES-NOT-DELIVER at HEAD** — driven by the two 🔴 cells, both of which are a documented promise
failing on the happy path, and one of which (`mount`) has zero effective test coverage so the suite stays green
while it is broken.

**Time-to-first-success:** ~40s (spawn → `exec` returns 42). **Friction count for the whole run: 14**
(4 mount-form failures, umount exit 1, 3 `run --server` refusals, 3 `env` arity refusals, 2 piped-exit-0 traps,
1 scratch EACCES family). Rows filed: `DF-BUNKER-38` … `DF-BUNKER-43` (6).

## 8. The one-hour fix list

1. **`DF-BUNKER-38` (P0)** — delete the `cmd.Start()`/`CombinedOutput()` double-start in `runWithTimeout`, and
   add a test that runs `remotePathExists` against a fake `ssh` on `PATH` (the function's only external
   dependency is ssh — there is no reason for it to be untested).
2. **`DF-BUNKER-39` (P1)** — resolve the default remote path *before* the preflight so the documented
   `bunker mount <id>` form can work.
3. **`DF-BUNKER-40` (P1)** — make the spawn path either provision the exchange root itself or fail the scratch
   step loudly; a scratch the agent cannot traverse is not "ready".
4. **`DF-BUNKER-41/42` (P2)** — extend the DF-BUNKER-31 unification to `run` and `env`, and make the arity
   errors name the arguments they saw.
5. **`DF-BUNKER-43` (P2)** — fix the piped exit-0 paths so scripted use can trust the status.

## 9. Reproduce this run

```bash
cd ~/bunker && make build                 # HEAD 93d7a53

bunker spawn --server bunker-las-03 --ttl 1h dfprobe
bunker exec  --server bunker-las-03 dfprobe -- sh -c 'echo secret > /tmp/s.txt; id; ls -l /tmp/s.txt'

# finding 1 — mount, all four documented forms
bunker mount dfprobe                                                     # 'no remote path to check'
bunker mount dfprobe /tmp/m --path /home/bunker-dfprobe                  # 'does not exist or is not a directory'
ssh -i ~/.bunker/keys/dfprobe bunker-dfprobe@100.69.3.13 'test -d $HOME && echo DIR_OK'   # ...but it does
BUNKER_SKIP_MOUNT_PREFLIGHT=1 bunker mount dfprobe /tmp/m --path /home/bunker-dfprobe     # mounts fine

# finding 2 — the readme's own cross-agent exchange example
bunker exec --server bunker-las-03 dfprobe -- sh -c 'cp /etc/hostname /srv/bunker-share/$(id -un)/x'
ssh bunker3-root 'stat -c "%a %U:%G" /srv/bunker-share'

bunker destroy --server bunker-las-03 dfprobe
```

Evidence trail for this run (control host, not committed): `/tmp/df1017/iso/*.log`,
`/tmp/df1017/install.log`, `/tmp/df1017/shim/ssh-calls.log`.
