# DF-BUNKER-77-IMGEXEC — live gate on bunker-mvp: **ALL CONJUNCTS GREEN** (2026-09-29)

**Row:** `DF-BUNKER-77-IMGEXEC` (P1) — image-exec path fails live on a real image-spec agent
(`bunker env set` cannot write the agent home; `docker info` reports a container user-namespace
mismatch), while the same two commands are GREEN on a plain agent.

**Fix under test:** commit `6e23390` (branch `wt/DF-BUNKER-77-IMGEXEC`) —
"fix(image-exec): write agent home + docker namespace parity from container context
(DF-BUNKER-77-IMGEXEC)". Tier-2 verdict `24b2201b` PASS (code conjuncts).

**Host:** bunker-mvp (78.46.173.180). **Live window:** 2026-09-29 **07:15Z** → **07:20Z**.
The CI battery (`e2e-full-battery.sh` pid 2140374 + nested regression) was running on the
shared host during the whole window — same contention discipline as the DF-BUNKER-77-LIVE run:
the gate ran on an ISOLATED coexist daemon, and the production daemon was NOT touched.

**Isolation shape:** coexist daemon from the CANDIDATE build (`/root/df77-imgexec/bunkerd`,
sha256 `d2fe9e36…`), REST `:28085`, gRPC `:29095`, agent ports `33000-33999`,
`registry.enabled: false`, own `base_data_dir` /var/lib/bunkerd-imgexec — disjoint from the
77-LIVE run's `:28083/:29093/32000-32999` and the CI battery's `:28081/:29091/30000-30999`.
`registry reconcile skipped` in the daemon log; the orphan walk never ran.

| # | Conjunct | Prior (45d7d44, 07fa266 fix) | Candidate 6e23390 | Evidence |
|---|---|---|---|---|
| A | `bunker env set <id> K=V` → rc 0 | ❌ `cannot create /run/bunker/<id>/env: Permission denied` | ✅ **rc 0**, value read back (`DF77_IE=2`) | §A |
| B | `exec <id> -- id -u` (agent identity, not foreign uid) | ⚠️ host uid 1071 (unmapped inside ns) | ✅ **ns uid 0** (the agent's own namespace identity) | §B |
| C | `exec <id> -- docker info` → rootless 29.x | ❌ `dial unix /var/run/docker.sock: no such file` | ✅ **Server Version 29.1.3, Storage Driver overlayfs, rootless, cgroup v2** — rc 0 | §C |
| D | agent-tools → git PRESENT (real image userland) | ✅ | ✅ `rg 14.1.0` (the spec's own package) + `git 2.43.0` | §D |
| E | agent HOME writable from exec context (prior §9 finding) | ❌ `Permission denied` on touch | ✅ **HOME-WRITABLE** (touch+rm rc 0) | §E |
| F | CONTROL: plain agent, same daemon/build | ✅ | ✅ env set rc 0, `docker info` rootless 29.1.3, id -u 1074 (SSH path, no image wrapping) | §F |

**Verdict: the live gate PASSES.** The image-exec path now writes the agent env file,
reaches the agent's own rootless docker socket from inside the exec container, and the
agent home is writable — while the plain-agent control is unchanged.

## Mechanism (matches the 77-LIVE doc's §12 fix directions 1–2)

The container joins the agent's rootless user namespace (`uid_map 0→<host-uid>`), so the
exec must run as **namespace uid 0** (what the agent itself is) instead of the demoted HOST
uid; `DOCKER_HOST=unix:///run/bunker/<id>/docker.sock` is now carried INTO the container via
`-e` (previously only the host-side docker CLI saw it); the env file is re-sourced inside the
container shell. Code: `internal/server/service.go` (`agentContainerIdentityFlag`,
`agentDockerHostEnv`, `imageInnerSourcePrefix`) + tests in
`internal/server/exec_image_agent_runtime_test.go` (RED/GREEN proven: restoring the old
`service.go` fails the new tests with the exact host-uid demotion fingerprint).

## Reproduction

1. `make build` in the worktree @ 6e23390 → sha256 bunker `34a75edd…` / bunkerd `d2fe9e36…`,
   scp to `bunker-mvp:/root/df77-imgexec/` (host hashes matched byte for byte).
2. `bash up.sh` — isolated daemon + image-spec agent `df77ie1` (spec: apt ripgrep,
   image `bunkerd-imagespec-a056ec2a0b57:latest`, spawn rc 0).
3. `bash conjuncts.sh` — the conjunct battery + plain-agent control above.
4. `bash down.sh` — destroy + stop + census: users 8→8, containers 1→1, no df77ie residue,
   no listener on :28085/:29095, agent home removed, production daemon `active` (pid 2016609,
   running binary 85d7927c = the 45d7d44 deploy — untouched by this run).

Scripts: `/root/df77-imgexec/{up,conjuncts,down}.sh` (adapted from
`.coding-hermes/evidence/df-bunker-77-live/scripts/`, commit 18171d2).
No token values in this document; the isolated daemon ran `auth.enabled: false`.
