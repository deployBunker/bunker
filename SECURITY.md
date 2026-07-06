# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| 0.1.x   | ✅                 |

## Reporting a Vulnerability

**Do not open a public issue.** Send vulnerability reports to:

- Email: `security@bunker.sh` (preferred)
- Or DM `@Bane` on the project's communication channel

You will receive an initial response within **48 hours** and a status update within **5 business days**.

### What to include

- Description of the vulnerability
- Steps to reproduce
- Affected versions
- Any potential mitigations you've identified

### Process

1. Report received → acknowledged within 48h
2. Triage and reproduce → 5 business days
3. Fix developed → private patch
4. Coordinated disclosure → public advisory + CVE request if warranted

## Security Model

Bunker isolates AI agents via Linux user accounts with rootless Docker. Key boundaries:

- **Agent isolation:** Each agent is a separate Linux user with its own rootless Docker daemon. Agents cannot access each other's containers, files, or processes.
- **Auth:** Dual-tier JWT (master tokens + agent-scoped sub-keys). Static bearer tokens for bootstrap. mTLS for production deployments.
- **Network:** Agents are firewalled from each other. Public access only through Cloudflare tunnels or Tailscale.
- **Resource limits:** CPU, memory, disk, processes, and open files are enforced via cgroups (systemd user slices).

If you find a way to escape agent isolation, bypass auth, or exceed resource limits, that is a critical vulnerability — please report it immediately.
