# Evidence: INTEG-002 — toolsd safety semantics survive the bunker exec path

**Row:** INTEG-002 (extends GAP-092) · **Date:** 2026-10-11 · **Agent:** `integ002-evidence` (disposable, spawned for this run)
**Server:** `bunker-mvp` (`100.115.67.116`) · **Agent uid:** `bunker-integ002-evidence` · **Agent arch:** x86_64
**toolsd under test:** built from `coding-hermes-tools` @ `a9af41a` (`CGO_ENABLED=0`, static), sha256
`293c8071fc4005d57d73950bb8e3e5fd816b6039157af866b9b0d730345e91c5`, reported version `toolsd version dev`;
delivered via `bunker cp` and **sha256-verified identical on the agent before the run** (recorded in the
transcript's ENVIRONMENT section, both ends).
**Method:** one driver (`tools/integ002-safety-probe.sh`), run **identically** on the control host and
inside the agent — each run seeds the same deterministic fixture in a fresh ROOT and prints raw
command/stdout/stderr/exit-code triples. No output below is paraphrased; every quoted line is from
the committed transcripts. Transcripts: `docs/prd/evidence/integ002-host.out`,
`docs/prd/evidence/integ002-agent.out`; normalized host↔agent diff: `docs/prd/evidence/integ002-parity.diff`.

## Result: all five safety semantics PASS inside the agent

### AC-1 · inexact patch refuses, target byte-identical — PASS

```
$ toolsd patch bad.diff
toolsd: patch: hunk 0: does not apply: no exact match for its 3 context/removal line(s) at line 1 within an offset window of 0
toolsd: patch: rolled back - no files in the tree were modified
rc=1
$ sha256sum patchme.txt
b6285c57e8797db5d4c51c80d6f11938afda9b11c6a003549709189e9b4b92a2  patchme.txt
```

stdout is empty (the refusal and the rollback statement both go to stderr), the reason names the hunk
and the match failure, and the post-refusal hash equals the pre-hash — and equals the value GAP-092
recorded for the same fixture recipe (`b6285c57…`), so the refusal path has not drifted since that run.

### AC-2 · multi-file apply rolls back on a commit-time failure — PASS

Positive control first (same shape, all-regular-file targets): `applied 2 file(s) atomically`, rc=0.
The subject edit-set writes file 1, then targets entry 2 at a **directory** — a deterministic commit-time
failure that fires *after* the first write is staged, which GAP-092's parse-time refusal (unknown key)
did not reach:

```
$ toolsd apply es-refuse.json
toolsd: apply: multifile: commit: snapshot …/probe-root/d2: read …/probe-root/d2: is a directory
toolsd: apply: rolled back - no files in the tree were modified
rc=1
$ sha256sum a1.txt big5.txt          # file 1 AND the bystander, unchanged
eb6f09c69e5e4b1dae158340da17a9c13204e988f27dcc3ea964a5f9fdd9436c  a1.txt
d8b453b3269032b2f9674dd33d2062f7d171fa69c06e4b78fb0b06e594cbae26  big5.txt
```

`ls d2` still shows an empty directory: the transaction snapshot/restore beat the wire. No partial write.

### AC-3 · ambiguous replace refuses naming the count — PASS

```
$ toolsd replace editme.txt a X
toolsd: replace: editme.txt: textedit: ambiguous match: 4 occurrences (replace exactly one by narrowing the needle, or use --all / replace-all semantics for every occurrence)
toolsd: replace: re-run with --all to replace every occurrence
rc=1
```

The no-match class (`toolsd: replace: editme.txt: textedit: no match`, rc=1) is also in the transcript.
`editme.txt` is byte-identical after both refusals: its POST-HASHES value `b0d5fcac…` equals the hash
recorded immediately after the (successful) `beta → BETA` control, so the two refusals changed nothing.
The fixture recipe reproduces GAP-092's exact bytes: `editme.txt` seeds at `4fdbc441…` and
`patchme.txt` at `b6285c57…` — both equal GAP-092's recorded fixture table.

### AC-4 · lease: one grant, one refusal naming the holder, registry on the agent — PASS

```
$ toolsd lease acquire --holder holder-A --ttl 45m --note INTEG-002 lines.txt   → grant, rc=0
{ "holder": "holder-A", "paths": ["lines.txt"], "expires_at": "2026-10-11T22:12:56.939982228Z", … }
$ toolsd lease acquire --holder holder-B --ttl 45m lines.txt
toolsd: lease: path "lines.txt" is held by holder-A (pid 0, expires 2026-10-11T22:12:56Z)
rc=1
```

Two independent exec round-trips, two separate toolsd processes: the second holder is refused by the
first's live registry entry — the cross-session property. The registry file itself lives **on the agent**
(`.git/agent-leases.json` inside the agent's probe ROOT); the transcript cats it and records its sha256,
then `lease release holder-A` empties it.

### AC-5 · stdin edit-set with quotes, newlines, NUL — applies or refuses by name, never mangles — PASS (both arms)

**Arm A — NUL as the JSON escape `\u0000`:** applied over the transport with `bunker exec --stdin`-shaped
redirection (`toolsd apply - < editset-nul.json`), rc=0, and the written file carries a **real NUL byte**
between the quotes, the newline and the backslash, hash-equal to the locally constructed expectation:

```
$ od -c nul.txt | head -2
0000000   l   i   n   e       o   n   e       w   i   t   h      \0
0000020   N   U   L       i   n   s   i   d   e  \n   l   i   n   e
$ sha256sum nul.txt
68f4b33e9e317aeaf15a5e9119783aab67aad6ac6ac361919fa376a589a3ed79  nul.txt   # == printf-emitted expectation
```

**Arm B — a RAW NUL byte in the JSON source:** refused by name before any write:

```
toolsd: apply: parse the edit-set as a JSON array: invalid character '\x00' after object key:value pair
rc=1
$ ls nul_bad.txt
ls: cannot access 'nul_bad.txt': No such file or directory     # nothing written
```

## Host↔agent parity

`integ002-parity.diff` compares the two runs' skeletons (every section header, command line, stderr line
and exit code; identity strings normalized) — **79 lines each, exactly ONE differing line**: the lease
refusal's `expires` timestamp, which is wall-clock time of the run, not semantics. This is the same
single difference class GAP-092 recorded ("lease timestamps are host-local"). Every refusal, every
rollback line and every exit code is identical across the exec boundary.

## Contract facts recorded while verifying the surface (verify-before-writing)

- **F1:** `toolsd patch -` and `toolsd apply -` both exist in the current build — stdin-carried payloads
  work for the diff verb as well as the edit-set verb (the ADR's stdin rule now covers both).
- **F2:** `apply` resolves entry paths against the **edit-set file's directory**, not the cwd (documented
  in `toolsd apply --help`; measured in prototyping: a file-anchored set wrote next to the set file).
  Transport consequence: prefer `apply -` (paths then resolve against the cwd) or absolute paths.
- **F3:** the current build closes GAP-092's Divergence F-1: `read`, `write` and `list` are now CLI
  subcommands on this box (surface observed locally; the agent legs exercised apply/patch/replace/lease).
- **F4:** NUL has two distinct contract behaviors: JSON `\u0000` → a real NUL byte in the written file;
  a raw NUL byte in the JSON source → a named parse refusal with nothing written. Neither mangles.

## Reproduce

```
# spawn a disposable agent and deliver the pinned binary + driver (sha256-verify both)
BUNKER_SESSION_TARGET=bunker-mvp bunker spawn integ002-evidence --ttl 6h
CGO_ENABLED=0 go build -o /tmp/toolsd ./cmd/toolsd        # in coding-hermes-tools @ a9af41a
BUNKER_SESSION_TARGET=bunker-mvp bunker cp /tmp/toolsd integ002-evidence:/home/bunker-integ002-evidence/bin/toolsd
BUNKER_SESSION_TARGET=bunker-mvp bunker cp tools/integ002-safety-probe.sh integ002-evidence:/home/bunker-integ002-evidence/probe.sh

# host leg
ROOT=$(mktemp -d) bash tools/integ002-safety-probe.sh > host.out

# agent leg (everything over the existing exec path)
BUNKER_SESSION_TARGET=bunker-mvp bunker exec integ002-evidence -- \
  sh -c 'mkdir -p $HOME/probe-root && ROOT=$HOME/probe-root TOOLSD=$HOME/bin/toolsd bash $HOME/probe.sh > $HOME/probe-agent.out'
BUNKER_SESSION_TARGET=bunker-mvp bunker exec integ002-evidence -- cat /home/bunker-integ002-evidence/probe-agent.out > agent.out

# normalize identity strings, diff the skeletons -> expect only the lease timestamp line
BUNKER_SESSION_TARGET=bunker-mvp bunker destroy integ002-evidence
```

## What this means

The ADR's decisive property — *the primitives execute in the agent's own privilege context and their
verdicts survive the exec boundary intact* — now holds for the safety machinery specifically, not just
the parameter surface GAP-092 measured: refusal-with-named-reason, atomic rollback after a staged write,
match-count refusal, and the per-tree cross-holder lease registry all demonstrably run INSIDE the agent
(the registry file is on the agent; the rollback happens agent-side; the NUL byte lands in an agent-side
file). No bunker Go code was modified: the exec path needed nothing new — raw transcript bytes, exit
codes and stdin payloads all round-trip as-is.
