# Compliance and Data-Handling Posture

**Status:** v1 (2026-09-20). This is a **statement of the shipped defaults**, not a
certification. It exists because "no data-residency/retention/deletion statement" was a
panel finding (SEC-18). It is written to be verifiable, and honest where no guarantee exists.

> **No certification is claimed.** Bunker has no SOC 2, ISO 27001, or similar attestation.
> This document describes what the software does with data; it does not assert compliance
> with any regime. Teams with a regulatory obligation should treat the gaps in §5 as their
> own work or as a reason to wait for the roadmap.

---

## 1. What data Bunker holds

| Data | Location | Written by | Sensitivity |
|---|---|---|---|
| Agent home directories | `/home/bunker-<id>` | The agent's workload | Source, artifacts, any credential the workload brought |
| Container images and layers | Per-agent rootless Docker store under the agent home | `bunker spawn` / image build | Workload-dependent |
| Audit trail | `/var/lib/bunkerd/audit` (host, root) | The daemon | Who did what, when — control-plane metadata |
| Daemon config | `/etc/bunkerd/config.yaml` (root, 0644 default) | Operator | Contains `auth.token` / `auth.jwt_secret` **inline** (see §4) |
| SSH keys | `/etc/bunkerd/ssh` + agent home | `bunker spawn` | Per-agent host access |

**The control plane does not store workload *content* by default** — it stores the homes the
workload creates and the metadata to manage them.

---

## 2. Retention

**There is no built-in retention or aging policy.** Data persists until an operator removes
it. Specifically:

- **Agent homes:** removed by `bunker destroy`, subject to `agent.destroy_home_policy`.
- **Audit trail:** rotated by size/count; rotated segments are retained locally. Off-box
  shipping happens **only** if `audit.ship_to` is set (it is empty by default). There is no
  automatic deletion of old audit segments.
- **Images:** retained in the per-agent store until the agent or its store is removed.

Any retention guarantee must therefore be implemented by the operator (a host-side cron,
backup policy, or storage lifecycle). **No retention period is promised by Bunker.**

---

## 3. Deletion semantics — `destroy_home_policy`

This is the one place the software makes a data-deletion choice, and it is important to get
right. Set in `/etc/bunkerd/config.yaml`:

```yaml
agent:
  destroy_home_policy: archive   # DEFAULT — the opposite of what most people assume
```

| Value | What `bunker destroy` does to `/home/bunker-<id>` | When to use |
|---|---|---|
| `archive` *(default)* | **Moves the home to an archive location; data is retained.** | Forensics, recovery, regulated audit |
| `purge` | **Deletes the home.** | When deletion is the required outcome |

**The default is `archive`, not `purge`.** A team that assumes `destroy` deletes data is
wrong by default: the home survives. If your obligation is *deletion*, you must set `purge`
explicitly — and understand that purge is irreversible.

*Roadmap note:* REQ-D4 makes this default explicit and documented (this document is part of
that). Whether `archive` should remain the default is an owner decision (PRD §8).

---

## 4. Confidentiality of the control plane

- **Secrets can be kept out of the config file.** `auth.token` and
  `auth.jwt_secret` accept three sources, resolved in this precedence order
  (highest last): inline in `/etc/bunkerd/config.yaml` (legacy — the daemon
  warns at startup), `auth.token_file` / `auth.jwt_secret_file` naming a file,
  and the `BUNKER_AUTH_TOKEN_FILE` / `BUNKER_AUTH_JWT_SECRET_FILE` env vars.
  Generated secrets are persisted under `$BUNKER_SECRETS_DIR` (default
  `~/.config/bunkerd/secrets`) with directory mode `0700` and file mode `0600`
  (SEC-14 / REQ-I5). A set-but-unreadable path is a hard startup error rather
  than a silent fallback; an inline credential still works and is warned
  about. Until every credential is moved, protect the file with host
  permissions: `chmod 600 /etc/bunkerd/config.yaml`.
- **Transport may be plaintext.** TLS is off by default (SEC-02/SEC-03). Until REQ-T1/T2
  land, any control-plane data on a non-loopback link is observable.
- **Audit trail confidentiality** is whatever your host permissions provide; it may contain
  agent ids, caller metadata, and paths.

---

## 5. Known gaps (state them; do not imply guarantees that do not exist)

| Gap | Effect | Tracked as |
|---|---|---|
| No retention/aging engine | No retention period can be promised | REQ-D4 |
| `archive` default | "destroy" does not delete by default | REQ-D4 / owner decision |
| Audit not anchored off-box by default | Tamper-evidence is conditional | REQ-A2 |
| Secrets inline in config | Config copies/backups carry credentials when the operator keeps them inline | REQ-I5 (0600-file indirection shipped; the shipped example still inlines a placeholder token) |
| No per-operator identity | Access logs attribute to "the operator", not a person | REQ-I1 |
| Exec command content only in syslog | Forensic completeness for exec is partial | REQ-A3 |
| No egress policy | Data-exfiltration containment is not enforced by Bunker | REQ-E1 |

**Data residency:** Bunker makes no claim about where data resides beyond wherever the host
runs. There is no geofencing or region-aware storage. Multi-region or residency-bound
deployments are the operator's responsibility.

**Sub-processors / third parties:** the rootless installer is fetched from the network at
provisioning time today; pinning is not enforced (SEC-12 / REQ-S1). Once pinned, this line
should name the source and version.

---

## 6. What a compliance reviewer should take away

1. The software is **transparent** about its data handling (this document).
2. It does **not** currently enforce retention, residency, or deletion policy on its own —
   the operator must, and one default (`archive`) actively retains data.
3. Control-plane confidentiality depends on host permissions and a TLS decision the operator
   must make, because the shipping default is plaintext and inline secrets.

Closing §5's gaps is what would let a team hand this document to an auditor instead of a
runbook. Until then, it is an honest inventory, not an attestation.
