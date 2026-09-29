# 2026-09-29 — release-channel (fresh-user install) surface, run 23

Dogfood lane: bunker-dogfood. Angle picked by the repeat-run rule: 22 prior
runs all proved installability by tar-streaming the dev checkout into an
ephemeral agent — the README's actual fresh-user install channel (Option 1,
the published release installer + `make build` from a real clone) had never
been exercised. This run installed the way a fresh user would.

## What I did

1. Probe bunker-las-03 (HOST_OK, bunkerd active, docker 26.1.5). Spawn
   `dfinst-rel` (TTL 2h).
2. **Option 1 — release installer** on the bare agent: `curl -fsSL …/install.sh | sh`
   → downloaded both binaries, verified against SHA256SUMS before writing,
   fell back to `~/.local/bin` (documented behavior), smoke `bunker --version`
   OK. **12 seconds total, RC 0.** Fresh agent knew nothing about the server;
   `bunker status` correctly said "no servers configured; run 'bunker connect'"
   — good first-run UX.
3. **Option 2 — build from source** on the same fresh agent (clone from
   github.com/deployBunker/bunker, HEAD 3cc8f73): the README's Go tarball
   recipe + `make build` → BUILD_RC=0 in **48s** cold (module cache empty,
   `go: downloading` lines confirm). `./bunker --version` → commit 3cc8f73.
   `./scripts/install.sh --dry-run` plan printed correctly.
4. Headline op timed warm on the same server: `bunker spawn --ttl 30m` →
   **40s** (dominated by the agent's rootless-docker home install — README
   documents 60–90s for first-spawn-on-host, so this is comfortably inside
   the documented envelope). spawn+destroy clean, `bunker list` back to 0.

## Findings (rows on tasks.jsonl, id family BUNKER-INST / PERF-012)

| ID | Finding |
|----|---------|
| BUNKER-INST-001 | **The release channel serves 879 commits of stale surface.** Release latest = v0.1.4 (235e715, built 09-19); HEAD = 3cc8f73; `v0.1.4..HEAD` = **879 commits**. README honestly discloses this (the "Freshness check" block) — good — but the consequence is that the one-command install, `go install @latest`, and every prebuilt binary give a fresh user a CLI whose `stop/start/restart`, `homes`, `linger`, `host-provision` surface is missing entirely. A fresh user following the README feature list hits "unknown command" on features the README describes as shipped. |
| BUNKER-INST-002 | **Multi-tenant shared-host /tmp collision:** the Go tarball download died twice with `curl (23) client returned ERROR on write` because `/tmp/go.tar.gz` already existed, owned by uid 1006 (another agent's leftover on the shared host /tmp — before host-provision, agent /tmp IS host /tmp, exactly what specs/agent-tmp-isolation.md documents). The README's own Go-install recipe uses `/tmp/go.tar.gz` as its literal path. Re-downloading into `$HOME` succeeded first try (66.9MB, RC 0). The two curl failures are 1:1 the README's recipe, not a bunker defect — but the README should use a collision-safe path, and this is live proof of the /tmp-host-shared class. |
| BUNKER-INST-003 | Nothing broke. The installer's non-writable-prefix fallback, SHA256 verify-before-write, PATH warning, and the CLI's unconfigured-server error all behaved exactly as documented. Option 2's only silent assumption was satisfied (make, gcc, git all present on the bare Debian agent). No install friction beyond BUNKER-INST-002. |
| PERF-012 | Release installer cold = 12s (one-shot; sub-second after download warm — not worth profiling). Source build cold = 48s. Spawn warm = 40s (inside documented 60–90s first-spawn envelope). Nothing here is slow enough that a user would notice beyond the documented first-spawn cost — no profile taken, no fix row. |

## Verdict

🟢 **SHIPPABLE (install surface)** — both documented install paths work on a
bare machine in 12s (prebuilt) / 48s (source), with honest release-lag
disclosure. The release channel's 879-commit staleness (BUNKER-INST-001,
P1) is the one real gap: not a bug, but the difference between what a fresh
user gets and what the README sells.
