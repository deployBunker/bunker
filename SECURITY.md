# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 1.x     | :white_check_mark: |

## Reporting a Vulnerability

**Do not open a public issue.** Email: wojons@wojonstech.com

Please include:
- Description of the vulnerability
- Steps to reproduce
- Affected versions
- Any potential mitigations you've identified

Response time: within 48 hours. We will keep you updated as we triage and address the issue.

## Security Model

Bunker provisions per-user Docker containers with:
- mTLS between `bunker` CLI and `bunkerd` server
- JWT-based API key authentication with scope enforcement
- cgroup resource limits (CPU, memory, PIDs)
- User namespace remapping for rootless containers
- SSH key isolation per container

**Out of scope:** social engineering, physical access, compromise of the host system by a user with root in their own container (containers are designed for root access).
