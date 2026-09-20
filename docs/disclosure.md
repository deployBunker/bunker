# Vulnerability Disclosure Process

**Status:** v1 (2026-09-20). Panel finding SEC-20: the previous policy was "email us" with no
embargo, no advisory path, and no security-release channel. This defines the process.

---

## 1. Reporting

**Do not open a public issue for a security vulnerability.**

Report privately to **wojons@wojonstech.com** with:

- A description of the vulnerability and its impact.
- Reproduction steps (a minimal case is ideal).
- Affected versions / commit.
- Any mitigations you have identified.
- Whether you intend to publish, and on what timeline.

We acknowledge within **48 hours** and will keep you updated through triage and fix.

**Encryption:** if your report contains a live credential or an exploit, request the current
PGP key in your first (content-free) message and we will send it before you transmit details.

---

## 2. Embargo

- **Default embargo: 90 days** from acknowledgement, or the date a fix ships, whichever is
  earlier.
- **Shorter, coordinated disclosure** is available by agreement for actively exploited issues
  or when the reporter has a publication deadline — ask; we prefer to coordinate rather than
  be surprised.
- **We will not ask for an open-ended embargo.** If 90 days pass without a fix, the reporter
  may disclose, and we will publish what we know alongside them.
- **Critical, actively-exploited issues** are published as an advisory as soon as a fix or
  mitigation exists, not held for the full window.

---

## 3. What we publish

Once a fix ships (or the embargo ends), a GitHub Security Advisory is published containing:

- A description, the affected versions, and the fixed version.
- The CVE id, when one is assigned.
- Credit to the reporter (unless they ask to remain anonymous).
- A "fix / workaround / residual risk" section — stating honestly if the fix is partial.

A **security-release channel** carries the same content:

- GitHub Security Advisories (watch the repository → *Custom → Security alerts*).
- A release tagged `security-*` with the advisory linked in its notes.

---

## 4. Supported versions

| Version | Security fixes |
|---|---|
| `1.x` (current) | ✅ |
| Older | Upgrade required; no backports |

---

## 5. Our commitments to the reporter

- **Prompt acknowledgement** (48 h) and honest status updates.
- **Public credit** for a valid report, unless anonymity is requested.
- **No legal action** against good-faith research that respects this process and does not
  access or exfiltrate others' data.
- **We will say when we cannot fix something.** Some findings here are residual risk by design
  (§7 of `docs/threat-model.md`); we will state that plainly rather than leave a report open
  indefinitely.

---

## 6. Internal handling (maintainers)

1. **Triage within 48 h**: reproduce, assign severity (BLOCKER/HIGH/MED/LOW), open a private
   board row. BLOCKER/HIGH get a fix bar; MED/LOW are scheduled.
2. **Fix on a private branch**; do not push a public commit that announces the bug before the
   advisory. Land the fix, tag a `security-*` release, then publish the advisory.
3. **Rotate any credential** that the report shows to have been exposed — see
   `docs/incident-runbook.md` §6.
4. **Update `docs/threat-model.md`** if the report reveals a new adversary or asset.
5. **Close the loop** with the reporter before publishing.

---

## Related documents

- `SECURITY.md` — the public policy (scopes, contact, supported versions).
- `docs/threat-model.md` — adversaries, boundaries, residual risk.
- `docs/incident-runbook.md` — operational response.
- `docs/compliance.md` — data handling.
