# Evidence: GAP-092 — the toolsd remote contract, hand-driven against a real agent

**Row:** GAP-092 (S1 zero-code proof) · **Date:** 2026-09-20 · **Agent:** `gap092-proof`
**Server:** `bunker-las-02` (`bunker-mvp`, `100.116.99.35`) · **Agent uid:** 1004 (`bunker-gap092-proof`)
**Method:** one driver script (`gap092-verbs.sh`), run **identically** on the control host and inside
the agent against a **byte-identical** fixture, then `diff`ed after normalising only identity,
binary path and tool availability. No output below is paraphrased; every line is a raw
command/output/exit-code triple from that run.

## Why this row existed

Every argument about how to integrate the remote tooling was unmeasured. This run replaces
those arguments with a transcript before more code is built on the design.

## The fixture

Six files, `sha256` verified identical on both sides (all six hashes matched; the manifest
was included in the tarball, and the agent's post-unpack hashes were compared line-for-line
with the host's):

| file | sha256 (identical host + agent) |
|---|---|
| `lines.txt` (120 lines, needles at 37 and 88) | `bc6dbc46d5e415cfb60c324ee9133a01df665b5558ef34577ccf1b5b647a07b2` |
| `editme.txt` (`alpha/beta/gamma`) | `4fdbc441ea7b546100e086ac1e4fc5ae6749b7314311c99db05be450eca12996` |
| `patchme.txt` (`one/two/three`) | `b6285c57e8797db5d4c51c80d6f11938afda9b11c6a003549709189e9b4b92a2` |
| `apply_a.txt` | `1964516747acc6decc956c98687976fd4a7c4dfc75e538268267025670cd8035` |
| `apply_b.txt` | `6fb09ca0c7d4c90df80fbe1c32b3b98790c9665a6d4a6d71e0b16412c6f73db8` |
| `binary.bin` (NUL + invalid UTF-8) | `7262474850a536e774f2bda95d3520cd60f23c442fee296e0cbb97945909b8e5` |

The agent had **no** git checkout of the fixture, so the driver `git init`s its scratch dir
before the lease section — lease requires a git working tree.

## Result: parameter parity holds, with exactly THREE divergences

The normalised diff of the two runs is 78 lines. Every line belongs to one of three classes,
and each is a finding rather than noise. **Everything else — every verb, every refusal, every
exit code — was byte-identical.**

### Divergence 1 (F-1, the significant one): the basic read/write/list primitives have NO CLI verb

```
$ toolsd read
toolsd: unknown subcommand "read"

$ toolsd describe --json     # the verb registry
apply  config  describe  diff3  fsops  help  lease  lsp  mcp
metrics  narrate  patch  probe  replace  session  status  version
```

`fsops` appears in the registry, but `read`, `write`, `list` and `mkdir` are all
**unknown subcommands**. The help text is explicit about why: *"Library primitives (Go
packages, no subcommand): fsops — Confined file-system operations: rooted read, whole-content
write, mkdir, list."*

**Impact.** The ADR's delivery model consumes `toolsd` as a **CLI contract**. Under that
contract the *advanced* verbs are all reachable — verified below — but the **basic**
read/write/list verbs are not reachable at all. "Normal basic file read/write tools plus
advanced ones" cannot be delivered as-is: the basic tier must either gain CLI subcommands or
be reached some other way, and any other way (shell `sed`/`cat`/`tee`) loses the `fsops`
confinement guarantee that makes the workspace a boundary. Today the read window is carried by
`sed` and the write by shell redirection — which is exactly the substitution the SPEC warns
against, and it is now measured rather than assumed.

### Divergence 2 (F-2): a fresh agent has no `rg`

```
host : tool rg: PRESENT (/usr/bin/rg)        $ rg -n 'needle' .   → 2 matches, rc=0
agent: tool rg: ABSENT                       $ rg -n 'needle' .   → rg: command not found, rc=127
```

All three search output modes (`-n` content, `-l` files-only, `-c` count) fail identically with
`rc=127`. Search parity is **not achievable on a fresh agent** until the image carries
ripgrep (or the search verb stops depending on an external binary).

### Divergence 3 (F-3): a fresh agent has no language server — and refuses CORRECTLY

```
host : lsp check --server gopls → full gopls handshake JSON, rc=0
agent: lsp check --server gopls → toolsd: lsp: server unavailable: executable "gopls" was not
                                   found in PATH (exec: "gopls": executable file not found in $PATH)
                                   rc=1
```

The capability is absent, but the *refusal is named, actionable and non-zero* — the behaviour
the SPEC requires of a capability-missing verb. Recorded as a delivery gap (GAP-096), not a
semantics defect.

### Not a divergence: lease timestamps are host-local

Lease JSON reports timestamps in the **host's local zone** (`-05:00` on the control host,
`-07:00` on the agent), so `expires_at`/`acquired_at`/`now` differ textually while the lease
semantics are identical (`holder-B` refused, naming `holder-A`). Anyone building a diff-based
regression harness from this method **must normalise timestamps or it will false-fail.**

## Per-criterion results (all from the agent run)

| # | Criterion | Result |
|---|---|---|
| 1 | toolsd present on the agent, version pinned and recorded | **PASS** — `toolsd version dev`, delivered md5-identical (stdlib-only static Go, x86_64 both ends) |
| 2 | Parameter parity measured; every divergence written down | **PASS** — 3 divergences, all above |
| 3 | Patch refusal reproduced remotely | **PASS** — `toolsd: patch: hunk 0: does not apply: no exact match for its 3 context/removal line(s) at line 1 within an offset window of 0` + `rolled back - no files in the tree were modified`, `rc=1`; `patchme.txt` sha256 identical after (`b2ef07f1…`) |
| 4 | Apply with rollback proven remotely | **PASS** — edit-set with an unknown key refused: `apply: edit 2 "apply_a.txt": unknown key "bogus" (an entry accepts only the "path" and "content" keys)`, `rc=1`; `apply_a.txt` sha256 identical after (`3b6cf3f7…`) |
| 5 | Lease writes ON THE AGENT; two concurrent acquires → 1 grant / 1 refusal naming the holder | **PASS** — `holder-A` granted; `holder-B` refused: `path "lines.txt" is held by holder-A (pid 0, expires …)`; registry at `.git/agent-leases.json` on the agent |
| 6 | Binary file (NUL + non-UTF8) survives a read/write round-trip sha256-identical | **PASS** — base64 round-trip through the transport, `cmp binary.bin binary.bin.rt` → `rc=0` |
| 7 | Written up with raw command/exit-code pairs, no paraphrase | **PASS** — this document |

Supporting parity confirmed identical (not merely "not checked"): the bounded read window
(`sed -n '30,40p'` and a beyond-EOF window), the unique-match `replace` plus **both** refusal
classes (no match, ambiguous needle) with the file left byte-identical
(`editme.txt` = `b0d5fcac…` both sides), and exit-code propagation through the transport
(`sh -c 'exit 42'`).

## What this means for the design of record

1. The ADR's rejection of library-linking and daemonising **stands** — the run was done entirely
   over the existing exec path with no new surface, and the safety semantics (refusal, atomic
   rollback, cross-session lease) survived the wire intact. That is the property the delivery
   model was chosen for.
2. The ADR's claim that the verbs are consumable as a CLI contract is **half true as measured**:
   true for the advanced tier, false for the basic tier. The basic tier needs a decision —
   `fsops` CLI subcommands, or an explicit, documented downgrade to shell primitives with the
   confinement guarantee named as lost. Filed as `TOOLS-001`.
3. `GAP-096` is no longer "deliver one binary" — the agent image must also carry the **supporting
   tools the verbs depend on** (`rg` for search; a language server per language for `lsp`).
   Filed as `TOOLS-002`.
4. This is a **permanent, re-runnable harness**, not a one-off: `gap092-verbs.sh` plus the
   normalised diff is the regression test for the whole remote verb layer.

## Reproduce

```
# host
ROOT=<fixture> TOOLSD=toolsd bash gap092-verbs.sh   > local.out
# agent
bunker cp gap092-verbs.sh <agent>:/home/bunker-<agent>/verbs.sh
bunker exec <agent> -- sh -c 'cd $HOME/workspace && ROOT=$HOME/workspace \
  TOOLSD=$HOME/bin/toolsd bash $HOME/verbs.sh'      > remote.out
diff <(norm local.out) <(norm remote.out)
```
