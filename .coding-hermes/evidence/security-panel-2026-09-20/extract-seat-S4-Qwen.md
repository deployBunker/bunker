BUNKER SECURITY READINESS — PANEL SEAT REPORT
Repo: github.com/deployBunker/bunker @ /home/kara/bunker
HEAD read: c50f7b3c53aa7254a195bfc1070a96aedd9c9426
Scope note: per brief, resource-limiting work (GAP-113..GAP-122 cgroup preset family) assumed in flight and excluded.

1. SIGN-OFF VERDICT

DO-NOT-APPROVE. The isolation engineering is genuinely good in places (master-only interceptors, hash-chained audit, fail-closed PAM guard), but the default transport posture is plaintext bearer-token service on all interfaces, the spawn RPC hands an agent SSH private key back over that same plaintext wire, and there is no threat model, no failed-auth logging, no egress story, and no per-operator identity — a security team approving this for its engineers would be signing a shared static token with no rotation, no revocation, and no record of who failed to authenticate.

Biggest reason: secrets-in-cleartext-by-default — auth.token rides plaintext HTTP on :9090/:8080 (internal/server/server.go:259-263, no TLS-required gate; internal/config/config.go has no TLS default), and the SpawnAgent response returns the agent SSH private key in-band (internal/agent/manager_spawn.go:718, internal/server/service.go:363-364).

2. CLAIM VERDICTS

C-1 | SPLIT | SECURITY.md:23-28 lists 5 mechanisms. Verified in code: JWT+scope auth (internal/auth/jwt.go:146-192; internal/server/server.go:178-179 master-only vs permissive interceptors); cgroup limits via systemd drop-in + systemd-run (internal/agent/manager_spawn.go:496,893-917; README.md:631-634); rootless dockerd per user (internal/agent/rootless.go:22,235-243); SSH keypair per agent (internal/agent/manager_spawn.go:261,311,341). CONTRADICTED parts: "mTLS between bunker CLI and bunkerd" is presented as a standing property but TLS is disabled by default and the MTLSAuth interceptor's context extraction is broken (internal/auth/mtls.go:88-99 uses ctx.Value(http.ServerContextKey).(*http.Request) — ServerContextKey carries the *http.Server, never the request, so the interceptor cannot read TLS state); and the doc says "per-user Docker containers" when the model is per-user dockerd + Linux users.

C-2 | SPLIT | Sound: HS256 pinned via jwt.WithValidMethods + HMAC type check (internal/auth/jwt.go:200-205); GenerateSecret enforces ≥32 bytes via crypto/rand (jwt.go:78-87); agent-scoped rejection on master-only endpoints (jwt.go:164-165,173-174); Agent service Metrics rejects foreign agent ids (internal/server/service.go:1360-1362). Not sound: TokenAuth compares with `token != a.token` — non-constant-time (internal/auth/interceptor.go:70), while the JWT path correctly uses subtle.ConstantTimeCompare (jwt.go:153); agentService.Heartbeat performs NO ownership check on req.Msg.AgentId (internal/server/service.go:1422-1452), so agent A's scoped token can extend agent B's TTL; failed-auth attempts produce no audit record by design (internal/server/server.go:208-212 audit interceptor composed inside auth) and no rate limiting exists anywhere in server.go.

C-3 | CONTRADICTED (not safe) | With TLS off the daemon does not refuse and does not warn about plaintext transport: server.go:238-246 builds tls.Config only `if s.cfg.TLS.Enabled`, else falls through to `srv.ListenAndServe()` (server.go:262, 278) on `:9090`/`:8080` (all interfaces). The only startup warning exists for auth DISABLED (internal/config/config.go:658); auth-enabled-over-plaintext is silent. README.md:104-119 Quick Start itself runs tokens over http://.

C-4 | SPLIT | Agent A cannot exec into agent B: ExecAgent/RunAgent/Destroy are on the master-only Bunkerd service (server.go:172-179), and agent homes are separate 0700-per-user. But a scoped agent token CAN act on agent B's TTL via the unchecked agentService.Heartbeat (service.go:1422), and before host-provisioning all agents share the host /tmp (README.md:45-58) — a same-host temp-file collision/snooping channel. Also /run/bunker/<id> dirs are MkdirAll 0755 (internal/agent/manager_spawn.go:358) and the env file under them carries `bunker env set` values readable on the host side.

C-5 | VERIFIED (with a design caveat) | Destroy/Stop/Start/Restart/Heartbeat(Bunkerd)/registry ops are all mounted under NewMasterOnlyAuthInterceptor (internal/server/server.go:178, 220-227) — strictly the same gate as Spawn. Caveat: "as strictly as spawn" is a low bar in a multi-operator world, because every master token is identical (one static token or master JWTs from one secret) — there is no per-human identity, so authorization exists but individual accountability does not.

C-6 | MUST RE-WORD | SECURITY.md:30: "Out of scope: ... compromise of the host system by a user with root in their own container (containers are designed for root access)." As written this is indefensible to a security-team audience: the product's headline is "complete isolation without ... full VMs" (README.md:3), yet the security model declares the escape class out of scope in the same breath. The honest wording is: rootless dockerd + user namespaces raise the escape bar and an escape then requires a kernel bug, but the boundary is NOT equivalent to a VM; residual risk depends on kernel currency, correct PAM/host provisioning (which is a manual step), and the fact that the daemon itself runs as root. Recommend a "Security boundary and residual risk" section stating exactly what contains what, instead of an out-of-scope line that reads as a disclaimer.

C-7 | SPLIT (holds conditionally) | Own dockerd: real — rootless installer + systemd-run bring-up per agent (internal/agent/rootless.go, manager_spawn.go:496). Userns remap: delegated to rootless Docker, real. SSH key isolation: real, 0600 keys / 0700 ssh dir (manager_spawn.go:290,311,341). The condition: /tmp isolation and the fail-closed PAM boundary require `bunker host-provision --apply` from a HEAD build (README.md:386-413); on an unprovisioned host or a tagged-release daemon, agent sessions share host /tmp and the newest release tag (v0.1.4, README.md:273) predates the isolation work entirely — so the shipped artifact does not carry the claimed isolation by default.

C-8 | ANSWER | Surfaces an agent user still has beyond its own container: (1) host /tmp shared until host-provisioning (README.md:45-48) — read/collide with other agents' temp files; (2) /srv/bunker-share group-readable exchange dir (README.md:736-753) — opt-in but membership in bunker-agents is granted to every agent at spawn (README.md:745-747), so anything an agent puts there is readable by all agents; (3) /run/bunker/<other-id>/ dirs are 0755 root-owned (manager_spawn.go:358) with env/port files created under them (manager_spawn.go:602 port file 0644) — host-side listing of other agents' metadata; (4) unrestricted egress to the LAN/internet from the agent user and its containers — no firewall, proxy, or docker network restriction anywhere in the repo; (5) sshd itself on the host is reachable, though only per-agent keys authorize (host_key.go:41). Direct read of another agent's home: not possible (separate users, 0700 homes).

C-9 | SPLIT (no shell injection; missing server-side validation) | All privileged calls are exec.Command argv arrays, never shell strings: useradd/userdel/chown/systemd-run/ssh-keygen (internal/agent/manager_spawn.go:222,261,294,496; manager_destroy.go:349) — agent-id cannot inject metacharacters. BUT the only agent-id validation is CLIENT-side regex ^[a-z0-9-]{1,64}$ (internal/cli/spawn.go:23); SpawnAgent (internal/server/service.go:304-371) re-validates TTL and image-spec but never the agent id before building username "bunker-"+id and filesystem paths — any gRPC client bypassing the CLI feeds the server directly. TOCTOU window exists between key write and chown (manager_spawn.go:311-341) but is root-owned-path-only, low risk.

C-10 | CONTRADICTED (unpinned critical dependency) | Bunker binaries: release assets with SHA256SUMS verified by install.sh before write (README.md:193-206) — good. Rootless Docker installer: `curl -fsSL https://get.docker.com/rootless` with NO checksum, signature, or version pin, then executed as the new user (internal/agent/rootless.go:22, 239-243; cached copy at rootless_installer_cache_dir also unverified, README.md:315-319). An unauthenticated ~93MB remote script runs inside every first spawn — a supply-chain hole in exactly the component the isolation claim rests on.

C-11 | SPLIT | Tamper-evident against agents: yes — append-only O_APPEND 0600 root-owned JSONL with sha256 hash chain + prev_hash (internal/audit/audit.go:47-51,130), HMAC-SHA256 rotation seals keyed by audit.seal_key (audit.go:53-61), verify spans rotations and catches mid-chain backup tampering (internal/audit/verify.go:13-40). Against a root-level attacker ON THE AGENT SIDE: that attacker is root only in a userns (unprivileged on host), so they cannot touch /var/log/bunkerd — but note the chain's root of trust is the same host: anyone with real host root (or a compromised root daemon) can rewrite the log AND regenerate a valid chain; only the optional seal key + optional remote shipping (audit.ship_to) detect that, and ship_to misconfig silently degrades to local-only (internal/server/server.go:68-74).

C-12 | SPLIT | Present for IR: QueryAudit with agent_id/method/since/until/limit (service.go:921-968), CLI verify/list/export/status, chain counts across rotation (verify.go). Missing for IR: FAILED AUTHENTICATION IS NEVER RECORDED (audit interceptor sits inside the auth interceptor, server.go:208-212) — an IR team investigating a brute-force or token-stuffing attempt gets zero evidence; exec records capture method+agent_id but there is no documented guarantee that the executed command line (or its hash) lands in the record; retention is just 5MiB × N rotation with no export-to-SIEM contract (README.md:351-361).

C-13 | SPLIT | Stored well: agent SSH private key 0600 in a 0700 dir (manager_spawn.go:290,341); authorized_keys 0600 (manager_spawn.go:311, host_key.go:41); audit log 0600 (audit.go:130); registry 0600 (README.md:280 of config); tokens never written to audit (server.go:210-212). Exposed badly: (1) bunkerd config.yaml carries auth.token / jwt_secret with NO permission check anywhere in internal/config/config.go — the README's own `tee` example creates it 0644 world-readable (README.md:103-111); (2) SpawnAgentResponse returns the SSH private key in plaintext over the wire (manager_spawn.go:718) and the API sub-key the same way (service.go:363-364) — over the default plaintext transport, both credentials traverse the network cleartext; (3) per-agent env file under a 0755 dir (manager_spawn.go:358, internal/cli/env.go:16-18) is the injection point for `bunker env set KEY=VALUE` secrets.

C-14 | SCENARIO | The likely incident: a team adopts Bunker; the single static master token lives in /etc/bunkerd/config.yaml (0644 from the documented install), shell histories, and CI variables. It leaks once (paste, backup, CI log). There is no TLS by default, no failed-auth record, no rate limit, and every master token is forever valid with no revocation surface. The attacker reaches :8080 from the LAN, spawns 50 agents as crypto miners or pivot hosts, or destroys the fleet — and the incident responder finds the audit trail has nothing about the failed probes, only successful RPCs. MISSING CONTROL set: TLS-required (or loopback-only) bind, per-operator scoped credentials with rotation/revocation, failed-auth logging + lockout, and token lifetime limits.

3. SECURITY-TEAM GAP LIST

G-01 | MISSING | Threat model document (assets, trust boundaries, attacker classes: malicious agent workload, tenant-vs-tenant, token thief, host-root) | every reviewer's first question; SECURITY.md's out-of-scope line is not one | BLOCKER
G-02 | MISSING | Plaintext refusal: daemon must refuse (or require an explicit --i-know flag) to serve non-loopback without TLS | credentials cross the wire in clear by default | BLOCKER
G-03 | ADD | Server-side agent-id validation (same ^[a-z0-9-]{1,64}$) in SpawnAgent before any useradd/path construction | only the CLI validates today; gRPC clients bypass it | HIGH
G-04 | ADD | Failed-auth logging + rate limiting on auth attempts | zero forensic signal for brute force today (server.go:208-212) | HIGH
G-05 | ADD | Egress policy story: per-agent firewall/nftables profile or documented proxy requirement | agents have unrestricted outbound; exfil and C2 are free | HIGH
G-06 | ADD | Per-operator identity: named credentials per human, rotation, revocation list | one shared static token = no accountability, blast radius = everything | HIGH
G-07 | ADD | Stop returning the SSH private key in SpawnAgentResponse over an unauthenticated-transport-possible channel; require TLS for spawn or deliver the key out-of-band | private key cleartext over the wire (manager_spawn.go:718) | HIGH
G-08 | ADD | Pin the rootless Docker installer by sha256 (or vendor it in the release); verify cached copies | unauthenticated remote script executed at every first spawn (rootless.go:239-243) | HIGH
G-09 | TUNE | Enforce/check 0600 on /etc/bunkerd/config.yaml at load; refuse or warn on group/world-readable | documented install creates it 0644 with the token inside | HIGH
G-10 | TUNE | Fix agentService.Heartbeat ownership check (mirror Metrics' 403 at service.go:1360) | scoped token can extend foreign agents' TTL | MED
G-11 | TUNE | Constant-time compare in TokenAuth (interceptor.go:70) | timing side channel on the static token | MED
G-12 | TUNE | /run/bunker/<id> dirs and env/port files: 0700 dir, 0600 files | env secrets sit under 0755 paths today | MED
G-13 | ADD | Fix or delete the MTLSAuth interceptor (broken context extraction, mtls.go:88-99) and stop claiming mTLS as a standing property | dead security code that compiles green is worse than absent code | MED
G-14 | ADD | Incident response runbook: token compromise, agent escape suspicion, audit-verify-failed procedure | none exists | MED
G-15 | MISSING | Compliance posture: audit retention policy, SIEM export, SBOM, dependency vuln process | SOC-2-adjacent reviewers ask these first; absent | MED
G-16 | TUNE | Spawn throttling per credential + disk-full fail-closed (spawn currently only WARNS >90% disk, service.go:306-316) | one leaked token can exhaust the host | MED
G-17 | DOC-UPGRADE | SECURITY.md rewrite (see section 4) | 30 lines, half of which is vulnerability-reporting boilerplate | BLOCKER (doc)
G-18 | ADD | Ship-failure alerting: audit.ship_to degradation is warn-only (server.go:68-74); a security deployment needs the loss of off-host shipping to be loud | silent loss of the only tamper-resistant copy | MED

4. DOC UPGRADES

SECURITY.md — NOT ADEQUATE. It is a vulnerability-reporting policy with a 5-bullet marketing appendix. It must become the security model: (a) explicit trust boundaries (host root daemon vs Linux user vs rootless dockerd vs container) with what contains what; (b) replace the "out of scope: escape" disclaimer with a residual-risk statement (kernel dependency, manual host-provisioning dependency, release-tag lag); (c) default posture disclosure (plaintext unless TLS configured); (d) credential inventory: every secret, its path, mode, lifetime, and who can read it; (e) failed-auth and audit-retention statement.
NEW docs/threat-model.md — owed. Assets (master token, agent keys, tenant workloads, audit trail), attacker classes (malicious agent workload, hostile tenant, token thief, network observer, host-root), per-class mitigations and known gaps.
NEW docs/operations-security.md — hardening checklist (TLS on, config 0600, host-provision verification, seal_key set, ship_to configured), token rotation procedure, and an incident runbook (token compromise; audit verify fails; suspected escape).
README.md — Quick Start should not demonstrate the insecure default (http:// + token) without a loud callout; the "do not run production workloads" warning on the demo (README.md:171) should extend to "not production-ready until TLS is configured."

5. TOP-10 RANKED FIXES

1 | Refuse non-loopback plaintext binds (TLS-required gate in server.Run) | BLOCKER | S | stops the cleartext-token default cold
2 | Rewrite SECURITY.md as a real security model + new THREAT-MODEL.md | BLOCKER | M | no team signs off without a stated model and honest residual risk
3 | Per-operator credentials with rotation + revocation (deprecate shared static token) | HIGH | L | accountability and blast-radius control; SOC-2 first question
4 | Log failed auth + rate-limit auth attempts | HIGH | S | today's IR visibility on attacks is zero
5 | Stop returning SSH private key / API key over plaintext; require TLS for spawn or out-of-band key delivery | HIGH | M | private keys currently cross the wire in clear by default
6 | Pin + verify the rootless Docker installer (sha256, vendored in release; verify cache) | HIGH | M | unauthenticated remote script runs in every first spawn
7 | Server-side agent-id validation in SpawnAgent | HIGH | S | client-side regex is not a control
8 | Enforce 0600 config.yaml (warn/refuse on world-readable) + secret-file mode audit for env/port files | HIGH | S | documented install leaks the master token to local users
9 | Egress control story (per-agent nftables template or proxy) documented + optional enforcer | HIGH | L | unrestricted outbound from every tenant
10 | Fix Heartbeat ownership check + constant-time TokenAuth compare | MED | S | two cheap correctness fixes in the auth path

6. REPEAT-OFFENSE SECTION

(a) Credential/key paths leaking: SpawnAgent returns the SSH private key in-band (internal/agent/manager_spawn.go:718; internal/server/service.go:363-364); config token unchecked for file mode (internal/config/config.go — no Stat/permission guard); env file under 0755 dir (internal/agent/manager_spawn.go:358).
(b) Success-marker string checks treated as state: exec session-denial classified by exit-code-254 signature (internal/server/service.go:727-733); /tmp isolation reported as string levels private/host-shared/unknown (service.go:157-161) — a probe result, not an enforced runtime state.
(c) State ladders: spawn's useradd→keygen→chown→systemd-run ladder with rollback (internal/agent/manager_spawn.go:222-496) and the residue-inventory planes it leaks when the ladder breaks (service.go:128-142).
(d) Silent-skip paths: audit open failure = warn + run without audit (internal/server/server.go:76-77); invalid ship_to silently downgrades to local-only (server.go:68-74); TLS off = silent plaintext (server.go:238-246); failed auth produces no record (server.go:208-212).
(e) Fixes that weaken a requirement to fit a lane: TokenAuth kept a plain `!=` compare (internal/auth/interceptor.go:70) while the JWT path got constant-time; the MTLSAuth interceptor was shipped with a context extraction that cannot work (internal/auth/mtls.go:88-99) rather than wiring the TLS state through the handler properly.
(f) Green-but-did-nothing: MTLSAuth compiles, satisfies connect.Interceptor at compile time (mtls.go:140), and can never authenticate a request in production; the newest release tag v0.1.4 ships none of the isolation work the docs describe (README.md:273-284), so `go install @latest` is green but isolation-less.

7. ONE COMMAND THAT WOULD FALSIFY MY MAIN CLAIM

My main claim: with default config, Bunker serves master tokens and freshly generated SSH private keys in plaintext on a reachable interface.

  cd /home/kara/bunker && grep -n "ListenAndServe()" internal/server/server.go && grep -n "SshPrivateKey:" internal/agent/manager_spawn.go

If the first grep returns nothing (no plaintext listen path) or the daemon refuses to start without TLS on non-loopback, and the second shows the key delivered only over a TLS-gated path, my claim is falsified. As read: server.go:262 and 278 are the plaintext branches, and manager_spawn.go:718 puts the private key on the wire.

PANEL REVIEW COMPLETE

session_id: 20260920_061714_201d30
