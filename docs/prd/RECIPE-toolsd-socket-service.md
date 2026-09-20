# RECIPE: the remote file tools on an agent, over a socket (proven, executable)

**Status:** every step below was executed against a live agent on 2026-09-20 and worked.
**Provenance:** `docs/prd/evidence-surfaces-mechanism.md`; reproducer `tools/surf-mechanism-proof.sh`.
**Audience:** whoever implements this. Nothing here is a sketch — the exact unit text, the exact
commands, and the exact checks are given.

This exists because the DESIGN question ("does the daemon spawn toolsd as the user?") was answered
by execution, and the answer is reusable: **the agent's own systemd starts the service as the agent
on first connect, and SSH hands the client the socket. The daemon is not in the data path.**

## The shape

```
client ──SSH────▶ agent host ──▶ /run/user/<uid>/toolsd.sock ──▶ systemd --user ──▶ toolsd mcp
                                                                  (spawns AS THE AGENT on connect)
```

Nothing listens on TCP. Nothing runs as root. There is no long-lived daemon holding state: with
`Accept=yes` each **connection** gets its own short-lived process.

## Step 1 — the artifact

The binary must be **static** and version-stamped, or it dies on the far side for a reason nobody
can see. In the toolkit repo:

```
make dist      # CGO_ENABLED=0, -trimpath, stamped; proves static + stamp, refuses otherwise
```

Then deliver it onto the agent:

```
bunker agent-tools <agent-id> --install --binary <toolkit-repo>/dist/toolsd-linux-amd64
```

That lands it in the agent's `$HOME/bin`, which the server **already** puts on every exec PATH, so
no shell profile is touched. The command re-probes the agent afterwards, so a failure is loud.

## Step 2 — the two unit files

Written into the agent's OWN `~/.config/systemd/user`. `%t` resolves to that user's runtime dir and
`%h` to its home, which is why the SAME text is correct for every agent.

`~/.config/systemd/user/toolsd.socket`
```ini
[Unit]
Description=toolsd socket (per-connection activation)
[Socket]
ListenStream=%t/toolsd.sock
SocketMode=0600
Accept=yes
[Install]
WantedBy=sockets.target
```

`~/.config/systemd/user/toolsd@.service`
```ini
[Unit]
Description=toolsd MCP (one process per connection, as the agent user)
[Service]
ExecStart=%h/bin/toolsd mcp
StandardInput=socket
StandardOutput=socket
```

`Accept=yes` is what makes this work with an UNMODIFIED stdio binary: systemd accepts the
connection and hands it to the service as stdin/stdout. **No bespoke socket mode is needed in
toolsd** — that is the single most useful finding here.

## Step 3 — install and activate, THROUGH THE AGENT'S CONTEXT

```
bunker exec <agent-id> -- sh -c '
  mkdir -p "$HOME/.config/systemd/user"
  # ...write the two unit files above...
  systemctl --user daemon-reload
  systemctl --user start toolsd.socket
  systemctl --user is-active toolsd.socket
'
```

**Critical: run this via `bunker exec`, not from the host.** The host cannot reliably address the
agent's user manager by name, and the agent's own identity is the only one that owns `%t`.

## Step 4 — the client attaches

```
ssh -i ~/.bunker/keys/<agent-id> \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    -N -L /tmp/toolsd.sock:/run/user/<uid>/toolsd.sock \
    bunker-<agent-id>@<agent-host>
```

Use the **agent's own key and identity** — not root. Least privilege, and it is also what actually
exists.

## Step 5 — talk to it

JSON-RPC over the socket. The contract is **argv-shaped**: `{args: [...], cwd}` where `args` is
everything after the verb.

```jsonc
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
// -> 20 tools

{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{
  "name":"toolsd_read", "arguments":{"args":["--root","/tmp","note.txt"]}}}
```

A client sending named fields (`{root, path, content}`) fails with
`requires exactly 1 positional argument`. The argv shape is the contract, and it is why a new CLI
verb is exposed automatically and the surface cannot drift.

## Verification that actually means something

```
systemctl --user is-active toolsd.socket      # active
stat -c '%U %a' /run/user/<uid>/toolsd.sock   # the AGENT, 600
pgrep -c toolsd                               # 0 BEFORE connect -> connection-activated
```

Then: `initialize` returns `{"name":"toolsd","version":...}`; `tools/list` returns 20 tools
including `toolsd_read`/`toolsd_write`/`toolsd_list` and `toolsd_patch`/`toolsd_apply`/
`toolsd_diff3`/`toolsd_lease`; and a real `toolsd_write` + `toolsd_read` round-trip succeeds
through the socket.

## Gotchas that cost time (all verified)

- **The daemon host is not the agent host.** Resolve the real agent host from the agent record
  (`bunker info <agent>` → the SSHFS Mount / Docker Tunnel lines). Getting this wrong produced two
  confident false conclusions in one session.
- **Ask the agent who it is** (`bunker exec <id> -- id -un`); do not trust host-side `getent`. The
  agent's user name is `bunker-<agent-name>`.
- **`%t`, not a hardcoded path.** `/run/user/<uid>` is the user's own runtime dir; hardcoding
  breaks the per-user property that makes this safe.
- **There is no root SSH from the control box to the agent host.** The agent's key is the working
  (and correct) identity.
- **The workspace root must exist before the file verbs are useful** — `fsops` refuses a
  non-existent `--root` by design, because a root that does not exist cannot be a confinement
  boundary. It is named, not silent, but it is a real precondition.

## What is NOT solved here (do not ship as if it were)

1. **Audit continuity (SURF-004) — a gate.** This path bypasses the daemon, so operations over the
   socket are absent from the hash-chained audit trail (GAP-047..050). Measured, not predicted.
   Solve before a MUTATING verb ships.
2. **The units are hand-written today.** Making install/remove a supported, idempotent step is
   SURF-006 (in flight).
3. **HTTP-shaped surfaces need real toolsd code.** Only the JSON-RPC-over-socket surface is free.
   That is SURF-007 / CHT-055.
4. **Lease invariant across two concurrent connections** is unproven (SURF-003).
5. **This is a TRANSPORT, not a security boundary.** The service runs as the agent user, so
   anything running as that user — including the untrusted repo code the agent executes — can call
   every verb. Never document it as isolation.
