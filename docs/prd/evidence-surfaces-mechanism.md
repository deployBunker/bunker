# EVIDENCE: how a proxied toolsd service attaches to an agent (measured)

**Date:** 2026-09-20 · **Author:** Hermes · **Status:** mechanism PROVEN; design decided
**Question (Bane):** "when the bunkerd daemon is running, does it open a new process of toolsd
running as the user for the correct context, and then that is what is being forwarded down bunker?"

**Answer: no — and it should not.** The mechanism below was executed end to end against a live
agent. Nothing in it requires the daemon to manage a process.

## The mechanism, as proven

1. `toolsd` is delivered to the agent's `$HOME/bin` (`bunker agent-tools --install`).
2. **Two unit files are written into the agent's own `~/.config/systemd/user`** — a socket unit
   (`Accept=yes`) and a `@.service` template whose `ExecStart=%h/bin/toolsd mcp` with
   `StandardInput=socket` / `StandardOutput=socket`.
3. The **agent's own systemd user manager** (already running — `Linger=yes`, `user@1004.service
   active`) activates on first connect and starts `toolsd mcp` **as the agent user**.
4. SSH forwards that remote unix socket to the client (`-L <local.sock>:<remote.sock>`), using the
   **agent's own key and identity**.
5. The client speaks JSON-RPC over the socket.

The daemon's role is reduced to a **file drop** (the same shape as the toolsd delivery) plus the
audit path. It never spawns, supervises or restarts the service.

## Measured

```
units installed in /home/bunker-sock-proof/.config/systemd/user
socket unit   : active
socket path   : /run/user/1004/toolsd.sock
listener      : bunker-sock-proof  600        <- the AGENT owns it, mode 0600
processes     : 0                              <- connection-activated, nothing running before connect
agent host    : srw------- bunker-sock-proof bunker-sock-proof /run/user/1004/toolsd.sock  (hostname bunker-las-02)
forward       : local socket created (mode 600)
initialize    : {"name": "toolsd", "version": "98c75fa-dirty"}
tools/list    : 20 tools
   basic      : toolsd_read, toolsd_write, toolsd_list
   advanced   : toolsd_diff3, toolsd_patch, toolsd_apply, toolsd_lease
```

No TCP listener exists anywhere in the path. Nothing in it runs as root.

## What this changes in the plan

**The MCP-over-unix-socket surface needs NO new toolsd code.** `Accept=yes` socket activation
turns the ALREADY-SHIPPED stdio `toolsd mcp` surface into a socket-served surface. That is the
cheapest working surface and it exists today.

**The surface is argv-shaped, so it cannot drift.** Every MCP tool's contract is
`{args: [<everything after the verb>], cwd}` — a thin projection of the CLI:

```
toolsd_read   required: [args]   props: args (array), cwd (string)
toolsd_write  required: [args]   props: args (array), cwd (string)
toolsd_list   required: [args]   props: args (array), cwd (string)
```

Consequence: **a new CLI verb is exposed over the surface automatically**, and a generated
REST/OpenAPI surface is a THIRD projection of the same argv rather than a parallel API to keep in
sync. (Discovered the hard way: a call sending `{root, path, content}` fails with
`requires exactly 1 positional argument` — the contract is argv, not named fields.)

**REST/OpenAPI/Connect-RPC surfaces still need toolsd code.** Only the JSON-RPC-over-socket
surface is free. CHT-055/056 remain for the HTTP-shaped surfaces.

## Operational finding that cost several cycles (worth pinning)

**The daemon host is not the agent host.** The client's server entry `bunker-las-02` resolves to
`78.46.173.180`, whose hostname is **`bunker-mvp`** — a *different machine* from where that
daemon's agents run. Its agents run on **`100.116.99.35`** (Tailscale; hostname `bunker-las-02`;
LAN `192.168.123.65`).

Two consequences, both hit hard here:

- Checks run against `78.46.173.180` say nothing about an agent. They showed a socket that "is not
  visible from the host" and users whose names "differ across the namespace boundary" — both
  artifacts of probing the wrong box. The namespace IDs were even IDENTICAL between "host" and
  "agent" (`mnt:[4026531841]`), which is what eventually exposed the error: identical namespaces
  with different hostnames is impossible on one machine.
- **There is no root SSH from this box to the agent host.** The working identity is the agent's
  own: `bunker-<agent-name>@100.116.99.35` with `~/.bunker/keys/<agent-name>` — which is also the
  correct least-privilege choice, since the socket is the agent's own.

Resolve the real host from the agent record (`bunker info <agent>` → the `SSHFS Mount` /
`Docker Tunnel` lines), never from the client config. And ask the agent for its identity
(`bunker exec <id> -- id -un`) rather than trusting host-side `getent`.

## Reproduce

`tools/surf-mechanism-proof.sh` (this repo) — spawns an agent, delivers toolsd, installs the units
through the agent's own context, activates the socket, forwards it over SSH, and speaks JSON-RPC,
then cleans up.
