# SPEC: agent tool delivery — getting the verb dependencies onto a bunker

**Status:** implemented (toolsd half) · **Date:** 2026-09-20 · **Author:** Hermes
**Scope:** how the executables the remote editing verbs call get onto an agent.
**Related:** `SPEC-bunker-native-file-tools.md` (what the verbs are),
`ADR-toolsd-integration.md` (why the tool is DELIVERED rather than linked),
`SPEC-toolsd-agent-surfaces.md` (the socket-service shape, gated on measurement).

## The problem this closes

The verbs execute inside the agent, in the agent's uid and filesystem context.
So a tool present on the *client* is irrelevant to them — but nothing said so,
and nothing reported the difference. A fresh agent was measured (GAP-092, and
again on an independent spawn) to have `git` and `jq` but **neither `toolsd` nor
`rg`**, so the remote editing surface was code-complete and unrunnable.

## The division of labour (the load-bearing decision)

There are two ways a tool can reach an agent, and picking wrong is how you get
either a supply-chain hole or an unmaintainable one-off.

**1. Copy a binary — for tools WE build.** `toolsd` is ours; it exists in no
registry, so a copy is the only honest mechanism. `bunker agent-tools --install`
copies it into the agent's `$HOME/bin`, which the server **already** puts on
every exec PATH (`agentExecBasePath`), so no shell profile is touched and
nothing about the agent's environment is mutated.

**2. `image-spec` package-add — for tools a distribution ships.** `ripgrep` and
the language servers (`gopls`) are in registries. Installing them through the
package manager keeps version pinning and signature verification, which a copied
binary silently discards. The existing mechanism already supports this and
already allows it by default (`agent.image_spec.enabled: true`, managers
`apt`/`go`/`npm`):

```json
{"packages":[{"manager":"apt","packages":["ripgrep"]},
             {"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}
```

The command therefore does **not** claim to have fully provisioned an agent. On
every run it names what it did not deliver and prints the directive above.

## The artifact has to be shippable

`toolsd` was described as a static binary while nothing built one: `make build`
is `go build ./...` (no version stamp, dynamically linked) and `make install`
stamps a version but still links dynamically. That is fine for the local box and
wrong for a binary that crosses a host boundary — a glibc difference is
invisible until run time, on the far side, with no obvious cause.

`make dist` (toolkit repo) builds the shippable artifact: `CGO_ENABLED=0`,
`-trimpath`, stamped. It proves the binary is static (ldd, `file(1)` fallback)
and that the stamp reached it, and refuses otherwise. `DIST_CGO=1` exists to
make that guard testable — a guard that only ever sees a passing input is not a
guard. Both directions were verified: default rc=0, forced-dynamic **rc=2** with
`file(1)` independently confirming the binary really was dynamic.

The client enforces the same guarantee at the last moment before shipping, so a
bad artifact is stopped where it can be, not discovered on the agent.

## Guards

| Guard | Behaviour | Why |
|---|---|---|
| Static-link, client side | Refuses a dynamic binary; asks ldd then `file`; **refuses when neither confirms** | The failure it prevents is remote, delayed and confusing, so a false refusal is cheaper than a false pass |
| Only-vendored | Only tools we build are copied; registry tools are named and handed to the package path | A binary copy bypasses version pinning and signatures |
| Version drift | A named WARNING when the local artifact and the agent's copy differ | Testing one build while an agent runs another is a silent behaviour split |
| `--binary` validation | Nonexistent, directory, or non-executable refused; absent-from-PATH names the remedy | A clear failure here is cheaper than a mystery downstream |
| Home directory | Asked of the **agent** (`$HOME`), never assumed | The home layout is the daemon's choice |
| Result is evidence | After copying, the agent is RE-PROBED; the delivery fails if the tool still is not reachable | "Copied the file" is not "the tool works" |

## Measured behaviour (fresh spawn, bunker-las-02)

```
BEFORE   toolsd absent | rg absent | git present (2.47.3) | jq present (1.7) | gopls absent
INSTALL  delivered toolsd version 98c75fa-dirty -> /home/bunker-verbs-proof/bin/toolsd
AFTER    toolsd present, version reported by the probe, drift warning silent (same build)

On the agent, with the workspace root created first:
  write (stdin payload)             rc=0   33 bytes
  read                             rc=0   content returned
  read --offset 2 --limit 1        rc=0   "line two"
  list / list --json               rc=0   name, dir, size
  read  ../../etc/passwd           REFUSED "path outside root", rc=1
  write ../escaped.txt             REFUSED "path outside root", rc=1
  no escaped file present afterwards
  describe --json                  84 verb names (advanced set present)
```

## Known behaviours worth stating

- **A nonexistent `--root` is refused** by `fsops` (`lstat … no such file or
  directory`). This is correct — a root that does not exist cannot be a
  confinement boundary — but it means the workspace root must be created before
  the verbs are useful. Discovered by the first live run, where the refusal was
  mistaken for a delivery failure: the error text says exactly what is wrong.
- **`--install` is a supported command, not yet an automatic spawn step.** A
  fresh agent still needs it run once. Making it automatic touches the spawn
  path, which requires the live E2E battery, and is filed separately.
- **No uninstall/rollback yet** (GAP-096 criterion 3).

## Remaining work

1. Deliver `rg` + a language server through the image-spec package-add path, and
   prove a fresh spawn reports them present (TOOLS-002's open half).
2. Make the delivery part of spawn so it is a property of an agent rather than a
   command someone remembers to run (GAP-096's "by design").
3. Uninstall/rollback, so removing a delivered tool is as supported as adding
   one (GAP-096 criterion 3).
