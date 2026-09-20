# SPEC: Bunker native file tools + toolsd verbs — namespace and workspace control

**Project:** bunker · **Rows:** GAP-094 (exec bodies), GAP-106 (dynamic instances), **this spec adds the namespace/workspace argument model and the verb catalog** · **Author:** Hermes · **Date:** 2026-09-20
**Status:** proposed (spec of record for Path A) · **Effort:** M · **Depends on:** GAP-093 (fail-closed binding) · **Feeds:** GAP-097 (shim)
**PRD of record:** `docs/prd/PRD-bunker-remote-editing.md` (Path A)

---

## The problem in one sentence

Path A gives the mesh of file operations over the wire — every verb of the local tool set and every `toolsd` primitive — and `toolsd` is *already registered as an MCP server on this box* (`config.yaml:1060`), so the work is not "expose tools"; it is **make every verb carry an unambiguous namespace and workspace, with the same semantics as the local tools.**

## Two sources, one catalog

The verb set is the **union** of what the local tool set does and what `toolsd` adds. They are not competitors: `toolsd` is the safety-hardened implementation of the same primitives, and the local tools define the *parameters* the surface must honour.

### Source 1 — the local tools (what "as good as local" means)

| Local tool | Parameters that must be honoured | Notes |
|---|---|---|
| `read_file` | `path`, `offset` (1-based), `limit`, line numbers, `total_lines`, continuation | windowed, never whole-file |
| `search_files` | `pattern`, `path`, `target` (content/files), `output_mode` (content/files_only/count), `context`, `file_glob`, `limit`, `order` | three output shapes |
| `write_file` | `path`, `content` (whole file, create parents) | atomic |
| `patch` | `old_string`, `new_string`, `replace_all` | unique match by default |
| (terminal) `argv` | command, cwd, timeout | execution |
| (skills/board) | n/a | out of scope |

### Source 2 — the `toolsd` primitives (what each is *for*)

| Verb | Purpose (from its own descriptor) | Parameters |
|---|---|---|
| `patch` | a diff you did not generate that might not apply cleanly — strict, **no fuzz**, refuses instead of corrupting | a diff **file** (or `-` for stdin); no flags |
| `replace` | change one known literal, exactly once — unique match or `--all` | `file`, `old`, `new`, `--all` |
| `apply` | a change spanning several files, atomic all-or-nothing with rollback | an edit-set JSON: `[{path, content}]` |
| `diff3` | two edited versions against a common base — reconcile instead of picking a winner | base + two sides |
| `lease` | cross-session edit registry — acquire/renew/release/status before editing shared files | `--holder`, `--paths`, `--ttl`, `--root`, `--pid` |
| `lsp` | definition / references / check before an edit made blind to symbols | `--file`, `--line`/`--character` or `--offset`, `--root`, `--server` |
| `probe` | bounded HTTP probe battery against one endpoint | `--n`, concurrency, rate cap |
| `narrate` | turn a diff into a JSON/Markdown evidence block | diff input |
| `session` | one git worktree per agent so work does not disturb another checkout | `start`/`end`/`list` |
| `fsops` | confined file-system ops: rooted read, whole-content write, mkdir, list — **the confinement root is the mechanism** | `Open(root)`; paths must resolve inside it |

**Note the marriage:** `fsops.Open(root)` is already *the* workspace-confinement primitive — every path must resolve inside the root or it fails `ErrOutsideRoot` before anything is touched. That is exactly the guarantee the remote layer needs, so the native verbs are built *on* `fsops` rather than re-implementing path checks.

## The catalog (constant), and the two axes that vary per call

Two axes, and confusing them is the failure mode to avoid:

| Axis | Values | Meaning |
|---|---|---|
| **namespace** | the fleet project / server alias (e.g. `bunker-las-02`, project `bunker`) | *whose* bunker — the routing axis |
| **workspace** | a tree inside the agent (repo root, subdirectory) | *which tree* — the confinement axis |

Both are **arguments**, never ambient globals. Resolution:

1. explicit per call (`--namespace`, `--workspace`)
2. the session binding (set once per session)
3. **refusal** naming the missing binding — never a fallback to a global

### The verb catalog (Path A)

| Verb | Parameters | Semantics | Backed by | Needs |
|---|---|---|---|---|
| `bunker_read` | `namespace?`, `workspace?`, `path`, `offset`, `limit`, `max_bytes` | line-numbered window + `total_lines` + `next_offset`; truncation notice | bounded `sed -n` + `wc -l` on the agent | — |
| `bunker_list` | `…`, `path` | one directory level, with metadata | `fsops.List` | — |
| `bunker_search` | `…`, `pattern`, `target`, `output_mode`, `context`, `file_glob`, `limit` | matches / file list / counts | `rg` on the agent | — |
| `bunker_write` | `…`, `path`, `content` | whole-file, parents created, atomic | `fsops.Write` (temp + rename) | GAP-094 |
| `bunker_edit` | `…`, `path`, `old_string`, `new_string`, `replace_all` | unique match by default; refuses naming the count | `toolsd replace` | GAP-094 |
| `bunker_patch` | `…`, `diff` | strict, no fuzz, refuses rather than corrupting | `toolsd patch -` | GAP-094 |
| `bunker_apply` | `…`, `edits[] {path, content}` | atomic all-or-nothing, with rollback | `toolsd apply` | GAP-094 |
| `bunker_diff3` | `…`, `base`, `ours`, `theirs` | three-way reconcile | `toolsd diff3` | — |
| `bunker_exec` | `…`, `command`, `cwd`, `timeout` | run to completion; exit code preserved | `ExecAgent` | — |
| `bunker_run` | `…`, `command`, `detach`, `name` | long jobs; detached returns a run id | `RunAgent` | — |
| `bunker_lsp` | `…`, `op`, `file`, `line`+`character` \| `offset` | definition / references / check | `toolsd lsp` | language server on agent |
| `bunker_lease` | `…`, `action`, `holder`, `paths`, `ttl` | lease on **the workspace tree** | `toolsd lease` | CHT-052 |
| `bunker_session` | `…`, `action` | worktree per agent in the workspace | `toolsd session` | — |
| `bunker_narrate` | `…`, `diff` | evidence block from a diff | `toolsd narrate` | — |
| `bunker_probe` | `…`, `endpoint`, `n` | bounded probe battery | `toolsd probe` | — |
| `bunker_instances` | — | discover reachable instances (data) | session profile + fleet inventory | GAP-106 |
| `bunker_bind` | `namespace`, `agent`, `workspace` | bind this session | session state | GAP-093 |

**Constant-catalog rule:** adding an instance does not add a tool. The catalog's size is fixed by the verb count; instances are discovered data and the target is an argument.

## Namespace + workspace enforcement (where it actually bites)

An argument is not enforcement. Four places the design makes it real:

1. **Resolution refusal.** No binding and no explicit argument ⇒ refuse (`no_workspace_bound`), never a default. Distinct from `namespace_unknown` and `workspace_invalid`, so an operator can tell "you did not say" from "that does not exist".
2. **Confinement.** Every path is resolved **inside the workspace root** — the `fsops` rule, applied remotely: a path that escapes the root (via `..`, a symlink, or an absolute path) fails **before** anything is touched, with `ErrOutsideRoot`. This is the mechanism that makes the workspace a boundary rather than a label.
3. **Workspace identity.** The resolved workspace must carry the expected marker (git remote, or a workspace marker file). A mismatch refuses naming the expected vs actual, so aiming at the wrong tree is detectable — the Path A counterpart of the mount's `stale` verdict.
4. **Echo.** Every verb's result names the `(namespace, agent, workspace)` it acted on, so an audit row and a tool result are both self-describing.

### Namespace isolation is per *namespace*, not per verb

Two sessions bound to different namespaces never share: not the binding, not the lease registry (per tree), not the mountpoint (per session), not the audit attribution. That is the isolation axis, and the spec requires it be *proved* (see below) rather than assumed.

## Interface rules (design imperatives, not preferences)

1. **Parameter parity is a contract, not a nicety.** For every verb with a local counterpart, the same arguments produce the same result. Divergences are enumerated in one place and each is justified — never discovered by accident.
2. **A refusal is a success of the tool.** `patch` refusing an inexact diff, `replace` refusing an ambiguous needle, confinement refusing an escaping path: report as an outcome, never retry blindly.
3. **No success without evidence.** A write reports success only after a read-back or a verified post-condition. Otherwise `unverified`.
4. **Two known divergences, both deliberate** (carried from the PRD, repeated here so this spec is self-contained):
   - local `patch` takes `old_string`/`new_string`; `toolsd patch` takes a diff ⇒ the two-string path is `bunker_edit` (on `toolsd replace`), and `bunker_patch` stays for genuine diffs.
   - local `patch` fuzzy-matches; `toolsd patch` refuses on inexact input ⇒ **stricter**, and documented as a behaviour difference.
5. **Capability variance is an error, not a missing tool.** A verb the target lacks stays on the surface and returns `capability_unavailable` naming what is missing.

## Acceptance criteria

1. **Parity matrix.** For `read`, `list`, `search` (all three modes), `write`, `edit`, `patch`, `apply`: the same arguments run locally and remotely produce identical output modulo the tree; every divergence recorded with its justification.
2. **Confinement proof.** A path escaping the workspace (`../`, an absolute path, a symlink pointing out) is refused with `ErrOutsideRoot` **and nothing is written** — proved against a real agent.
3. **Binding refusal proof.** With no binding and no explicit argument, every mutating verb refuses with `no_workspace_bound`, naming the missing piece, and touches nothing; `namespace_unknown` and `workspace_invalid` are distinct verdicts.
4. **Identity proof.** With the workspace pointed at a different project's tree, the call refuses naming expected vs actual.
5. **Constant-catalog proof.** With three instances known: the offered tool list is byte-identical before and after adding or removing an instance; switching instances changes only the target.
6. **Namespace-isolation proof.** Two sessions bound to different namespaces run concurrently: zero cross-writes, zero shared state, distinct audit attribution.
7. **Refusal-fidelity proof.** `bunker_edit` on an ambiguous needle refuses naming the count with the file left **byte-identical**; `bunker_patch` on an inexact diff refuses with the file untouched.
8. **Anti-lying proof.** No verb reports success without a read-back; `unverified` never renders as success; a capability-missing verb reports `capability_unavailable` while remaining on the list.
9. **Echo proof.** Every result names the `(namespace, agent, workspace)` acted upon, and the audit row carries the same triple.

## Boundaries — what this spec does NOT do

- **Does not grow the catalog per instance.** Instances are data.
- **Does not replace the mount path.** It is the targeted/attributed write path; the mount is the high-fidelity editing path.
- **Does not run builds locally.** Execution is always remote.
- **Does not manage credentials.** Per-session profiles partition them; the verbs never store, rotate or mint them.
- **Does not fuzzy-match.** Strict refusal is the designed behaviour.
Related: [docs/both-ways.md](../../both-ways.md) — when to use the verbs vs the SSHFS mount.
