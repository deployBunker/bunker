# PERF-020 — `bunker cp` loses the scp child: measured run, 2026-11-22

Worker run: PERF-020, worktree `/home/kara/worktrees/bunker-PERF-020` (branch `wt/PERF-020`).
Server: `bunker-las-03` (https://100.69.3.13:10001). Agent: `6e024cd4` (plain spawn, `--ttl 4h`).
Builds: `go build -o /tmp/bunker-perf020 ./cmd/bunker` (before) and
`/tmp/bunker-perf020-after` (after) from this worktree. Host clock during measurement:
Sun Nov 22 2026 ≈ 19:00 -05. All runs serial; 3 warm + 3 cold per command; the mux
ControlPath socket for exactly this agent ([sha256(user|host|port|key)]) was removed
before each cold run.

## Agent provenance deviation (disclosed)

`bunker list --server bunker-las-03 --status all` returned **no agents** and the server
reported `Agents: 0/8` — the live agent the row assumed does not exist. The battery needs a
live agent, so ONE plain agent (`6e024cd4`, `--ttl 4h`) was spawned on the same server the
2026-11-12 dogfood used, with the same plain-spawn shape. The spawn itself failed once at
`slice-limits` (`memory.swap.max = "max", want "0"`), then landed on the retry — the same
first-attempt/retry pattern DOGFOOD-015 recorded. Nothing else about the host was touched.

## The brief's premise, corrected by measurement

The row's premise — "the CLI already talks SSH in-process elsewhere (internal/cli/ssh.go,
sshhost.go — the exec path)" — is not what the tree does:

1. `bunker exec` does not use a client-side SSH connection at all; it streams through the
   daemon's `ExecAgent` RPC. The connection to the agent is made ON the server host
   (`internal/server/service.go`: the daemon builds `ssh bunker-<id>@localhost …`).
2. There is no in-process SSH client anywhere in the CLI (`grep -r crypto/ssh internal/ cmd/`
   matches nothing; `golang.org/x/crypto` is an indirect dep).
3. An in-process SSH client was measured anyway (scratch probe, x/crypto/ssh, same 60-byte
   file, 3 runs): dial 0.17–0.21s + handshake 1.14–1.33s + write/fixup session 0.57–0.59s ≈
   **1.9–2.1s**, and the GetAgent lookup (0.41s, needed to resolve the agent) cannot be
   removed because its result determines the dial target. That misses the acceptance gate
   (≤1.5s warm) no matter how the dial is overlapped.
4. Therefore the fix that meets the gate is the one already measured fast: stream the file
   through the daemon's `ExecAgent` stream with the bytes as the command's stdin
   (GAP-094) — no client-side SSH connection at all, and the chown rides the same remote
   shell.

## Table (same file, same protocol as 2026-11-12; `bunker info` control = 0.40–0.42s)

| operation | runs before (s) | runs after (s) | note |
|---|---|---|---|
| info (control) | 0.41/0.42/0.42 | 0.42/0.41/0.41 | RPC control |
| exec warm | 1.32/1.40/0.58 | 0.60/0.86/0.55 | via daemon RPC (after-run host was briefly busy; warm rows 0.55–0.60) |
| **cp 60B warm** | **4.44/4.42/4.36** | **0.68/0.69/0.74** | in-process daemon stream |
| cp 60B cold | 4.49/4.41/4.43 | 0.81/0.91/0.94 | mux socket removed |
| cp 2MB one-off | — | 5.55 | correctness check (byte-identical md5) |
| deploy warm | 2.37/2.48/2.72 | 2.52/2.54/2.85 | unchanged (scp child path kept, as scoped) |
| deploy cold | 5.21/6.55/5.20 | 4.97/5.09/9.11¹ | ¹ host-load spike on the third run |
| pull warm | 3.64/3.86/3.12 | 3.13/3.16/3.31 | unchanged |
| pull cold | 3.15/3.26/3.24 | 3.05/3.18/4.06 | unchanged |
| raw scp 60B (ref) | 2.61/2.92 | 3.02/4.09 | transport floor, fresh handshake |
| raw ssh echo (ref) | 1.58 | 1.77 | fresh handshake |

Acceptance: a 60-byte `bunker cp` lands at **0.68–0.74s warm** (before: 4.36–4.49s;
2026-11-12 dogfood: 3.9–4.1s) — at/below the 1.5s gate with margin. The before/after
decomposition: cp warm was `info` RPC (0.42) + mux-reused scp child (~1.5) + a second FULL
handshake for the chown child (~2.4, cp's chown child lacked the mux triple deploy has).
Both handshakes are gone by default.

## What changed

`internal/cli/cp.go` + new `internal/cli/cp_daemon.go`:

- Default (`--ssh-host/--ssh-port/--ssh-key` unset, file ≤ 64 MiB): the transfer is one
  `ExecAgent` stream — request `Command: "sh", Args: ["-c", script]`,
  `StdinPayload: <file bytes>` — where script = `cat > <path> || exit 2; chmod <source
  mode> <path> || exit 3; chown "$(id -u):$(id -g)" <path> || exit 4`. The chown still
  happens (AC: "in-process is fine"); numeric `id -u/:id -g` is correct for both the host
  context and an image-backed agent's container (where the `bunker-<id>` NAME may not
  resolve). Note: image-backed agents were not live-tested (no image agent exists on
  bunker-las-03); the reasoning above is why the numeric chown was chosen. The daemon's
  exec runs as the agent user in both contexts (verified live: `id` → uid
  1002(bunker-6e024cd4)).
- The mode is set from the source file's mode, which is scp's own contract (verified:
  a local 664 source lands 664, exactly as the scp path behaved).
- scp stays the transport **verbatim** when any SSH identity flag is set (the daemon
  cannot honor a `--ssh-host`/`--ssh-port` override, and `--ssh-key` names the secret the
  connection must use) or the file exceeds `maxInprocFileBytes` (64 MiB — the stream
  payload rides one request message; scp streams from disk, so the memory-for-speed trade
  is only taken below that line).
- The fallback's `ssh` chown child gained the mux triple cp's scp run already established
  (deploy.go does this; the missing triple was cp's second-full-handshake cost).
- Failure paths unchanged and still exercised on BOTH paths: GAP-144 partial-transfer
  reporting and the DF-BUNKER-17 ownership hint run the same bounded ssh probes with the
  same identity; the signal-aware child context (proc.go) governs the stream exactly as it
  governed the scp child (no child process exists to outlive the CLI on the fast path).
- ExecAgent has no configured byte limit on client or server (connect-go defaults = 0 =
  unlimited), so the 64 MiB cap is conservative, not required.

## Failure modes (AC 4) — transcripts byte-compared

Missing local path (identical, both run the local preflight before any transport):

```
bunker: local file "/nonexistent/file-020": stat /nonexistent/file-020: no such file or directory
```

Missing remote directory (fast path; byte-identical after the errno-dialect fix below):

```
scp: dest open "/nonexistent-dir-020/x": No such file or directory
scp: failed to upload file /home/kara/.hermes/cache/scratch/p020/tiny.txt to /nonexistent-dir-020/x
scp failed: 0 of 1 files landed, destination may be incomplete: /nonexistent-dir-020/x — remove it or re-run the copy before using it
bunker: scp: exit status 1
```
(exit code 1 before and after; diff = empty.)

Unreachable server (`--ssh-host 127.0.0.1 --ssh-port 1`; forced onto the scp fallback
path, so the transcript is scp's own output verbatim — identical before/after):

```
ssh: connect to host 127.0.0.1 port 1: Connection refused
scp: Connection closed
scp failed: destination /tmp/p020-x may be incomplete — the landed file count could not be measured (exit status 255) — remove it or re-run the copy before using it
bunker: scp: exit status 255
```
(exit code 1 before and after.)

Two deliberate, disclosed reconstructions make the dest-open transcript byte-identical:

- The daemon-side agent shell reports the same ENOENT in a different dialect — measured:
  "Directory nonexistent" where scp's sftp transport said "No such file or directory".
  `scpErrnoVocabulary` maps known errno synonyms onto scp's exact words (unknown phrases
  stay verbatim) and the two re-emitted lines copy the scp child's `\r\n`
  line terminator (observed in the before transcript).
- The exit-code contract is re-stated rather than re-derived: a failed remote write is
  scp's `exit status 1`, an unreachable transport would be 255 — as before.

A failure mode that IS textually different (disclosed): with the daemon up but the agent's
own sshd down, today's "scp dial refused" becomes the daemon's exec error wrapped
`scp: …` — same non-zero exit, same GAP-144 report + hint pipeline, different words for a
failure class the graded three do not cover.

## Gates

- `gofmt -l internal/cli/` → empty (repo-wide flag on `internal/audit/interceptor.go` is
  pre-existing debt, not touched; `git status` shows no modification to it).
- `go build ./...` / `go vet ./...` → clean.
- `go test ./internal/cli/ -count=1` → ok (45s; includes 6 new PERF-020 tests
  + `TestCpCommand_SIGTERMReapsSCPChild` re-run on the scp fallback path).
- `go test ./... -count=1` → all packages ok.
- Adapted tests: `TestCpCommand_ScpFailureOwnershipHint`,
  `TestCpCommand_ScpInvocationCarriesIdentitiesOnly`, `TestCpCommand_SIGTERMReapsSCPChild`
  now pass `--ssh-key` (the fixture's own default), because their subject is the scp child
  itself, which is no longer the default transport; the argv/probe-identity assertions are
  unchanged.
- New tests: routing (`cpShouldStreamViaDaemon`), remote-script contract, errno phrase +
  scp vocabulary, and six end-to-end wirings against a stub daemon (success carries the
  payload + chmod + chown, dest-open failure prints scp's lines, transport failure keeps
  the GAP-144 report, explicit flags keep scp, default path never execs scp).

## What was NOT done

- deploy/pull keep their scp children (out of scope unless FREE; measured within noise).
- No board files were touched. No other agents were touched; the spawned agent stays under
  TTL. Live probe artifacts live under the session's scratch (before/ + after/ + after2/).
