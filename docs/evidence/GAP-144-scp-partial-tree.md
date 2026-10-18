# GAP-144 — `bunker deploy` / `bunker cp` partial-tree evidence

Measured 2026-10-17 against `bunker-mvp` (78.46.173.180, tailnet 100.115.67.116),
binary built from the tree at `7e5d4d5b` with the fix below, client OpenSSH 10.2p1.

The board row named three things. Only one of them was still true.

## 1. The identity pair was already present (claim did NOT hold)

`buildSCPArgs` (`internal/cli/cp.go`) has carried `-o IdentitiesOnly=yes` and
`-i <agent-key>` since `c238df46`; `cp` and `deploy` both go through it. The same
pair is in `buildSSHArgs`, `buildPullSCPArgs`, `buildSSHProbeArgs` and the
agenttools post-cmd. `bunker exec` shells out to no ssh/scp at all — it is RPC
(`ExecAgent`).

## 2. "Too many authentication failures" did not reproduce (claim did NOT hold)

Scenario built as described: a private ssh-agent (`ssh-agent -a <sock>`) with
**7** throwaway ed25519 keys, an agent spawned on bunker-mvp, and its own key
passed with `-i`.

```
# no IdentitiesOnly, ControlMaster/ControlPath disabled (no ambient mux reuse)
scp -o ControlMaster=no -o ControlPath=none -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null -i ~/.config/bunker/keys/<agent> \
    <file> bunker-<agent>@bunker-mvp:/tmp/control-a2.txt
rc=0

# with IdentitiesOnly
scp ... -o IdentitiesOnly=yes -i ~/.config/bunker/keys/<agent> ...
rc=0
```

`ssh -vv` shows why — the explicit `-i` identity is offered FIRST, so the decoy
agent keys are never tried:

```
debug1: Offering public key: .../keys/<agent> ED25519 ... explicit
debug1: Server accepts key: .../keys/<agent> ED25519 ... explicit
Authenticated to 78.46.173.180 ... using "publickey".
```

`IdentitiesOnly=yes` remains worth keeping (it is the portable guarantee, and
older/other clients do offer agent keys first), so it stays pinned by tests —
but on this host it was not what broke the transfer.

## 3. The partial tree was real — and its cause was the copy deadline

`bunker deploy /tmp/gap144-tree <agent>:/home/bunker-<agent>/test-deploy` with the
290-file tree failed twice, before the budget fix:

```
scp failed: 41 of 290 files landed, destination may be incomplete: ... — remove it or re-run the copy before using it
bunker: scp -r: exit status 1          rc=1
scp failed: 37 of 290 files landed, destination may be incomplete: ... rc=1
```

The reported counts were exact — the remote directory held 41 and 37 files in the
two runs. Those counts come from the bounded `find <dest> -type f | wc -l` probe
over the transfer's own SSH identity, which is what the fix adds.

Root cause of the abort: `copyChildBudget` was a flat 30s for the whole recursive
copy, and the measured transfer rate on this link is ~0.7 s/file (raw
`scp -r` of the same tree: 166 files in 120s). So scp was SIGKILLed by its own
deadline after ~41 files — every deploy of a real tree died part-way.

## 4. After the fix

`bunker deploy` of the same 290-file tree, same 7-key agent:

```
Deployed /tmp/gap144-tree to <agent>:/home/bunker-<agent>/test-deploy3
rc=0
```

remote: `find <dest> -type f | wc -l` → `290`, owned by the agent user
(`drwxrwxr-x bunker-<agent> bunker-<agent>`).

The recursive step now scales (30s + 2s/file, capped at 30m); single-file `cp`
and the following chown keep the flat 30s.

## Tests that pin this

`internal/cli/cp_test.go`, `internal/cli/deploy_test.go`:

- `TestBuildSCPArgsForcesIdentitiesOnly`, `TestDeploySCPArgsForcesIdentitiesOnly`
  — table-driven argv construction (`-o IdentitiesOnly=yes`, `-i <key>`, order
  relative to the operands, `-r` placement).
- `TestSCPArgvCarriesIdentitiesOnlyEverywhere` — family sweep over every
  builder in the package, so a new one cannot reopen the hole.
- `TestCpCommand_ScpInvocationCarriesIdentitiesOnly` and the deploy twin — the
  EXECUTED argv is recorded from a stub and asserted (wiring, not just builder).
- `TestCpCommand_PartialTransferReported` and the deploy twin — landed/total,
  zero-landed, and unmeasurable-destination branches.
- `TestCountLocalSourceFiles`, `TestParseRemoteFileCount`,
  `TestReportSCPFailureStateUnreadableSource`, `TestDeployCopyBudgetScalesWithTree`.

RED proof: removing the two `reportSCPFailureState` calls made all six
partial-transfer subtests fail with the pre-fix production message
(`Error: scp: exit status 1`, no counts); restoring the files byte-identically
(`sha256`) turned them green.

## Operational note

The destination is never deleted by the CLI on failure: it may have pre-existed
with the agent's own data. The partial state is made loud instead, with the
counts and the remedy, and the exit status stays non-zero.
