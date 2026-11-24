# Evidence: bunker dogfood bunker-dogfood-2026-11-24-20-42-53

Run date: 2026-11-24. Single dispatch (gateway 15:43:14 EST). Product workdir HEAD 6ab8fc9e == origin/main at start.

## Contents
- out/census-reap.txt
- out/census-status.txt
- scripts/arms-spawn-open.sh
- scripts/arms-spawn-retry.sh
- scripts/arms-spawn-std.sh
- scripts/real-use-arm.sh
- scripts/transfer-control.sh
- scripts/transfer-inspect.sh
- scripts/transfer-mechanism.sh
- spawn-arm1.err
- spawn-open.out
- spawn-retry.err

## Redaction table (applied to every file here)
- the preset-open spawn bundle's agent API key line: `API Key: <value>` -> `<REDACTED-agent-api-key>`
- tailscale IP `100.69.3.13` -> `<host>`
- absolute home `/home/kara/` -> `~/`

Counts and exit codes are otherwise byte-identical to the raw scratch copies that ran.

## Key measured facts (arm -> result)
- std-preset spawn (default, README quickstart): 41.84s -> rc=1 `containment landing did not converge within 40 reads: memory.swap.max="max", want "0"` (DFS arm 1); verbatim retry 22.17s -> rc=1 on `memory.high="7730937856", want 1932735240`. Control: `--preset open` 17.95s rc=0; same-day 12:42 std-preset spawn (agent 90365977) converged, slice reads swap.max=0. -> DF-BUNKER-124
- Failed std spawn residue: /etc/systemd/system/user-1005.slice.d/50-bunker.conf stranded (MemorySwapMax written, user gone); healthy-agent destroy DID remove its own slice dir (user-1004 gone after `bunker destroy c940c03f` 16.64s rc=0).
- cp/pull 30s budget: cp 30,000,000B -> file landed 27,156,480B (3316x8KiB) at 33.04s rc=1 "1 of 1 files landed, destination may be incomplete"; two 60MB cps truncated at 33s each; agent-side writes verified unlimited (ulimit -f unlimited; 30/40/60MB direct writes full-size). pull 27MB: 25.50s rc=0 byte-honest; pull 60MB: 30.44s rc=1 "scp: exit status 1" while pulled bytes md5-equal the source (full landing + failure rc). Mechanism: copyChildBudget=30s (internal/cli/cp.go:503) at cp.go:246 and pull.go:157. -> DF-BUNKER-125
- status vs reap: status Residue line all zeros; reap same day: 165 orphan apparmor profiles would-remove, 163 unmanaged ignored; inventory has no profile field (grep OrphanProfiles = 0 hits). -> DF-BUNKER-126
- exec roundtrip 0.55s; docker run --rm alpine echo hello 5.13s (incl. image pull) — quickstart docker promise holds; destroy 16.64s rc=0 clean.
- Perf context: cp warm 5.81s / cold 4.63s on the deployed CLI (0422dae7 2026-11-11, verified to NOT contain PERF-020's 18a73960 merge) — the landed 0.7s streaming claim is not observable through the binary users actually run.
