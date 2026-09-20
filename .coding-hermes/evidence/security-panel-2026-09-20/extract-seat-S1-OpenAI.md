  ⟳ compacting context…
  ⟳ compacting context…
Review basis: repository HEAD c50f7b3c53aa7254a195bfc1070a96aedd9c9426 (`c50f7b3 dogfood: bunker DOES-NOT-DELIVER — bunker mount cannot mount any agent; shared scratch exchange unreachable`). Focused tests passed: `go test ./internal/auth ./internal/audit ./internal/server ./internal/agent -count=1` → all four packages OK. The worktree already contained unrelated modified/untracked files; this review made no repository changes.

1. SIGN-OFF VERDICT

I would not approve Bunker for a team today. Several components are well-built—fail-closed startup authentication, per-agent SSH keys, private `/tmp`, durable lifecycle state, and a useful audit chain—but the central tenant-isolation claim is not defensible. Most importantly, subordinate UID/GID ranges are allocated from each adjacent account’s ordinary UID/GID, creating overlapping 65,536-ID mappings. That compromises the identity separation on which rootless containers depend. The control plane also defaults to plaintext on wildcard listeners, has an agent-to-agent heartbeat authorization gap, and lacks the threat model, egress controls, supply-chain policy, secret-broker story, and incident-response posture a security team would require.

DO-NOT-APPROVE — the user-namespace identity boundary is configured with overlapping subordinate-ID ranges.

2. CLAIM VERDICTS

C-1 | CONTRADICTED | The five `SECURITY.md` bullets do not accurately describe the product. (1) mTLS is claimed at `SECURITY.md:23-24`, but TLS and mTLS are optional and disabled by default at `internal/config/config.go:365-380`; the server wires certificate verification but not `VerifyCN` identity authorization at `internal/server/server.go:349-379`, and the CLI client only exposes certificate-verification bypass, not client-certificate configuration (`internal/cli/client.go:14-26`). (2) JWT scope enforcement is claimed at `SECURITY.md:25`, but `agentService.Heartbeat` accepts the request’s arbitrary `agent_id` without comparing it to authenticated claims (`internal/server/service.go:1421-1451`). (3) CPU, memory, and PID controls are real (`internal/agent/isolation.go:120-139`). (4) user namespaces are configured, but each range starts at the user’s adjacent UID/GID (`internal/agent/rootless.go:535-583`), causing overlap. (5) SSH keys are per agent rather than “per container”; server copies are persisted under a common root-owned directory (`internal/agent/manager_spawn.go:282-344`).

C-2 | CONTRADICTED | Random token generation and JWT algorithm handling are mostly sound: JWT secrets use `crypto/rand`, require at least 32 bytes, carry expiry/not-before, and use HS256 (`internal/auth/jwt.go:77-116`); static fallback comparison in the JWT path is constant-time (`internal/auth/jwt.go:146-167`); opaque keys use 32 random bytes and stored SHA-256 hashes (`internal/apikey/manager.go:40-71`). Master-scoped service separation also exists (`internal/server/server.go:172-179`). However, scope enforcement is not correct: the Agent Heartbeat handler takes `req.Msg.AgentId` directly and renews it without checking `claims.AgentID` (`internal/server/service.go:1421-1451`). The configured mTLS CN restriction is not connected to server construction: `VerifyCN` is configured at `internal/config/config.go:71-82,496-498`, while `buildTLSConfig` only installs CA-based client-certificate verification (`internal/server/server.go:349-379`); the CN-checking interceptor exists separately at `internal/auth/mtls.go:15-83,133-136` but is not mounted.

C-3 | CONTRADICTED | Authentication fails closed when enabled without a credential (`internal/config/config.go:651-663`), but transport does not. The defaults are wildcard-style `:9090` and `:8080` with TLS disabled (`internal/config/config.go:365-385`). With a token configured, the server calls ordinary `ListenAndServe` without warning or refusal (`internal/server/server.go:238-278`). The operator documentation explicitly says “Plaintext is the default” (`docs/integration.md:56-58`). Thus bearer credentials and crown-jewel RPC traffic can traverse reachable plaintext listeners.

C-4 | CONTRADICTED | Bunkerd RPCs are master-only (`internal/server/server.go:172-179,220-236`), and Agent Metrics derives or checks the scoped identity (`internal/server/service.go:1348-1368`). But Agent Heartbeat does not: agent A can submit agent B’s ID and extend B’s lifetime (`internal/server/service.go:1421-1451`). This is a cross-agent authorization failure even if its immediate effect is renewal rather than command execution.

C-5 | CONTRADICTED | Spawn and Destroy sit on the master-only Bunkerd service (`internal/server/server.go:172-179`; `internal/server/service.go:304-305,373-375`). Renewal is also exposed through the permissive Agent service and is not bound to the caller’s agent ID (`internal/server/service.go:1421-1451`). There is no Register RPC in the current proto—the Agent service exposes GetInfo, Metrics, and Heartbeat (`proto/bunker/v1/bunker.proto:62-69`). Therefore renewal is weaker than spawn/destroy, and the register portion of the claim does not describe the current API.

C-6 | CONTRADICTED | The wording is not defensible for a security-team audience. `SECURITY.md:30` places “compromise of the host system by a user with root in their own container” out of scope while simultaneously selling rootless container isolation as a security boundary. It should instead state: container root is expected; all tenants share the host kernel; rootless user namespaces reduce consequences but do not remove kernel/rootlesskit/container-runtime escape risk; host-kernel escape is a residual product risk requiring supported-kernel baselines, patch SLAs, runtime CVE monitoring, and a stronger isolation tier such as VMs for hostile tenants. A host compromise cannot be excluded from the threat model when preventing it is the product’s core promise.

C-7 | CONTRADICTED | Each agent does receive a separate Linux account, SSH material, rootless dockerd unit, socket, and private `/tmp` (`internal/agent/manager_spawn.go:221-236,282-350`; `internal/agent/isolation.go:67-143`). Private-key files and `authorized_keys` use mode 0600, with `.ssh` and the server key directory at 0700 (`internal/agent/manager_spawn.go:282-344`). But the user-namespace mapping is unsafe: `/etc/subuid` and `/etc/subgid` entries start at each account’s own UID/GID and span 65,536 IDs (`internal/agent/rootless.go:535-583`). For adjacent users, the read-only arithmetic probe produced `1001-66536`, `1002-66537`, `overlap=65535`. “Unique namespace derived from system identity” at `rootless.go:535-538` is therefore false. The update is also a read-then-append operation with no cross-process lock (`rootless.go:562-583`).

C-8 | ANSWER | An agent is a full unprivileged host user, not merely a process trapped inside its container. Detached workloads execute as that host UID using `systemd-run` (`internal/agent/run.go:15-64,85-118`), so the user retains ordinary access to host networking, world-readable files, `/proc` visibility permitted by host policy, user services, and its Docker socket. No systemd hardening properties such as `ProtectSystem`, `ProtectHome`, `NoNewPrivileges`, `PrivateNetwork`, `IPAddressDeny`, or `SystemCallFilter` are applied; the dockerd unit’s security properties stop at `PrivateTmp` and resource settings (`internal/agent/isolation.go:109-143`). The sanctioned shared scratch is deliberately group-readable across agents and enabled by default (`internal/config/config.go:432-441`; `internal/agent/isolation.go:19-47,198-218`). User homes are created by plain `useradd -m` without an explicit home mode (`internal/agent/manager_spawn.go:221-236`), leaving cross-home confidentiality dependent on host `useradd`/umask defaults. Open egress remains an acknowledged pending feature (`.coding-hermes/board/tasks.jsonl:135`, GAP-076).

C-9 | SPLIT | Direct shell-injection exposure is substantially controlled: agent IDs must match `^[a-z0-9-]{1,63}$` before side effects (`internal/agent/manager.go:24-25`; `internal/agent/manager_spawn.go:39-45`), and privileged tools such as `useradd`, `chown`, and `systemd-run` receive separate argv elements rather than concatenated shell input (`internal/agent/manager_spawn.go:221-223,293-315,346-364`). However, two state/race classes remain. First, `useradd` failure text containing “already exists” causes Bunker to reuse the account without verifying the account’s UID, home, ownership, origin, or existing processes (`manager_spawn.go:221-233`). Second, subordinate-ID files are read and then appended without a lock, uniqueness allocation, overlap check, or atomic rewrite (`internal/agent/rootless.go:562-583`). The shell-injection claim is favorable; the TOCTOU/state-adoption claim is not.

C-10 | SPLIT | Release installs download binaries plus `SHA256SUMS` and refuse a mismatch (`scripts/install.sh:264-318,365-393`). That detects transfer corruption but not a compromised release account/channel because checksums and binaries come from the same unsigned release. Local `--from-dir` installation explicitly proceeds without verification when `SHA256SUMS` is absent (`scripts/install.sh:396-438`). More seriously, the rootless-Docker installer is downloaded from a mutable URL and executed as the agent user; “validation” only checks regular-file status, minimum size, and a shebang (`internal/agent/rootless.go:357-384,440-462,588-604`). There is no pinned digest, signature, provenance verification, SBOM gate, or trusted-version manifest. Allowed base images are mutable tags, not digests (`internal/imagespec/parse.go:12-24,178-209`).

C-11 | SPLIT | The writer creates a root-side 0600 log in a 0700 directory and hash-chains every record and rotation (`internal/audit/audit.go:82-145,305-352,377-435`). `Verify` detects malformed records, hash changes, and broken links across retained files (`internal/audit/verify.go:20-86,127-184`). Root inside a rootless agent container should not be able to write `/var/log/bunkerd/audit.log`; it can mainly generate activity, flood retention, and exploit visibility gaps. The chain is nevertheless only locally tamper-evident, not immutable: an attacker controlling the host can replace the whole retained chain and recompute unkeyed SHA-256 records. HMAC seals and off-host shipping are optional (`audit.go:74-106,261-277`), and shipping can drop the oldest queued segment or discard a segment after five attempts (`internal/audit/ship.go:35-44,263-294,340-360`). Plain HTTP and UDP syslog are accepted (`ship.go:68-106`). Also, only authenticated RPCs reach the audit interceptor; failed authentication is intentionally absent (`internal/server/server.go:208-218`).

C-12 | SPLIT | Query, export, local verification, filters, chain-head status, backup sizes, and shipping state are useful (`docs/audit.md:9-20,44-53,55-122,138-157`). But incident-response readiness is incomplete: `status` always reports `last verify: n/a` (`docs/audit.md:81,103-104`); missing logs report `enabled: false` and exit successfully (`docs/audit.md:57-61`); Query silently skips malformed lines and requires a separate Verify pass (`internal/audit/query.go:43-49,86-101`); auth failures are not audited (`internal/server/server.go:208-212`); local retention is only 5 MiB × three backups (`internal/audit/audit.go:16-26`); and remote shipping is lossy/best-effort. The separate snoopy documentation overclaims a “complete execution record” (`docs/exec-audit.md:13-45`) even though `LD_PRELOAD` logging is bypassable by static binaries and is subject to journald rate limiting and rotation (`docs/exec-audit.md:173-182`).

C-13 | SPLIT | Positive controls exist: the CLI token registry is stored in a 0700 directory and 0600 YAML file (`internal/cli/config.go:80-101`); agent private keys are persisted server-side in a 0700 directory with mode 0600 (`internal/agent/manager_spawn.go:336-344`); `.ssh` and `authorized_keys` use 0700/0600 (`manager_spawn.go:282-315`); opaque API keys store only SHA-256 hashes in memory (`internal/apikey/manager.go:40-71`). Weaknesses remain: the CLI stores long-lived bearer tokens in plaintext YAML (`internal/cli/config.go:27-34,154-168`); static tokens, JWT secrets, seal keys, tunnel credentials, and Tailscale auth keys are plain config/env values with no vault/keyring integration or permission validation (`internal/config/config.go:71-121,338-362,472-561`). Spawn returns raw SSH private key and API key material (`proto/bunker/v1/bunker.proto:191-204`). The CLI silently ignores key-directory/key-file write errors while claiming the key was saved, and prints the raw API key to stdout (`internal/cli/spawn.go:218-242`). No documented rotation, revocation drill, escrow, break-glass, or secret-injection policy exists.

C-14 | CONTRADICTED | The most likely real incident is prompt-injected or compromised agent code exfiltrating repository credentials, API tokens, package-manager credentials, or copied production data from its host-user home/environment over unrestricted outbound network access. The agent executes arbitrary commands as a normal host UID (`internal/agent/run.go:15-64`), the unit lacks host/filesystem/network hardening beyond private `/tmp` and resource limits (`internal/agent/isolation.go:109-143`), and egress policy is still pending because “agents currently reach the internet freely” (`.coding-hermes/board/tasks.jsonl:135`, GAP-076). The missing controls are brokered short-lived secrets, outbound default-deny/allowlisting, per-agent network policy, destination logging, and a policy forbidding durable credentials in agent homes.

3. THE SECURITY-TEAM GAP LIST

SG-01 | MISSING | Formal threat model with assets, trust boundaries, attacker classes, abuse cases, and explicit residual risk | The current policy is five bullets plus one broad exclusion; a security reviewer cannot determine what is actually guaranteed | BLOCKER

SG-02 | ADD | Non-overlapping subordinate UID/GID allocator, lock/atomic update, overlap migration, and startup overlap refusal | Overlapping host IDs invalidate the core user-namespace separation claim | BLOCKER

SG-03 | ADD | Per-RPC authorization matrix enforced centrally; agent-scoped handlers must derive the target from claims or compare it explicitly | The Heartbeat IDOR shows authorization currently depends on each handler remembering the check | BLOCKER

SG-04 | TUNE | TLS required by default; refuse plaintext on non-loopback; make client/server mTLS usable; wire certificate identity/SAN policy; remove or loudly gate `tls_insecure` | Bearer tokens and privileged RPCs cannot be approved over default plaintext listeners | BLOCKER

SG-05 | ADD | Per-agent egress modes: none, audited allowlist, and open; block direct access to the host control plane and metadata/internal networks | Prompt injection plus unrestricted egress is the most probable data-loss path | BLOCKER

SG-06 | ADD | Host-user containment baseline: explicit 0700 homes, `ProtectSystem`, `ProtectHome`, `NoNewPrivileges`, syscall/address-family/device restrictions where compatible, `/proc` policy, and documented incompatibility tests | The agent is a host account with a shell; rootless Docker alone does not constrain host-user activity | HIGH

SG-07 | ADD | Secret-management architecture: keyring/Vault/KMS-backed control-plane credentials, short-lived workload identity, scoped secret injection, redaction, rotation/revocation procedures, and no secrets persisted in agent homes | A regulated reviewer first asks where secrets live, who can retrieve them, and how compromise is contained | BLOCKER

SG-08 | ADD | Signed releases, provenance attestations, SBOMs, verified rootless-installer digest/version, digest-pinned base images, registry allowlist, vulnerability policy, and package pinning | Same-origin SHA files and mutable install/image sources do not establish artifact authenticity | HIGH

SG-09 | ADD | Audit denied-auth attempts, authorization denials, secret/key lifecycle events, policy changes, and control-plane configuration changes | Security monitoring needs attempted abuse, not only authenticated operations | HIGH

SG-10 | TUNE | Durable authenticated audit export with mandatory TLS, persistent disk-backed retries, delivery acknowledgement, retention policy, clock-sync monitoring, alerts, and an immutable external anchor | Current shipping can drop evidence after seconds and accepts plaintext transports | HIGH

SG-11 | DOC-UPGRADE | Operator incident runbook: containment, token/key revocation, agent freeze, evidence acquisition, chain verification, host isolation, rebuild, notification, and recovery validation | An on-call engineer needs executable steps before an incident, not scattered CLI descriptions | HIGH

SG-12 | DOC-UPGRADE | Security deployment baseline and preflight command that produces an attestation of effective TLS, auth, mTLS, UID-map uniqueness, home modes, egress policy, audit shipping, kernel/runtime versions, and host hardening | Security teams approve effective deployed state, not configuration intent | HIGH

SG-13 | ADD | Image/workload policy: approved registries, digest-only images, admission scanning, package provenance, rootless-runtime version policy, and emergency revocation | Agents currently consume mutable images and a mutable installer | HIGH

SG-14 | DOC-UPGRADE | Compliance/control map for SOC-2-adjacent deployments: access reviews, change control, log retention, vulnerability management, incident evidence, asset ownership, backups, vendor dependencies, and exceptions | There is no material from which a reviewer can map Bunker to logical-access, monitoring, and change-management controls | MED

SG-15 | ADD | Security regression suite for cross-agent authorization, subordinate-ID overlap, cross-home reads, control-plane reachability, egress, credential leakage, and log-loss behavior | The focused package tests are green despite the Heartbeat IDOR, dead `verify_cn`, and overlapping ranges | HIGH

SG-16 | TUNE | Shared scratch off by default for hostile tenants, explicit sender/receiver ACLs, provenance labels, malware scanning, and audit events | Group-readable cross-agent exchange is an intentional data boundary and must not be mistaken for isolation | MED

4. DOC UPGRADES

SECURITY.md — Rewrite completely. It is not an adequate security model. It currently claims universal mTLS and JWT scope enforcement despite optional plaintext/static-token operation and a scope defect (`SECURITY.md:21-30`). It must define assets, trust zones, attacker capabilities, guarantees, non-guarantees, supported deployment profiles, shared-kernel risk, agent host-user access, network behavior, secret ownership, audit limitations, security update policy, and precise language for container escape. Keep vulnerability reporting, but separate it from the product security model.

docs/threat-model.md — New, mandatory. Include data-flow and trust-boundary diagrams; host root, operator, agent host user, container root, malicious image, compromised dependency, network attacker, and control-plane credential theft; STRIDE-style abuse cases; control mapping; residual risks; and tier-specific acceptance criteria.

docs/authentication-and-authorization.md — New. Document static token, JWT, opaque sub-key, mTLS, claims, exact RPC permission matrix, agent-to-resource ownership rules, issuance/rotation/revocation, certificate identity rules, and break-glass access. The matrix should become test-generated or test-checked.

docs/secure-deployment.md — New. Provide a fail-closed production baseline: TLS/mTLS, loopback/private bind rules, firewall, control-plane isolation, unique sub-ID verification, 0700 homes, required kernel/runtime versions, host-provision verification, egress mode, audit collector, time sync, backups, and a machine-readable preflight report.

docs/secrets.md — New. Inventory the master token, JWT secret, seal key, tunnel credentials, Tailscale key, agent API keys, host/agent SSH keys, and workload secrets. State storage location, owner, mode, lifetime, exposure, rotation, revocation, backup, and incident procedure for each. Explicitly prohibit durable secrets in agent homes unless accepted by policy.

docs/network-security.md — New. Describe inbound port allocation versus actual access enforcement, host/control-plane reachability, DNS, egress modes, private-network/metadata-service denial, logging, tunnels, Tailscale/Cloudflare trust, and container-versus-host-user differences.

docs/supply-chain.md — New. Define release signing, checksums versus signatures, provenance, SBOMs, rootless-Docker installer pinning, image digest policy, registry allowlist, scanner cadence, CVE SLA, dependency updates, and emergency revocation.

docs/audit.md — Rewrite its guarantees. Add: auth failures excluded; a local SHA chain is rewritable by host root without an external anchor; sealing/shipping are optional; queues can drop; HTTP/syslog can be plaintext/lossy; malformed query lines are skipped; `last verify` is not recorded; exact retention math; and mandatory regulated-mode settings.

docs/exec-audit.md — Rewrite “complete execution record.” Document LD_PRELOAD limitations, static/setuid bypasses, journald drops and retention, sensitive command-line exposure, privacy controls, and why this is supplementary telemetry rather than a security boundary.

docs/incident-response.md — New runbook with named commands and decision points for stolen master token, suspected agent escape, malicious image, cross-agent access, audit-chain failure, missing shipment, destructive lifecycle error, and host compromise.

docs/compliance.md — New, explicitly non-certifying. Map implementation/evidence to SOC-2-adjacent logical access, change management, monitoring, incident response, vulnerability management, retention, and business-continuity controls; name gaps and operator responsibilities.

docs/integration.md — Change “Plaintext is the default” (`docs/integration.md:56-58`) from a neutral fact into a development-only warning, use HTTPS in production examples, document CA/client-certificate handling, and state that bearer tokens must never be sent over HTTP.

5. TOP-10 RANKED FIXES

1 | Replace subordinate-ID allocation with globally non-overlapping ranges; lock updates, refuse overlap, and migrate existing hosts | BLOCKER | M | The current rootless identity mapping invalidates the core tenant-isolation promise.

2 | Fix Agent Heartbeat ownership and implement a centrally declared/tested per-RPC authorization matrix | BLOCKER | S | One scoped credential must never mutate any other tenant’s state.

3 | Require TLS for non-loopback listeners; ship complete client mTLS support and enforce certificate identity/SAN rules | BLOCKER | M | The control-plane credential and privileged RPC stream must not traverse plaintext.

4 | Add per-agent egress control and block agent access to the host control plane, metadata, and internal management networks | BLOCKER | L | This is the principal containment control for prompt-injected or compromised AI workloads.

5 | Publish a real threat model and rewrite the “complete isolation”/container-escape language | BLOCKER | M | Approval requires precise guarantees and residual risks, not exclusions that remove the central threat.

6 | Build a secret-broker path with short-lived workload identity, rotation, revocation, and keyring/Vault/KMS integration | BLOCKER | L | Persistent tokens in files, stdout, homes, or environments create predictable exfiltration incidents.

7 | Pin and authenticate the rootless installer; sign releases; publish provenance/SBOMs; require digest-pinned images | HIGH | M | Checksums from the same mutable channel do not establish artifact authenticity.

8 | Apply and verify a compatible host-user/systemd hardening baseline, including 0700 homes and `/proc`/filesystem/network restrictions | HIGH | L | An agent is an unprivileged host shell user, so Docker controls alone are incomplete.

9 | Upgrade audit/IR: denied-auth logging, immutable authenticated off-host delivery, persistent retries, alerts, verification history, and retention policy | HIGH | L | Security teams need reliable evidence during an incident, including attempted abuse and proof of delivery.

10 | Add a security deployment attestation plus cross-tenant adversarial regression suite | HIGH | M | Current green tests do not detect the key authorization, mTLS wiring, or sub-ID overlap failures.

6. REPEAT-OFFENSE SECTION

(a) Credential/key paths leaking

The API returns raw SSH private-key and agent API-key material (`proto/bunker/v1/bunker.proto:191-204`), while generated connection strings contain the server-side private-key path (`internal/agent/manager_spawn.go:577-592`). The CLI stores the private key but ignores both `MkdirAll` and `WriteFile` errors, then prints the path and raw API key (`internal/cli/spawn.go:218-242`). This preserves the historical class: key material and key locations cross too many surfaces, and “saved” is reported without proving persistence.

(b) Success-marker string checks treated as state

`useradd` failure output containing the string “already exists” is treated as proof that the account is a reusable prior Bunker agent (`internal/agent/manager_spawn.go:221-233`). There is no authoritative follow-up checking UID/GID, expected home, owner marker, home ownership, active processes, or whether another daemon/operator owns that account.

(c) State ladders

Daemon ownership falls through a layered marker/state inference. If daemon-instance identity creation fails, the caller retains an empty identity, disabling ownership checks and treating markers as absent (`internal/agent/registry.go:187-196`). That is a classic state ladder: strong identity → marker inference → legacy absence behavior, with security semantics changing according to which artifact happened to load.

(d) Silent-skip paths

Audit Query silently discards malformed JSON records (`internal/audit/query.go:43-49,86-93`), and failed authentication never reaches the audit interceptor (`internal/server/server.go:208-218`). Both make “no matching record” ambiguous between “nothing happened,” “the attacker was unauthenticated,” and “the line was damaged.”

(e) Fixes that weaken a requirement to fit a lane

Isolation provisioning explicitly makes scratch and private-`/tmp` instance creation best-effort to avoid aborting an otherwise complete spawn; a failed tmp-instance precreation logs a warning and returns success (`internal/agent/isolation.go:192-225`). Likewise, rootless-installer “validation” was narrowed to shape checks—regular file, eight bytes, shebang—rather than authenticity (`internal/agent/rootless.go:357-384`). These are operationally convenient degradations, not security-team acceptance criteria.

(f) Green-but-did-nothing

The focused command `go test ./internal/auth ./internal/audit ./internal/server ./internal/agent -count=1` passed all packages, yet the live wiring still does not consume `TLS.VerifyCN`: the field is defined and bound (`internal/config/config.go:71-82,496-498`), the checking interceptor exists (`internal/auth/mtls.go:15-83,133-136`), but server construction only installs CA verification (`internal/server/server.go:349-379`). The same green suite does not catch the Heartbeat cross-agent mutation or overlapping subordinate-ID ranges. This is passing component coverage without proving the promised security property.

7. ONE COMMAND THAT WOULD FALSIFY YOUR MAIN CLAIM

Run with agent A’s scoped key against an existing agent B:

curl -si "$BUNKER_URL/bunker.v1.Agent/Heartbeat" -H "Authorization: Bearer $AGENT_A_KEY" -H 'Content-Type: application/json' --data '{"agent_id":"agent-b"}'

A 2xx response acknowledging `agent-b`, or a changed expiry for agent B, falsifies the claim that one agent’s credentials cannot affect another agent.

PANEL REVIEW COMPLETE

session_id: 20260920_061705_91211c
