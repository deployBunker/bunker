# Security Policy

## Supported versions

| Version | Supported |
| ------- | --------- |
| 1.x     | ✅        |

Older versions are not backported; upgrade to receive security fixes.

## Reporting a vulnerability

**Do not open a public issue.** Email **wojons@wojonstech.com**.

Include a description, reproduction steps, affected versions, and any mitigations you have
identified. We acknowledge within **48 hours**. The full process — embargo (default 90 days),
advisory publication, security-release channel, and our commitments to you — is in
[`docs/disclosure.md`](docs/disclosure.md).

## Security model

Bunker provisions **per-agent rootless Docker containers** with these controls. Each entry
states its **default** and its **preconditions**, because a control that is off by default,
or that only works after host provisioning, is not a control until you turn it on.

| Control | Default | Precondition | Notes |
|---|---|---|---|
| **TLS between CLI and daemon** | **OFF** (`tls.enabled: false`) | Operator sets `tls.enabled: true` | The daemon starts on a non-loopback bind with TLS off. **Run TLS in any non-loopback deployment.** |
| **Mutual TLS (client certs)** | OFF (`tls.mtls: false`) | `tls.enabled` + `tls.ca_file` | Not enabled by default. |
| **API authentication** | **ON** (`auth.enabled: true`), one static **master token** | `auth.token` (or `auth.jwt_secret`) set in the config | The daemon refuses to start if auth is enabled with no credential. There is **no per-operator identity or RBAC** — one shared credential today. |
| **Private `/tmp` per agent** | Enforced **only after host provisioning** | `bunker hostprov` (host isolation) run | On an unprovisioned host, SSH sessions share the host `/tmp`. |
| **User-namespace remapping** | ON | — | Subordinate-ID ranges are allocated **globally disjoint per agent** (GAP-140), under a host-wide lock, and `bunkerd` refuses to start if an overlap is present; remediate with `bunker subid-migrate`. |
| **cgroup resource limits (CPU, memory, PIDs)** | ON | — | Bounds a runaway agent's resource use; not a security boundary on its own. |
| **SSH key isolation per agent** | ON | — | Keys are per-agent; the spawn response can also return the agent's key over the wire — see residual risk. |
| **Shared scratch exchange** (`/srv/bunker-share`) | **ON** (`shared_scratch_enabled: true`), group `bunker-agents`, mode `2770` | — | Cross-agent read/write is enabled by default. Turn it off if agents must not exchange data. |
| **Audit trail** | ON, hash-chained, **not anchored off-box** | `audit.seal_key` + `audit.ship_to` for tamper-evidence | Without an off-box anchor, a host-root attacker can rewrite the log undetectably. |
| **Egress control** | **NONE** | — | No default-deny or allowlist exists yet. Containment against exfiltration is not enforced by Bunker today. |

## Residual risk (please read)

These are true even with every control above enabled, and are disclosed rather than hidden:

- **A malicious container workload can attack the host kernel.** Containers share the kernel;
  the rootless/user-namespace design is defence-in-depth, not a hypervisor boundary.
- **An agent with legitimate `exec` access can run what the daemon would run.** Audit records
  it; it does not prevent it.
- **Isolation is per-instance, not per-lifetime.** PID namespaces and `/tmp` state are not
  preserved across a restart. PID-namespace isolation is intentionally omitted on the current
  rootlesskit version (see `internal/agent/isolation.go`).
- **The audit trail's tamper-evidence is conditional.** It is meaningful only with
  `audit.seal_key` and an off-box `audit.ship_to` configured; without both, treat it as
  corruptible by anyone with host root.
- **`destroy` retains data by default.** `agent.destroy_home_policy` defaults to `archive`,
  not `purge` — a destroyed agent's home is archived, not deleted. See `docs/compliance.md`.
- **The control plane credential is not per-operator and not revocable at runtime.** Rotation
  today means updating the shared token and restarting the daemon. See the
  [`docs/incident-runbook.md`](docs/incident-runbook.md).

## Out of scope

- Social engineering and physical access to the host.
- **A malicious host root.** Any process with host root can read the daemon config, rewrite
  the audit log, and spawn agents. No sandboxing product defends this, and Bunker does not
  claim to. The goal is safe operation *below* that line, and honesty about it.

## Documentation

- [`docs/threat-model.md`](docs/threat-model.md) — assets, adversaries, boundaries, residual risk.
- [`docs/incident-runbook.md`](docs/incident-runbook.md) — freeze, evidence capture, rotation.
- [`docs/compliance.md`](docs/compliance.md) — data handling, retention, deletion defaults.
- [`docs/disclosure.md`](docs/disclosure.md) — the reporting process.
- [`docs/prd/security-readiness.md`](docs/prd/security-readiness.md) — the gap between the
  above and what a team should require, prioritised.

> **Status of this document.** It describes the **shipped** behaviour honestly, including its
> gaps. Where a protection is planned but not built, it is listed as a gap with a tracked row,
> not claimed here. This file is the record of what Bunker does *today*.
