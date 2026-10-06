# Program aliases — docker-as-installer (GAP-066)

An agent host has no compilers and no apt rights. A **program alias** lets an
agent *run* a program it never installed: the daemon maps a program name onto a
container image, pulls the image on first use, and runs the program inside a
fresh container.

```
bunker alias set yq --image mikefarah/yq:4
bunker run <agent-id> -- yq --version
```

The agent never learns where the program came from — stdout, stderr and the exit
code are the container program's own, so a script that calls `yq` cannot tell the
difference between the alias and a natively installed binary.

---

## Commands

| Command | What it does |
|---|---|
| `bunker alias list` | List every registered alias (name, image, entrypoint, mounts, network). |
| `bunker alias set <name> --image <ref>` | Create or update an alias. `--entrypoint` (repeatable or comma-separated) is the container's entrypoint program; `--mount host[:container][:ro]` (repeatable) adds an extra bind mount; `--network` gives the container egress; `--description` is an operator note. |
| `bunker alias delete <name>` | Remove an alias. A name that was never registered is `not found`, not a silent success. |

All three are **master-credential** operations: they sit on the `Bunkerd`
service, so an agent-scoped sub-key is rejected (`unauthenticated`).

### Flags

| Flag | Meaning |
|---|---|
| `--image <ref>` | Required. The container image the program runs out of. Pulled by the daemon on first use. |
| `--entrypoint <tok>` | Docker's own `--entrypoint`: the program the container enters through. `Entrypoint[0]` becomes `--entrypoint <it>` and any further elements are the program's leading arguments (`docker run --entrypoint <ep0> … <image> <ep1…> <caller args…>`). Leave it EMPTY to use the image's own `ENTRYPOINT`/`CMD` — the usual choice for a tool image like `mikefarah/yq`, whose entrypoint is already `yq`. Set it when the image has no entrypoint of its own (`--entrypoint go` for `golang:1.26-alpine`) or to override one (`--entrypoint sh` on the yq image if you want a shell). |
| `--mount <spec>` | Extra bind mount, `host[:container][:ro]`. **`host` must be inside an agent home** (`/home/bunker-<id>/…`): a path outside that root can never be valid and is refused at registration; a path inside a *different* agent's home is stored but refused when the alias is run. Empty container = same absolute path as the host. |
| `--network` | Give the container network access. Default (absent) is `--network none`. |
| `--description <text>` | Operator-facing note shown by `alias list`. |
| `--server <alias>` | Target server. Falls back to `BUNKER_SESSION_TARGET`; mutating verbs never silently use the shared active default. |

---

## How a run works

1. **Resolve.** `bunkerd` looks the command token up in its alias store
   (`/var/lib/bunkerd/program-aliases.json` by default, next to
   `agents.jsonl`; relocate with `BUNKERD_PROGRAM_ALIASES_PATH`).
   Only a **bare program name** is considered — a command containing `/` is the
   caller explicitly asking for a native binary, and a command carrying
   punctuation that is not legal in an alias name is not an alias at all.
   `/usr/bin/yq` therefore always runs the native binary, even when `yq` is an
   alias.
2. **Make the image present.** The daemon runs `docker image inspect` against the
   agent's own rootless socket; if the image is missing it runs `docker pull`
   and logs how long the pull cost. The agent's own command line never carries a
   pull, and once this daemon process has confirmed an image, repeat runs skip
   the round trip entirely.
3. **Install the shim and run it.** The daemon materialises the alias at
   `<agent home>/bin/<name>` — a small POSIX shell script that `exec`s the docker
   argv — and then `exec`s that same script with the caller's arguments. Because
   both the daemon-mediated invocation and a script inside the agent run *the
   same file*, their output and exit status cannot drift apart.

### What the container gets

```
docker run --rm -i --user 0 \
  --memory <agent memory_max_bytes, default 2 GiB> \
  --cpus <agent cpu_quota, default 2> \
  --pids-limit 512 \
  --network none \
  [--entrypoint <program>] \
  -v <home>:<home> -w <home> \
  [--mount <validated extra mounts>] \
  <image> [<entrypoint args…>] <args...>
```

* `--user 0` is the agent's identity **inside the agent's own rootless user
  namespace**, where the agent *is* uid 0 (the same identity the image-spec exec
  path uses, DF-BUNKER-77). Files the container writes into the home are owned by
  the agent — native-run parity.
* Resource limits come from the **agent's own spawn-time envelope**. An agent
  with no recorded envelope gets modest defaults, never "unlimited".
* The home is always mounted at its own absolute path and used as the working
  directory, so host-absolute paths stay valid in-container.

### What the container never gets

* **No docker socket.** The alias container is given no `/run/bunker/<id>` bind,
  no `/var/run/docker.sock`, and no `--privileged` / `--pid host`. The existing
  image-spec exec path mounts the agent runtime directory (which carries
  `docker.sock`) so an in-container `docker` CLI can reach the agent's own
  daemon; a program alias deliberately does not. The docker CLI that *starts*
  the container lives on the agent host under the daemon's own constructed argv,
  and nothing the alias names can change that.
* **No host path outside the agent's home.** Every extra mount is validated
  against the agent's home before any argv exists; a non-home mount is refused,
  as is any path naming a docker socket.
* **No agent-chosen image, mount, or limit.** All three are daemon-side.

---

## When to use an alias vs apt

| Use a **program alias** when | Use **apt / the host image** when |
|---|---|
| The tool is a CLI utility or toolchain an agent needs *sometimes* (`yq`, `jq`, a Go/Rust/Node toolchain). | The whole agent session depends on it before any exec runs (shells, `sshd`, PAM artifacts, cron). |
| You want it versioned and upgraded by changing **one registry entry** instead of rebuilding the host image. | The daemon itself needs it in order to spawn the agent at all. |
| You want the exact same tool available to every agent on the fleet without per-host drift. | It is host plumbing (systemd units, mount helpers, the container runtime). |
| You want the tool to run with the agent's home and no network by default. | The tool must run *as the host* by design. |

A program alias **never replaces a host package**: it is per-invocation, it runs
in a container with the agent's home and no network by default, and it cannot
touch a host path outside that home. If a command must exist for the agent to
boot, install it on the host.

### Worked example — a toolchain

Fetching modules needs egress, so opt in explicitly. `golang:1.26-alpine` has no
entrypoint of its own, so `--entrypoint go` is what turns the image into the `go`
program:

```
bunker alias set go --image golang:1.26-alpine --entrypoint go --network \
  --description "Go toolchain (needs network for module fetch)"
bunker run <agent-id> -- go version
```

### Worked example — a tool with a read-only mount

```
bunker alias set yq --image mikefarah/yq:4 \
  --mount /home/bunker-<id>/datasets:/home/bunker-<id>/datasets:ro
```

The mount is accepted because it is inside an agent home. `--mount /etc` is
refused at registration (`mount host path must be inside an agent home
(/home/bunker-<id>)`), and a mount inside *another* agent's home is refused when
the alias runs (`mount host path must be inside the agent's home directory`) —
the rule is enforced twice, once where the operator can see it and once against
the agent that is actually about to run.

---

## Using an alias from a script inside an agent

The shim is installed on the first alias run, and `<agent home>/bin` is first on
the exec `PATH`, so anything running inside the agent can call the program:

```bash
bunker alias set yq --image mikefarah/yq:4
bunker run <agent-id> -- yq --version          # installs the shim, runs it
printf 'yq --version\n' | bunker exec --script <agent-id> -
```

Both invocations run the same shim file, so their exit codes and stdout match.

`--raw` execs resolve the alias too, but keep raw mode's no-shell contract: the
docker argv is passed verbatim and no shim is installed.

---

## Notes and limits

* **Scope.** Aliases resolve in the `ExecAgent` path (`bunker exec`,
  synchronous `bunker run`). A `--detach` run — a systemd transient unit — is
  not alias-resolved today; run the program through `bunker run` (synchronous)
  or install it on the host.
* **Script execs.** A `--script` upload has no single command token, so it is
  never alias-resolved. The shim is what makes the program available to it.
* **Image freshness.** The daemon caches "this image is present" per process.
  Restart the daemon (or use a different tag) to re-resolve a moved tag. A failed
  pull is never cached.
* **A missing or failed image is a loud failure.** The exec returns an error
  naming the image instead of racing a container start against a half-finished
  pull.
* **`bunker alias list` reports the store path** so an operator can inspect or
  hand-edit the (versioned, pretty-printed, `0600`) document.
