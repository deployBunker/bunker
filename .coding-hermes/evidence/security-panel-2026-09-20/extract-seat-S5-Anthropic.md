# BUNKER SECURITY READINESS — PANEL SEAT REVIEW

**Artifact:** `github.com/deployBunker/bunker`, local `/home/kara/bunker`
**HEAD at read time:** `c50f7b3 dogfood: bunker DOES-NOT-DELIVER — 'bunker mount' cannot mount any agent`
**Review mode:** read-only. 3 live falsification experiments, all scratch in `/tmp/bunker_probe/` (nothing written to the repo, no git writes, no service restarts). Board read via `/tmp` scripts because `python3 -c` is policy-blocked in this session.

---

## 1. SIGN-OFF VERDICT

**DO-NOT-APPROVE.** The engineering discipline visible in this repo is genuinely above average — the audit chain, the PAM `/tmp` guard with a root-owned trust chain, fail-closed spawn rollback, and the durable registry are real, not aspirational, and my probes confirmed the hash chain catches edited and dropped records. But the artefacts a security team is asked to sign off *on* do not exist and, worse, two of them are affirmatively wrong: there is no threat model document at all (grep for "threat model" across `README.md` + all of `docs/` = 0 hits), and `SECURITY.md:24` claims "mTLS between `bunker` CLI and `bunkerd` server" in the default security model when `DefaultConfig()` sets `TLS.Enabled=false` and `auth.mtls=false` (`internal/config/config.go:373-380`) and the daemon serves plaintext-over-HTTP with no warning — and I proved the audit log's tamper-evidence does not cover its own tail, which is the one property a forensics tool is bought for.

**Single biggest reason:** the documented security model describes a product that is not the one that boots — there is no threat model, `SECURITY.md` overstates by default, and an attacker with write access to `/var/log/bunkerd/audit.log` (the agent's own host, in a box whose whole point is untrusted tenants) can destroy the newest records and forge replacement ones that `bunker audit verify` reports as `OK`.

---

## 2. CLAIM VERDICTS

### A — Control plane

**C-1 | SPLIT**
`SECURITY.md:23-28` lists 5 bullets. Only 2 are true by default; 1 is false by default; 2 are true only after a manual, out-of-band host step.

| Bullet | Verdict | Evidence |
|---|---|---|
| "mTLS between `bunker` CLI and `bunkerd` server" | **FALSE by default** | `internal/config/config.go:373-380` — `DefaultConfig()` `TLS.Enabled=false, MTLS=false, CAFile=""`. `config.example.yaml:17-26` ships the whole `tls:` block **commented out**. With TLS off, `server.go:259-263` calls `srv.ListenAndServe()` on `:9090` and `:8080` — plaintext HTTP. |
| "JWT-based API key authentication with scope enforcement" | **VERIFIED** | `internal/auth/jwt.go:194-210` (HS256 pinned via `jwt.WithValidMethods` + explicit `*jwt.SigningMethodHMAC` type check → `alg:none`/RS256 confusion rejected); `internal/auth/interceptor.go:122-133` master-only interceptor for the Bunkerd service vs. `server.go:213-215` permissive for the Agent service. |
| "cgroup resource limits (CPU, memory, PIDs)" | **VERIFIED (but sees only `CPUQuota`, `MemoryMax`, `TasksMax`; the family's missing swap/`memory.high`/`oom.group`/IO work is in flight per brief)** | `internal/agent/isolation.go:120-139` |
| "User namespace remapping for rootless containers" | **SPLIT → not a Bunker control** | `internal/agent/rootless.go:21-22` fetches `https://get.docker.com/rootless` unverified (no digest/signature), and that upstream script is what configures subuid/subgid + userns. The claim delegates to an unpinned third-party script. |
| "SSH key isolation per container" | **VERIFIED, but the mode contradicts the wording's implication** | Per-agent ed25519 keypair generated per spawn (`manager_spawn.go:261-266`), `0600` (`:341`), separated by `authorized_keys` per user. But every agent shares one supplementary group (`EnsureAgentGroupMembership`, `internal/hostsetup/scratch.go:295`) whose per-agent dirs are cross-writable `2770` (`scratch.go:40`) by design. The word "isolation" is the wrong word for a deliberately shared exchange group. |

The decisive point is not which bullet is false — it's that a reader of `SECURITY.md` cannot tell that four of the five are conditional on operator steps (`host-provision`, building from HEAD, setting `tls.enabled: true`) that the doc never names. README is more honest than `SECURITY.md` (`README.md:651-698` documents the fail-closed PAM path and the build-dependent isolation), which makes `SECURITY.md` the weaker document.

**C-2 | SPLIT — auth primitives are sound; one authorization check is missing from the Agent service.**
- Sound: HS256-only validation (`jwt.go:200-205`), `subtle.ConstantTimeCompare` on the static-token fallback (`jwt.go:153`), master-only separation (`interceptor.go:119-133`), JWT secret floor of 32 bytes (`jwt.go:79-81`), `GenerateSecret` from `crypto/rand` (`jwt.go:83`), sub-keys from `crypto/rand` with SHA-256 at rest (`internal/apikey/manager.go:47-56`).
- **Finding:** `agentService.Metrics` scopes correctly — `service.go:1360-1362` returns `CodePermissionDenied` when `req.Msg.AgentId != claims.AgentID` (this is the landed `DF-BUNKER-28` fix). **But `agentService.Heartbeat` (`service.go:1422-1440`) has no ownership check at all.** I grepped the whole handler: the only `claims.AgentID` reads in the range are the two in `GetInfo` and `Metrics`; the sole `CodePermissionDenied` in the range is `Metrics`' line 1361. An agent-scoped sub-key can call `Heartbeat(agent_id=<other agent>)` and extend a **peer's lifetime indefinitely**, 6h per call, with no `PermissionDenied`. `agent_interceptors` uses the permissive `NewJWTAuthInterceptor` (`server.go:215`), which is exactly the interceptor that admits agent-scoped keys. Severity: cross-tenant TTL manipulation, not data read — but it is an authorization bypass on an RPC and it is not covered by any of the panel's prior rows.

**C-3 | CONTRADICTED — the daemon does not refuse, does not warn, and silently serves plaintext.**
`DefaultConfig()` → `TLS.Enabled: false` (`config.go:373`). `config.Validate()` (`config.go:565-592`) only checks TLS *internals* when `Enabled` is true; it never complains that TLS is off, never compares the bind address to loopback, and never rejects a wildcard bind. `CheckAuth()` (`config.go:656-664`) is the *only* startup gate and it guards the **auth** dimension — it fires only when `auth.enabled: true` with no credential, and merely prints a loud string when auth is explicitly disabled. There is no equivalent for TLS. Result: a shipped-default `bunkerd` binds `:9090`/`:8080` on all interfaces over HTTP and logs `"bunkerd gRPC listening"` with `"tls": false` (`server.go:258`) — a structured log line, not a refusal or a warning. For contrast, the repo proves it knows how to do this correctly: `agentMgr.RegistryError()` fails the daemon *before opening any listener* when persistence is unavailable (`server.go:193-196`). The same treatment is owed to TLS.

**C-4 | VERIFIED**
Cross-agent action is gated by ownership, not by server-side operator privilege. `SpawnAgent` passes `req.Msg.AgentId` into `agentMgr.Spawn`, whose first act is `validAgentID.MatchString(agentID)` against `^[a-z0-9-]{1,63}$` (`manager.go:25`, `manager_spawn.go:43-44`) — no `/`, `.`, uppercase, or space can reach a path or a shell word; and `DestroyAgent` (`service.go:374-375`) reaches a `validAgentID`-gated path (`manager_destroy.go:194`), as does the key-removal helper (`ssh_key_cleanup.go:39-47`). The single exception is the C-2 Heartbeat lifetime extension, which affects state, not reading or destroying another agent's data.

**C-5 | SPLIT — stronger for destroy, weaker for renew/register.**
`DestroyAgent`, `StopAgent`, `StartAgent`, `RestartAgent`, `ListAgents`, `GetAgent`, `AgentMetrics`, `ExecAgent`, `RunAgent`, `QueryAudit` all live on the Bunkerd service, which mounts `NewMasterOnlyAuthInterceptor` (`server.go:213`) — i.e. `masterKeyOnly` rejects any token carrying an `agent_id` claim (`jwt.go:164-166`). That is *at least* as strict as spawn, as required. The weakness is on the Agent service side: `GetInfo`/`Metrics` correctly scope to `==` the caller's own agent, but the **renewal** surface (`Heartbeat`) does not — so "renew" is *less* strictly authorized than spawn. `Register` has no RPC at all (no such method in `proto/bunker/v1/bunker.proto`); registration is CLI-client-local (`internal/cli/config.go:136-140`), which sidesteps the question but also means there is no server-side revocation of a registered client.

---

### B — Agent isolation

**C-6 | VERDICT: the wording is genuinely indefensible as written — re-word, do not delete.**
`SECURITY.md:29` says root-in-own-container is "out of scope (containers are designed for root access)". Two problems. First, **it is not true of this architecture**: rootless Docker inside a userns-remapped per-user daemon, plus the distinct per-agent Linux users, means container-root maps to a subuid — container root is *not* host root, and the doc's rationale ("containers are designed for root access") is a property of rootful Docker, not of what Bunker builds. Second — and this is the security-team risk — "out of scope: compromise of the host system by a user with root in their own container" is exactly the phrase a reviewer reads as *"they have not thought about the shared-host boundary."* In a design whose entire selling point is *20+ mutually-untrusted agents on one bare-metal host*, the strongest plausible adversary is root-in-container, so dismissing them in one half-sentence while README spends 50 lines on `/tmp` isolation reads as a doc written for a single-tenant host and reused for a multi-tenant product.

Recommended replacement wording (three sentences, all of which the code already supports): *"Bunker's isolation boundary is the per-agent Linux user and its private user namespace; container root does not grant host root (rootless dockerd + userns remap, per-agent subuid/subgid). We therefore treat as IN SCOPE: cross-agent access via shared host paths, the shared agent group, TCP-reachable services, and the control plane. We treat as OUT OF SCOPE: kernel privilege-escalation CVEs (a container escape to host root is a kernel vulnerability, patched by kernel updates, not by Bunker), and physical access. Hosts running untrusted tenants must be treated as a trust boundary: kernel patch cadence is a Bunker deployment requirement, not a Bunker feature."* That is a signable scope statement; the current one is not.

**C-7 | VERIFIED, not aspirational — with one caveat.**
- Own dockerd per agent: `isolation.go:86-139` builds the `systemd-run --system --unit=… --uid=<uid> --gid=<gid>` argv with `DOCKER_HOST=unix://<per-agent socket>` (`:102`).
- Per-session private `/tmp`: `--property=PrivateTmp=yes` (`isolation.go:118`); SSH sessions get `pam_namespace` behind a **verified trust chain** — a name-pattern `pam_succeed_if` classifier, then a root-owned `pam_exec` precondition helper whose bytes hash against a root-owned manifest (`README.md:667-686`), and the module line deliberately carries no `ignore_config_error`. This is the strongest piece of engineering in the artifact.
- **Caveat:** `isolation.go:77-80` documents that `rootlesskit v1.1.1` cannot support `--detach-netns`, so `--pidns` is *intentionally omitted*. The agent's containers share the daemon's PID namespace view in the documented sense that the flag is off. That is an honest, dated disclosure — but it belongs in `SECURITY.md`, not only in a Go comment, because "own dockerd" is not "own PID namespace."

**C-8 | ANSWERED — yes, several, and two of them are load-bearing gaps.**
An agent user still has, beyond its own container:
1. **A shared group whose members can write into each other's directories.** `ScratchDirMode = 0o2770` (`scratch.go:40`) — group-writable, setgid — and every agent is a member of `bunker-agents` (`scratch.go:295-305`), which is verified explicitly by the code comment at `scratch.go:293-294`: *"it is also what makes setgid files readable across agents."* So agent A reads and writes agent B's exchange directory by construction. The cap is per-directory `size=<256 MiB>` tmpfs (`hostsetup.go:176`), which bounds bytes but not confidentiality.
2. **A reachable network surface per agent.** Each agent owns a 100-port sub-range from a `10000-19999` pool (`config.go:390-392`); the `port_range` metadata is written world-readable `0644` (`manager_spawn.go:602`) and then chowned to the agent. Whether a peer can *reach* those ports is a function of the deploy topology, which is exactly why it has to be written down (see egress row in §3) — the repo has no statement either way.
3. **`/tmp` is shared before provisioning.** `README.md:653-656` states plainly that on a fresh, unprovisioned host, agent SSH sessions see the host `/tmp`. Honest, but it means the default *install* has agents sharing a filesystem with each other and with root's daemon.
4. **The host tmpfs itself.** `QA-BUNKER-5` (board, `retired`) documents `bunker-las-03` having a host-wide 16G tmpfs shared by root **and** all agents' containers. Retirement of that row without a replacement control is a gap I'd flag on the board.
5. **`/run/bunker/<id>/`** — the per-agent runtime tree (`env`, `tmp`, `docker.sock`). Not asserted as an isolation boundary by the code itself (`isolation.go:103-106` says so explicitly), but it *is* a shared-namespace path where the agent's docker socket lives.

---

### C — Provisioning & supply chain

**C-9 | VERIFIED for the injection class, with one TOCTOU-shaped acknowledgement.**
Every `useradd`/`userdel`/`chown`/`systemd-run` call site passes argv as a **slice**, never a shell string: `exec.CommandContext(ctx, "useradd", "-m", "-s", "/bin/bash", username)` (`manager_spawn.go:222`), `exec.CommandContext(ctx, "chown", "-R", username, sshDir)` (`:294`), `exec.CommandContext(ctx, "userdel", "-rf", username)` (`manager_destroy.go:349`). `username` is always `"bunker-" + agentID` with `agentID` already gated by `^[a-z0-9-]{1,63}$` (`manager_spawn.go:220`). No `sh -c` on any user-facing value in the provisioning path.

The one place agent-influenced data becomes a **string** is the rootless installer, and it is the correct construction: `rootlessInstallerSessionCmd` → `sessionCommand(runtimeDir, installerPath, …)` → `sessionEnvPrefix(...) + script`, and the code is explicit that the script is appended **byte-identical** and unquoted (`rootless.go:161-169`). That is safe only because `installerPath = filepath.Join(userHome, "rootless-install.sh")` (`rootless.go:656`) and `userHome = /home/bunker-<validated-id>` — for which `rootless.go:171-176` provides a correct `shellQuote` helper that this path does not need. I rate it VERIFIED but note it as a **latent hazard**: the invariant "every value reaching `sessionCommand` is `validAgentID`-derived" is enforced nowhere and documented only in a comment. One future call site that passes a config-supplied string to `sessionCommand` turns this into `su - bunker-x -c '<injected>'`.

TOCTOU: `useradd` → `lookupAgentUser` → `provisionIsolation` (`manager_spawn.go:222-256`) is a read-after-write on the user DB, and `spawn_uid_recycling_test.go` / `spawn_recycled_uid_residue_test.go` exist precisely because recycled UIDs caused real incidents (`INT-SPAWN-001/002/003`, `DF-BUNKER-21`). The mitigation is the session probe plus rollback (`manager_spawn.go:621-638` fails closed if the session doesn't work), which is the right shape.

**C-10 | CONTRADICTED — nothing in the install path is pinned or verified.**
- Rootless Docker: `const rootlessInstallURL = "https://get.docker.com/rootless"` (`rootless.go:21-22`), fetched with `curl -fsSL -o <path> <url>` (`rootless.go:242`). No `--proto`/`--pinned-key`, no SHA-256, no GPG, no signature. The cached variant validates only "non-empty / copyable" and, on the fallback, is described as *"treats a partial or invalid cache entry as a miss and refreshes"* — i.e. **there is no notion of "valid" beyond present** (`rootless.go:293-352`). One compromised or spoofed response runs a root-context script inside the agent user on every host the fleet provisions.
- Bunker's own binaries: `go install github.com/deployBunker/bunker/cmd/bunker@latest` — the board's own history is a catalogue of this path serving stale/None code (`GAP-027`, `GAP-043`, `GAP-046`, `GAP-081`). No checksum file, no cosign signature, no release-attestation in the repo; `internal/releasecheck/` verifies the *build pipeline's* git state, not the artifact a user downloads.
- Version skew is the same class: `internal/hostsetup/daemonversion.go` + `MinDaemonVersion` with the board flagging that the floor "rests on a false statement" (`GAP-082`).

---

### D — Audit & forensics

**C-11 | CONTRADICTED.** I built a 3-record chain, then ran the real `bunker audit verify` against four variants. Results are from `/tmp/bunker_probe/bunker` (built from HEAD `c50f7b3`):

```
A) pristine                
   audit log .../chain/audit.log: OK (3 records)                                   exit=0
B) mid-record field edit (no hash recompute)
   tamper detected at record 2 — record 2: hash mismatch (tampered)                exit=1
C) TAIL TRUNCATION (newest record deleted)
   audit log .../audit-truncated-tail.log: OK (2 records)                          exit=0   <-- FALSE CLEAN
D) middle record dropped
   tamper detected at record 2 — record 2: prev_hash does not chain (tampered)     exit=1
E) FORGED APPEND re-using the true chain head
   audit log .../audit-forged-append.log: OK (4 records)                            exit=0   <-- FORGED RECORD ACCEPTED
```

Findings:
- **B and D are genuinely caught.** The per-record SHA-256 and the `prev_hash` link do their job on edits and interior deletions. That part of the claim is real.
- **C is a false clean.** Deleting the newest N records — the single most common real tamper, "remove my tracks" — leaves a shorter prefix of the *same* valid chain, and `verify` reports OK with exit 0. `audit.Verify` (`verify.go:37-87`) has no external anchor: it checks internal consistency only. With no heartbeat, no shipped copy, and no seal, it cannot know 3 records were ever written.
- **E is a false clean for the same reason** — and worse, it is *constructive*: the chain is an **unkeyed** SHA-256, so anyone who can read the log can compute a valid successor to the true head and append arbitrary records (method, caller, outcome, summary) that `verify` accepts. `docs/audit.md:41-53` advertises the chain as tamper-evidence without stating that it is unkeyed by default. Only the opt-in `audit.seal_key` **at rotation time** adds an HMAC (`audit.go:53-61`), and by default `seal_key` is empty and `ship_to` is empty (`config.go:117-122`; `bunker audit status` on the forged file reported `shipping: disabled / rotation sealing: false`).
- **What an agent-side root attacker can still do** (the panel's actual question): the audit log lives at `/var/log/bunkerd/audit.log` — root-owned `0600` (`audit.go:130`), so a container-root agent cannot write it directly, but the whole document is on the same host the untrusted tenant occupies. Against a host-root-adjacent attacker the chain degrades to exactly C and E. The honest claim is: *"tamper-EVIDENT against edits and interior deletions; tamper-DETECTABLE against tail truncation and forgery only when `audit.seal_key` and/or `audit.ship_to` are configured off-host."* That sentence is not in the docs.
- Minor but real: `docs/audit.md:44-53` says "The live log rotates at 5 MiB, keeping 3 backups" with no caveat that files rotated beyond the budget are simply gone — while `status.go:31-34` is careful to call its own count a *lower bound*. The query doc is less honest than the code.

**C-12 | SPLIT.** A responder gets more than most tools of this size, but three things are missing and one is a filter you can't express.
Present and good: `docs/audit.md:31-42` documents the record schema (`ts`, `caller`, `method`, `remote_addr`, `agent_id`, `duration_ms`, `outcome`, `summary` + chain fields); `bunker audit list --agent/--method/--since/--until`, `export` (lossless JSONL), `verify` (chain), `status` (chain head, sizes, rotation lower bound, shipping state, `--json`) — `internal/cli/audit.go:211-237`, `status.go:13-59`. `audit list --server` queries a remote daemon over `QueryAudit`.
Missing:
1. **No hash-chain fields in the query surface.** `bunker audit status` surfaced `last verify: n/a` on a file I had just verified — the CLI does not record, and the query path does not expose, per-record `hash`/`prev_hash` for spot-checking. An examiner cannot pull "record 4,123 and prove it chains" without hand-rolling the digest.
2. **No agent-command visibility from the daemon trail.** The daemon record for `ExecAgent` carries method/agent/outcome/duration, **not** the command line (`audit.go:29-40`); exec content comes from snoopy via syslog (`docs/exec-audit.md:1-30`), i.e. a completely separate system, on a *different clock*, not correlated to the daemon chain. There is no join key documented between the two. That is the single biggest forensic hole: "which command did agent A run at 14:03" requires correlating two stores by timestamp and uid.
3. **Only authenticated requests are recorded.** By deliberate design the audit interceptor sits *inside* auth (`server.go:229-234`), so failed-auth attempts produce no record at all — there is no record of brute-force or credential-stuffing against the control plane. A SOC's first question is "denied attempts," and the answer is "we don't log those."
4. Attribution: `caller` is `master` for every static-token call and static token is a single shared secret, so all human operator action on a team host is indistinguishable. `GAP-095` (board, pending) already names this.

---

### E — Secrets

**C-13 | SPLIT — file modes are right; the storage model underneath them is not.**
- **Per-agent SSH private keys: VERIFIED and well-handled.** `0600` on disk (`manager_spawn.go:341`), parent dir `0700` (`:338`), held in memory only long enough to return, temp copy removed on both the success (`:563`) and rollback (`:167-169`) paths, and cleanup scoped by `validAgentID` + non-empty `SSHDir` + regular-file-only (`ssh_key_cleanup.go:39-47`). `DF-BUNKER-24` (board) documents the real historical leak — 139/139 orphans on `las-03` — and the fix converged every teardown path onto one helper.
- **JWT secret: VERIFIED as a value, not as a store.** Minimum 32 bytes, `crypto/rand` (`jwt.go:78-87`); never logged. But it lives in the daemon YAML and in `config.example.yaml:32` as a placeholder — a long-lived symmetric secret in plaintext config, no OS keyring, no file-mode enforcement checked at load, no rotation path. `config.Load` (`config.go:474-534`) reads env overrides for `auth.jwt_secret` via viper `BindEnv`, so in practice it lands in a systemd unit or an env file.
- **bunkerd static token: same class** — one shared secret, plaintext, grants full master authority (`interceptor.go:29-31`, `auth.go:95-100`).
- **Agent-scoped sub-keys: CONTRADICTED on "where stored."** `apikey.Manager` keeps keys in an **in-memory map only** — `keys map[string]*Key` (`manager.go:17-21`), no file I/O anywhere in the package (0 hits for `os.Open`/`os.WriteFile`/`json.Unmarshal`). There is no `Revoke` RPC in the proto and no persistence, so: (a) every sub-key a spawned agent receives is dead after the next daemon restart, and (b) there is no way to revoke one short of restarting the daemon. For a team-use story that is both an operational trap and an incident-response gap.
- **What is exposed to the agent itself:** the agent's *private* SSH key is not in the agent's home (it is server-side at `SSHDir/<id>`), which is correct. But the agent **owns** its persisted key file's directory entry via the `chown` chain and can read its own `/run/bunker/<id>/env` (`service.go:1002`), and per C-8 it can read peers' `2770` scratch dirs. Nothing in the agent home holds the daemon's master token — that is fine.

---

### F — Operational security

**C-14 | The most likely real incident: a shared-host lateral move from a compromised agent's dependency to production data, with no means to reconstruct it and no control to stop it.**
Concrete scenario, using the product as intended: a team onboards 15 engineers, each with a per-user agent on one bare-metal host. One engineer's agent installs a dependency with a postinstall hook (the normal, unremarkable case that the whole "20+ agents, one host" pitch is built around). The hook runs as `bunker-<id>`, enumerates `/srv/bunker-share/*` — **it can read and write every peer's scratch directory, `2770` + shared `bunker-agents` group by design** (`scratch.go:40`, `scratch.go:293-305`) — harvests whatever colleagues exchanged there (their credentials, build artifacts, `.env` files copied through the sanctioned exchange point), and exfiltrates outbound. Nothing stops the egress: there is no egress policy in the code, and the board's `GAP-076` ("Per-agent network egress policy: agents currently reach the internet freely") is **pending**, with no default. Seven days later the team asks "who read the shared scratch, and when?" The daemon audit trail records the `ExecAgent` call but **not the command** (`audit.go:29-40`); the only command-level record is snoopy in a separate syslog store (`docs/exec-audit.md`); and `caller` is `master` for every operator, so the trail cannot say which human. Then someone runs `bunker audit verify` on the log — which is exactly the moment I proved it reports `OK` on a truncated or forged file (experiment C/E above).

**The missing control is egress policy, not more logging** — you cannot log your way out of a data-exfiltration path that has no deny rule. That is `GAP-076`, and it is the one gap that turns the other gaps from "embarrassing" into "disqualifying."

---

## 3. THE SECURITY-TEAM GAP LIST

| ID | Type | What it is | Why a security team demands it | Severity |
|---|---|---|---|---|
| **SEC-01** | MISSING | **Threat model document.** `grep -ic "threat model"` over `README.md` + all 9 `docs/*.md` = **0**. | Cannot approve a multi-tenant isolation product with no written adversary/asset/boundary/trust model. It is the first document a SOC-2 or regulated reviewer asks for, before any code. | **BLOCKER** |
| **SEC-02** | DOC-UPGRADE | `SECURITY.md:24` claims mTLS "between CLI and server" while `DefaultConfig()` is `TLS.Enabled=false` (`config.go:373`). | A stated security model that misdescribes the default is worse than no model — it is the sentence an assessor will quote back. | **BLOCKER** |
| **SEC-03** | ADD | **TLS/plaintext startup gate.** The daemon serves HTTP on `:9090/:8080` by default with no refusal and no warning (`server.go:259-278`); only auth has a gate (`config.go:656-664`). Require explicit `tls.enabled: false` acknowledgement, refuse wildcard bind without TLS, refuse plaintext when the bind address is not loopback. | An unauthenticated-sniffing posture on the control plane is an instant finding. The pattern already exists in-repo: `RegistryError()` fails the daemon before any listener opens (`server.go:193-196`). | **BLOCKER** |
| **SEC-04** | ADD | **RBAC / multi-operator identity.** Today there are two roles — `master` (one shared static token) and agent-scoped (`internal/auth/jwt.go:22-26`). No per-operator principal, no roles, no auditible human identity. | Team use *is* multi-operator. "Who destroyed agent X" must name a person. `GAP-095` names the symptom; the fix is an identity model, not a field. | **HIGH** |
| **SEC-05** | ADD | **Control-plane denial logging.** The audit interceptor sits inside auth (`server.go:229-234`) so failed-auth attempts are never recorded. | Brute-force / credential-stuffing detection is table stakes; a SOC asks "show me denied attempts" first. | **HIGH** |
| **SEC-06** | ADD | **Anchored tamper-evidence.** Chain is unkeyed SHA-256 by default; tail truncation and forgery both verify `OK` (proved). `audit.seal_key`/`audit.ship_to` are opt-in and off (`config.go:117-122`). | Forensics tooling that silently blesses truncation is a liability, not a control. Make off-host shipping + sealing the *default*, or state the limitation in the docs and in `verify`'s own output. | **HIGH** |
| **SEC-07** | ADD | **Egress control / default-deny outbound.** No egress policy exists; `GAP-076` pending. | Without it, one compromised dependency is an exf
triltration path with no control. This is the highest-value single addition to the product. | **BLOCKER** |
| **SEC-08** | ADD | **Shared-scratch confidentiality model.** Per-agent dirs are `2770` + shared group (`scratch.go:36-40`), explicitly cross-readable/cross-writable (`scratch.go:293-305`). Byte-capped (256 MiB, `hostsetup.go:176`) but not access-controlled per peer. | The "sanctioned exchange point" is, as built, an unauthenticated lateral channel between mutually-untrusted tenants. Either scope exchange pairs explicitly, or document it as "treat the scratch as public to all agents" and stop calling it isolation. | **HIGH** |
| **SEC-09** | ADD | **Image / supply-chain policy.** No allow-list, no signature verification, no digest pinning for the rootless installer (`rootless.go:21-22,242`) or for the agent image specs (`internal/imagespec/`). | "Which images may my engineers pull" and "is the 93 MB script you run as root verified" are the two questions a supply-chain reviewer leads with. | **HIGH** |
| **SEC-10** | ADD | **Key/token lifecycle.** `apikey.Manager` is in-memory only (`manager.go:17-21`), no persistence, no `Revoke` RPC, no rotation for `auth.jwt_secret` / `auth.token`. | Expiry-after-restart and unrevocable credentials are an incident-response failure on their own. | **HIGH** |
| **SEC-11** | ADD | **Heartbeat ownership check.** `service.go:1422-1440` has no `claims.AgentID` comparison, unlike `Metrics` (`:1360-1362`). | An agent-scoped key can extend any peer's TTL indefinitely — the clearest authorization bug I found. | **HIGH** |
| **SEC-12** | ADD | **Command-content logging inside the daemon chain.** Exec/run command lines land only in snoopy/syslog (`docs/exec-audit.md`), uncorrelated to the audit chain (`audit.go:29-40`). | "Which command, by which agent, under which caller" must be one joinable record or incident reconstruction is guesswork. | **MED** |
| **SEC-13** | TUNE | **`bunker audit status` / `verify` should surface their own limits.** `status` printed `last verify: n/a` immediately after a successful verify; nothing reports "no off-host anchor configured." | A tool must not overstate its own guarantees to the responder relying on it. | **MED** |
| **SEC-14** | ADD | **Compliance posture statement.** No data-residency, retention, or deletion guarantee documented; `destroy_home_policy` defaults to **archive**, not purge (`config.go:430`). A team deleting an agent's data expects deletion. | Regulated-industry reviewers ask "where does the data live, how long, and does delete mean delete" before anything else. The archive default is a good safety choice that is a compliance surprise. | **MED** |
| **SEC-15** | ADD | **Operator incident runbook.** No document covers: kill-switch for a compromised agent, emergency token rotation, how to freeze a host mid-incident, who to notify. | Doctrines live in runbooks, not in a `SECURITY.md` contact email (`SECURITY.md:11`). | **MED** |
| **SEC-16** | DOC-UPGRADE | **PID-namespace and `/tmp` state disclosures belong in `SECURITY.md`, not only in code comments** (`isolation.go:77-80`; `README.md:653-656`). | The README is more honest than the security doc. Reviewers read `SECURITY.md`. | **MED** |
| **SEC-17** | TUNE | **`verify` on an attacker-writable host should say so.** A forensics command that runs on the box it is auditing should print that anchor/shipping is required for real tamper-detection. | Prevents a false "the chain is intact" conclusion in the incident writeup. | **LOW** |
| **SEC-18** | ADD | **Vulnerability disclosure SLA + CVE process.** `SECURITY.md:19` promises 48h response; there is no embargo policy, no advisory publication path, no security-release channel. | External researchers and a regulated buyer both check this. | **LOW** |

**The first three things a SOC-2-adjacent reviewer asks, in order:** (1) *"Show me your threat model and your trust boundaries"* → nothing exists (SEC-01). (2) *"Show me that the default install is safe"* → the default install is plaintext HTTP with no warning (SEC-03). (3) *"Show me who did what, and prove the log wasn't altered"* → `caller` is `master` for everyone, failed auths aren't logged, and I demonstrated a silent-OK on truncation (SEC-04/05/06).

---

## 4. DOC UPGRADES

1. **`SECURITY.md` — REWRITE (currently inadequate, 30 lines, `SECURITY.md:21-30`).** Must contain: (a) a real scope statement including *what is in scope* (cross-agent access, control plane, shared paths, egress) — not just three half-sentences of "out of scope"; (b) per-bullet **defaults and preconditions** — every one of the 5 bullets annotated with "default on/off" and "requires `host-provision` / TLS config / HEAD build"; (c) the PID-namespace and shared-`/tmp`-before-provisioning disclosures; (d) the audit chain's actual guarantee (*tamper-evident for edits/interior deletion; tamper-detectable for truncation/forgery only with `seal_key`/`ship_to`*); (e) the trusted-tenant assumption stated explicitly or refuted.
2. **`docs/threat-model.md` — CREATE (owed, and the single highest-leverage doc).** Assets (agent home, scratch, control plane, host); adversaries (malicious agent, compromised dependency, curious insider, host-level attacker); trust boundaries (agent ↔ peer agent ↔ daemon ↔ host root); explicit non-goals; per-boundary control mapping to file:line; residual risk register with the board row that owns each.
3. **`docs/operations-runbook.md` — CREATE.** Compromise triage (identify agent → `DestroyAgent` → `seal_key`/`ship_to` verification → peer audit), emergency credential rotation, host freeze/isolation procedure, escalation path. Note that `bunkerd` currently has no per-agent kill switch beyond destroy — that itself should be on the board.
4. **`docs/audit.md` — PATCH (currently overstates).** Add the unkeyed-chain caveat and the truncation/forgery limitation to §"Rotation and the hash chain" (`docs/audit.md:44-53`), add the "rotated-away files are gone" caveat that `status.go:31-34` already knows about, and document the snoopy↔daemon correlation gap (no join key today).
5. **`docs/exec-audit.md` — PATCH.** Currently reads as a complete answer to "what did the agent run"; must state that it is a *separate, uncorrelated* store. Fix the mojibake (`documents`/`agre`/`Ä`) which suggests an encoding defect in the doc pipeline.
6. **`docs/compliance.md` — CREATE (new).** Data residency, retention, `destroy_home_policy: archive` semantics, and what "delete an agent" actually deletes.
7. **`README.md` — ALREADY GOOD.** `README.md:651-698` is the most honest security writing in the repo and should be the *source* for `SECURITY.md`, not the exception to it.

---

## 5. TOP-10 RANKED FIXES

| # | Fix | Sev | Effort | Why it is a security-team demand |
|---|---|---|---|---|
| 1 | Write `docs/threat-model.md`; rewrite `SECURITY.md` with per-bullet defaults + preconditions | BLOCKER | M | Nothing can be approved without a stated model; the current one misleads on defaults |
| 2 | Refuse/warn at startup on plaintext or wildcard bind when TLS is off (mirror `RegistryError()`'s fail-before-listen pattern) | BLOCKER | S | Safe-by-default is the baseline bar; the pattern already exists in `server.go:193-196` |
| 3 | Default-on off-host audit shipping + rotation sealing; make `verify` print "no anchor configured" | HIGH | M | Closes the demonstrated false-OK on truncation and forgery |
| 4 | Implement egress control (`GAP-076`) with a default-deny option | BLOCKER | L | The exfiltration path in C-14 has no control today |
| 5 | Add `claims.AgentID` ownership check to `agentService.Heartbeat` (`service.go:1422`) | HIGH | S | Real cross-agent TTL manipulation via an agent-scoped key |
| 6 | Per-operator identity + roles; stop collapsing every human to `caller:"master"` | HIGH | L | Team use is multi-operator; attribution is a SOC-2 control |
| 7 | Audit denied/unauthenticated attempts (move or duplicate the interceptor outside auth) | HIGH | S | Brute-force detection is table stakes |
| 8 | Persist + revoke sub-keys; rotate `jwt_secret`/`token`; wire a `Revoke` RPC | HIGH | M | Unrevocable, restart-fragile credentials are an IR failure |
| 9 | Define the shared-scratch confidentiality model (pair-scoped, or documented-as-public) | HIGH | M | The "exchange point" is an unauthenticated lateral channel today |
| 10 | Pin/verify the rootless installer (digest + signature) and add an image allow-list | HIGH | M | Largest unpinned root-context execution in the product |

---

## 6. REPEAT-OFFENSE SECTION

**(a) Credential/key paths leaking.** The `DF-BUNKER-24` class — agent private keys surviving their agent — is *fixed at the helper* (`ssh_key_cleanup.go:39-47`) but the *owning bug class* is not: any new teardown path that forgets to call the helper re-creates it, and there is no invariant test asserting "keys dir ⊆ live agent ids." Live evidence of the residual: the board's own `DF-BUNKER-24` title records **139/139 orphans on `las-03`**. The `apikey` in-memory map (`manager.go:17-21`) is the same class in the other direction — a credential whose lifecycle is process state, not durable truth — which is exactly the pattern `DF-BUNKER-29` fixed for the audit chain head (`audit.go:137-144`) and nobody fixed here.

**(b) Success-marker string checks treated as state.** `manager_spawn.go:224-231` decides "user already exists, reuse it" by `strings.Contains(string(out), "already exists")` — parsing human-readable output of `useradd` to infer identity state, on the exact code path where a wrong inference means re-using another agent's home, key, and dockerd data. Related: the trust chain in the PAM guard (`README.md:678-682`) is verified by hashing helper bytes, which is the *right* way; the `useradd` marker is the wrong way, in the same file family.

**(c) State ladders.** `spawn_failure.go` and the `rollback()` closure (`manager_spawn.go:167-217`) carry a multi-stage ladder with `rbRes.ok/failed/notice` bookkeeping; the file's own comments (`manager_spawn.go:180-184`) confess the prior failure mode — *"a breadcrumb could assert a teardown that had failed."* The fix is right, but the ladder remains multi-stage enough that "did rollback actually complete" is asserted by breadcrumb strings rather than by re-probing host state.

**(d) Silent-skip paths.** (i) `server.go:60-81` — audit log open failure is **non-fatal**, and an invalid `ship_to` silently falls back to a non-shipping log; a host can run with a local-only trail and no signal on the wire. (ii) `route`-less / unwired seams: `internal/audit/audit.go:151-158` says recovery errors only `logWarn`. (iii) `config.Validate()` never checks TLS-off (`config.go:565-592`) — a silent skip of the deployment's most important safety question. (iv) `main.go:154-158` prints the auth-disabled warning to **stderr** on every start, which on a systemd host means it disappears into the journal.

**(e) Fixes that weaken a requirement to fit a lane.** `GAP-062`/`DOC-13` show the repo's habit of shipping the code and deferring the doc — fine — but `SECURITY.md` shows the same reflex on a *requirement*: the security model was never upgraded to match the code's own `README.md:651-698`, so the weaker doc stayed as the shipped claim. `docs/audit.md:44-53` likewise states a stronger chain than the code provides, while `status.go:31-34` is scrupulously honest about its own lower bounds — the doc was not carried to the same standard as the code.

**(f) Green-but-did-nothing.** `bunker audit verify` printing `OK (2 records)` exit 0 on a truncated chain is the purest instance in this artifact: a green checkmark asserting a property it cannot observe (proved, experiment C). Same shape: `bunker audit status` printing `last verify: n/a` immediately after a successful verify, and `GAP-043`/`GAP-046`/`GAP-081` — a documented install path serving code that is not HEAD — which is green-but-didn't-ship at the release layer.

---

## 7. ONE COMMAND THAT WOULD FALSIFY MY MAIN CLAIM

```bash
cd /home/kara/bunker && go build -o /tmp/bunker_probe/bunker ./cmd/bunker && \
python3 /tmp/bunker_probe/mkchain.py /tmp/bunker_probe/chain && \
/tmp/bunker_probe/bunker audit verify --path /tmp/bunker_probe/chain/audit.log
```

If that prints honest tamper-detection on a **truncated** chain — or if `audit.Verify` gains an external anchor (shipped-copy head comparison) so that deleting the newest records is caught — my finding **C-11 ("tamper-evidence does not cover the tail")** is falsified. My run: pristine chain → `OK (3 records)`, exit 0; same chain minus its newest record → `OK (2 records)`, exit 0; true chain plus a forged append → `OK (4 records)`, exit 0.

Secondary falsifier for **C-2/C-11 (Heartbeat scoping)**: `sed -n '1422,1452p' internal/server/service.go | grep -c 'claims.AgentID'` → my run prints **0**, while the same grep on `Metrics` (`:1349-1362`) prints **2** plus a `CodePermissionDenied`.

---

PANEL REVIEW COMPLETE

session_id: 20260920_061718_e039d4
