# Dogfood run 21 — 2026-09-25d — the ops/maintenance surface

**Target:** bunker @ HEAD 2118511 (local), live daemons bunker-mvp (v0.1.4 @ 509fc42) and cube-las-00 (v0.1.4).
**Angle:** runs 1–20 covered spawn/exec/env/cp/mount/tunnel-on-HEAD/durability/REST/TLS/renewal. This run took the
surface an operator hits on day two: the stop/start/restart lifecycle, the host residue tools (`homes`, `linger`),
`agent-tools`, the docker-tunnel workflow, and the fresh-agent install leg.

## Promise under test

"A user can spin up isolated agents from one CLI and operate them over their whole life: spawn, drive, pause,
restart, destroy — and keep the host clean with the residue tools." That promise is TRUE on HEAD, PARTLY FALSE on
the release channel the README installs.

## What was driven (all commands as a user runs them)

- `bunker --server bunker-mvp spawn --ttl 1h dfops0925` — 16.0s. Warn: no client SSH key (see DF-BUNKER-71).
- `bunker exec <id> --server bunker-mvp -- ...` — id, docker --version, docker run alpine (3.8s incl. pull), sh -c.
  exec round trip measured with hyperfine (10 runs, warmup 2): **1.25s ± 0.04s** against agent 2d8489e8 (warm).
- `bunker env set <id> DOGFOOD=ops0925` then exec — env visible. RPC verbs fully functional on a keyless agent.
- `bunker cp` / `bunker ssh` on the keyless agent — hard error, no recovery (DF-BUNKER-71/73).
- HEAD-CLI build (`git archive HEAD | go build`, commit 19892c3 included): spawn dfops0925c → key delivered,
  `cp` 7.0s byte-verified via exec cat, `ssh` verb works, `stop` (1.6s) → `start` (0.4s) → file survived,
  `restart` (0.4s) → **TTL reset to +6h** (documented semantics), file survived all three.
- `bunker tunnel dfops0925c 2377` → local `docker -H tcp://localhost:2377 ps` → connection reset (docker inside
  was dead — this is how DF-BUNKER-72 was found, via the tunnel the user is told to use).
- Host residue, read-only over root SSH on the control host: `bunker homes` (581 entries, 580 stale, 208.1 KB,
  2.5s), `bunker linger` (335 entries, 333 stale, "run `bunker linger prune` ... --dry-run first"). Both tools
  do exactly what their help says and correctly refused `--server` (local-only by design).
- `bunker agent-tools <id>` — clean dependency report (toolsd/rg/git/jq/gopls, missing-vs-required split).
- Lifecycle destroy ×4 (dfops0925/b/c/d) — all clean, local keys removed, registry back to the pre-run agent.

## Headline findings (full detail on the board)

1. **DF-BUNKER-71 (P1)** — release binaries still lose the client SSH key at spawn. The DF-BUNKER-59 fix
   (19892c3, 2026-09-25 01:31Z) is 597 commits ahead of the v0.1.4 tag; every live daemon AND the README-installed
   CLI predates it, so the spawn key fetch fails with `unauthenticated: missing Authorization header` and
   cp/deploy/ssh/mount/tunnel are all dead on those agents while exec/env/docker keep working. Proven both
   directions live against the same daemon: release CLI → warn + no key; HEAD CLI → key delivered, SSH family works.
2. **DF-BUNKER-72 (P1)** — rootless dockerd failed to start on a fresh spawned agent (dfops0925c):
   `[rootlesskit:parent] error: failed to lock /run/user/1007/dockerd-rootless/lock, another RootlessKit is
   running with the same state directory?`. uid 1007 had been recycled the same day from destroyed agent
   conctest-3-91761; the stale /run/user/1007 state dir survived the destroy. Agent showed `Status: running`
   throughout — no health signal. A manual user-unit start succeeded once the state was gone; next spawn on a
   fresh uid (dfops0925d) was clean. The kill -9 durability battery (run 5) proved registry replay; uid-recycle
   state hygiene is the uncovered gap.
3. **DF-BUNKER-73 (P2)** — no CLI recovery path for a keyless agent. GetAgentKey exists server-side (GAP-128);
   no command surfaces it. Suggested: `bunker key fetch --agent <id>`.
4. **DF-BUNKER-74 (P2)** — README's `curl ... raw.githubusercontent.com/.../v0.1.4/scripts/install.sh` 404s;
   the release-asset installer (`releases/latest/download/install.sh`) works and is FAST.

## Install leg (ephemeral bunker)

- Planned host bunker-las-03 was DOWN (`ssh 100.69.3.13: connect timed out`) — leg run on cube-las-00
  (100.84.195.43, rootless docker active) instead. Recorded here per the skill's no-silent-skip rule; the host
  substitution is itself the infra note.
- Spawned dfinst0925 (bare Debian user, no toolchains) → downloaded the release-asset install.sh → `sh install.sh`
  → both binaries 0.1.4 in ~/.local/bin + `smoke check OK` in **6 seconds** (INSTALL_SECONDS=6, cold).
- Friction: README raw-URL variant 404; installer warns `~/.local/bin is not on your PATH` — README quickstart
  does not mention the export. → DF-BUNKER-74.

## Performance (Step 2b)

- Headline operation timed: agent exec round trip 1.25s ± 0.04s warm (hyperfine, 10 runs, `bunker exec ... -- true`,
  remote agent on bunker-mvp). At this number a user never waits — no PERF row filed; a row here would be noise.
- Cold costs worth knowing (one-shot `time`): spawn 16.0–16.7s (includes rootless-docker bootstrap), cp 7.0s,
  stop 1.6s, start/restart 0.4s, install leg 6s, `homes` scan 2.5s for 581 entries.
- Nothing measured was slow enough to hurt: the run's pain was correctness (71/72), not latency.

## Verdict

🟡 **PROMISING-BUT-ROUGH.** The daemon/CLI pair at HEAD is genuinely good to operate: lifecycle semantics are
coherent (stop keeps state, restart resets TTL, destroy cleans keys), the residue tools classify honestly and
refuse to guess, agent-tools tells you exactly what is missing and why. The release channel, however, does not
deliver that system: a README-fresh user gets a keyless agent (dead SSH family) and, with bad uid luck, an agent
whose docker never starts while status says running. Both fixes exist or are small; the gap is release velocity
and one health signal.

## Run record

- Agents spawned/destroyed: dfops0925, dfops0925b, dfops0925c (HEAD CLI), dfops0925d, dfinst0925 (cube-las-00).
  All destroyed and verified via `bunker list` (bunker-mvp back to 3 pre-existing agents, cube-las-00 back to 1).
- No repo visibility/permission changes; no credentials minted or committed; tunnel process killed after probe;
  scratch build dir /tmp/bunker-src left out of the repo.
