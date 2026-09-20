# Both-ways: using a bunker mount AND the verb layer

One page, so you don't have to rediscover which side fits which job.

## The two ways

| | **Mount** (`bunker mount`) | **Verb layer** (`bunker exec`, and the file verbs as they land) |
|---|---|---|
| How it works | The agent's workspace appears as a local directory over SSHFS | RPC to the agent; every call is targeted and attributed |
| Best at | Reading, grepping, diffing, inspecting git history, small edits — highest fidelity because it **is** a filesystem | Writes that must be targeted; anything that must not be able to wander to another tree |
| Bad at | **Builds/tests** — the toolchain reads thousands of small files over SFTP; locally it is 10–100× slower and burns the very CPU offloading was meant to save | Bulk reading; output is capped and encoded, so a huge grep is a bad fit |
| Failure shape | A dropped transport becomes **visible at the next syscall** (bounded, named); nothing silently half-applies | Refusals are explicit (`no target bound: pass --server/--agent`) and binding is per-session |

## The one rule

**Builds are always remote.** Inside a mount this is enforced, not advised:
mount writes a marker at the mountpoint root, and the shell wrappers from
`eval "$(bunker guard install)"` refuse `go`/`make`/`cargo`/`npm`/… with a
message naming the remote command. Bypass with `command go …` if you truly know
better; `bunker umount` removes the marker, so a stale marker can never refuse
a legitimate build later.

## When to use which

- **Explore / inspect / review** → mount. `git log`, `grep`, reading a file — the mount is a filesystem, full stop.
- **Small, known edits** → either. Prefer the mount for interactive work; prefer a verb when the edit must be refused-on-unbound (scripts, automation).
- **Automation / scripts** → verbs. Binding is explicit per invocation, so a script cannot silently re-target whichever server a sibling session last `use`d.
- **Anything that compiles** → remote, via `bunker exec <agent-id> -- go build ./...` (the guard will say so if you forget).

## Durability guarantees (what happens when the network does something)

- A blip (< the alive window) is absorbed by `-o reconnect` — no operator action.
- A dead transport is **refused at write time** within the bounded window (never an indefinite hang, never a phantom success).
- A crashed mount leaves a stranded mountpoint that the next `bunker mount`/`bunker umount` clears automatically.
- Every mount **prints the workspace identity** (`<remote> [<branch> @ <head>]`); `--expect-workspace deployBunker/bunker` refuses to mount anything else, so aiming at the wrong tree is an error, not a silent surprise.

## Known limitations (documented, not hidden)

- Mount fidelity is verified against a live agent in the GAP-112 live battery; the divergence list lives with that evidence rather than here.
- Files cached by the kernel on a dead transport serve stale reads until the recovery window elapses — the write guard refuses first, so a stale read cannot mask a lost write.
- The verb layer's binary-safe reads and stdin (GAP-094) are **live in the CLI** (`--stdin`, `--base64`, `--exec-cap`); see Exec fidelity below.

## Mount paths and namespacing (GAP-113)

A mountpoint is `<root>/<server>/<agent-id>`, where root is `$BUNKER_MOUNT_ROOT`,
then `$XDG_RUNTIME_DIR/bunker/mnt`, then `~/.bunker/mnt`. The **server is part of
the path** deliberately: agent ids are unique per server, not globally, so two
bunkers each running an agent named `dev` would otherwise share one mountpoint —
the second mount would collide with the first server's live mount, and an
operator could read the wrong tree believing it was theirs.

- Ids containing `/`, `..` or control characters are reduced to a single safe
  path component, so an agent id can never escape the mount root.
- `bunker umount <agent-id>` searches the namespaced layout and the pre-GAP-113
  `<root>/<agent-id>` layout, so mounts created before this change still clean up.
- If the same agent id is mounted from **more than one server**, `umount` refuses
  and names both paths rather than guessing: silently unmounting the wrong tree
  is the failure the namespace exists to prevent. Pass the mountpoint explicitly
  to choose.

## Exec fidelity (GAP-094)

- `--stdin <file|->` pipes a payload (or bunker's own stdin) into the remote command; binary-safe via temp file, natural EOF.
- `--base64` returns stdout/stderr base64-encoded per frame so non-UTF8 bytes survive text-only transports.
- `--exec-cap N` lowers (never raises) the per-direction output cap; the FINAL frame carries a truncation notice naming the cap and the remedy (narrow the command, or fetch the file with `bunker cp`). Output is never silently dropped.
