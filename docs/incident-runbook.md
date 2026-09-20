# Incident Runbook

**Status:** v1 (2026-09-20). Covers the top operational security scenarios. Every command
below exists in the shipped CLI (`bunker audit status|verify|list|export`, `bunker list`,
`bunker destroy`, `bunker registry`, `bunker config`); paths are the documented defaults
(`/etc/bunkerd/config.yaml`, `/var/lib/bunkerd`, `/etc/systemd/system/bunkerd.service`).

> **Read this first.** Until the P1/P2 controls land (`docs/prd/security-readiness.md` §6),
> two facts shape every response: the transport may be **plaintext**, so treat any response
> action as observable on the wire; and the master token is **not revocable per-operator**,
> so "rotate the credential" means "rotate the one shared credential and restart."

---

## 1. Severity and first moves

| Severity | Trigger | First move |
|---|---|---|
| **SEV-1** | Suspected control-plane compromise, host root, or mass exfiltration | §2 Freeze, then §3 |
| **SEV-2** | Suspicious agent behaviour, cross-tenant access attempt, audit anomaly | §4, then §3 |
| **SEV-3** | Single agent misbehaving; leaked agent key; policy violation | §5 |

**Universal first step — preserve evidence before you change state.** Audit records are the
only forensic record and are written to the host; do not restart or rebuild until §3's export
has run.

```bash
# Snapshot the audit state and export the trail BEFORE any remediation.
bunker audit status                       # chain head, sizes, shipping/anchor state
bunker audit verify                       # confirm the chain is intact right now
bunker audit export --since <start> --until <now> > /root/ir-$(date +%s).jsonl
```

---

## 2. Freeze (SEV-1)

Stop the bleeding with the least destructive action that works.

1. **Stop the control plane** (prevents new spawns/execs; running containers keep running):
   ```bash
   sudo systemctl stop bunkerd
   ```
2. **Stop agent containers** (no daemon needed):
   ```bash
   bunker list                     # capture the inventory first
   # per agent, or via your host tooling:
   sudo -u bunker-<id> XDG_RUNTIME_DIR=/run/user/$(id -u bunker-<id>) \
       docker stop $(docker ps -q)
   ```
3. **Isolate the host** at the network layer (do not rely on Bunker for this — there is no
   egress policy today): remove it from the network, or firewall it.
4. **Do not wipe anything.** Homes, containers and the audit log are evidence.

---

## 3. Evidence capture

Run before remediation; the audit chain is the incident timeline.

```bash
bunker audit status                                   # note the chain head hash
bunker audit export > /root/ir-full-$(date +%s).jsonl # the whole trail
tar czf /root/ir-homes-$(date +%s).tar.gz /home/bunker-* 2>/dev/null
sudo cp /etc/bunkerd/config.yaml /root/ir-config-$(date +%s).yaml
# Preserve the runtime: container list + images per agent
for u in $(ls -d /home/bunker-* 2>/dev/null); do echo "== $u"; sudo -u ${u#/home/} docker ps -a; done
```

**Honesty note (must be stated in any report):** `bunker audit verify` proves the chain is
internally consistent. Unless `audit.seal_key` and `audit.ship_to` are configured, it does
**not** prove the log was not rewritten by someone with host root. Check `bunker audit
status` for the anchor state and say plainly in the write-up whether an off-box anchor
existed at the time.

---

## 4. Triage: was it an agent, a credential, or the host?

`bunker audit list` and the exported JSONL answer "who did what", with one honest limit:
**failed authentication is not currently recorded** (the audit interceptor sits inside auth),
so a burst of denied RPCs will not appear in the chain today. Correlate with the daemon
journal:

```bash
bunker audit list --since <start> --limit 500
journalctl -u bunkerd --since "<start>" | grep -iE "auth|denied|unauthenticated|spawn|exec|destroy"
```

- **A specific agent** acting outside its lane → §5.
- **The master token used from an unexpected source** → §6 rotation; assume the control plane
  is compromised (SEV-1).
- **Evidence of host-root activity** → outside Bunker's model; escalate to host/OS incident
  response and record that Bunker does not claim to detect it.

---

## 5. Contain a single agent

```bash
bunker audit list --agent <id> --limit 200    # establish the agent's action history
bunker stop <id>                              # stop its container(s)
bunker destroy <id>                           # deprovision
```

**Note the destroy-home policy before you run destroy.** The default is `archive`, not
`purge` (`agent.destroy_home_policy` in `/etc/bunkerd/config.yaml`). Archive preserves the
home for forensics — the right choice during an incident. Only pass a purge policy once
evidence has been exported.

**The agent's own key cannot be revoked per-agent today** (no `Revoke` RPC). It dies with the
agent's user; until REQ-I4 lands, containment means `stop` + `destroy`, not key revocation.

---

## 6. Credential rotation (the compromised-token case)

Today this is a **shared-secret rotation with a restart**, because there is exactly one
master credential and no revoke RPC.

1. Generate a new token.
2. Update `auth.token` in `/etc/bunkerd/config.yaml` (root-only).
3. **Enable TLS before restarting if at all possible** — restarting onto a plaintext link
   re-exposes the new token exactly as the old one was exposed:
   ```yaml
   tls:
     enabled: true
     self_signed: true     # or cert_file/key_file for a real cert
   ```
4. Restart:
   ```bash
   sudo systemctl restart bunkerd
   ```
5. Re-issue agent keys as needed; distribute the new token to operators out of band.
6. **Record the rotation in the audit trail** (a manual note until the lifecycle RPCs land)
   and confirm the old token no longer works.

If keys are managed elsewhere (a secret store or a wrapper), rotate there and update the
`auth.token` indirection in the same window.

---

## 7. Notification

- **Report a Bunker vulnerability privately** — see `SECURITY.md`. Do not open a public issue.
- **Customer/user notification** is a policy decision: until `docs/prd/security-readiness.md`
  REQ-D5 (disclosure process) and REQ-D4 (compliance posture) land, there is no published
  embargo window or advisory channel. Decide and document per incident.
- **Preserve the timeline** from §3; it is the input to the post-incident review.

---

## 8. Post-incident review

Within one week, produce a written review answering:

1. What boundary was crossed (BT1 transport / BT2 authorization / BT3 isolation / BT4 egress)?
2. Was it a **missing control** (→ backlog row), a **mis-default** (→ change the default), or
   a **known residual risk** (§7 of the threat model)?
3. Was the audit trail sufficient to reconstruct it? If failed-auth records were missing, say
   so — that is REQ-A1's motivation.
4. Update `docs/threat-model.md` if the incident revealed an adversary or asset not modelled.

---

## 9. Known gaps that limit this runbook (state them, do not paper over)

- **No per-operator revocation** — rotation is fleet-wide and needs a restart (REQ-I4).
- **No failed-auth audit, no rate limiting** — brute force is invisible (REQ-A1, SEC-15).
- **No egress policy** — network isolation must be done by host tooling, not Bunker (REQ-E1).
- **No off-box anchor by default** — tamper-evidence is conditional (REQ-A2).
- **No compliance/retention statement** — retention is whatever your host does (REQ-D4).

Each gap maps to a tracked board row; this runbook should shrink these as controls land.
