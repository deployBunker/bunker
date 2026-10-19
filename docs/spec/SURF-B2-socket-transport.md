# SURF-B2 — Socket transport + lifecycle (agent-local 0700 unix socket)

Status: Implemented
Task: SURF-B2 (surface lane)
Related: SURF-006 (`bunker surface install/remove`), SURF-008
(`bunker surface connect`), NET-BUNKER-007 (per-agent socket dir 0700),
RECIPE-toolsd-socket-service.md (the proven mechanism this formalizes),
docs/prd/evidence-surfaces-mechanism.md (the live proof).

## 1. The transport model

```
client ──ssh -L──▶ agent host ──▶ /run/bunker/<agent-id>/toolsd.sock ──▶ systemd --user ──▶ toolsd mcp
        unix socket at BOTH ends                                     (Accept=yes, one process per connection)
```

The agent's tool surface is a systemd --user socket unit whose `ListenStream`
is the agent's own runtime-directory socket, `/run/bunker/<agent-id>/toolsd.sock`.
A client attaches by SSH local forwarding (`ssh -L
<local>:<remote>`), which is a **unix-socket-to-unix-socket** hop: OpenSSH
binds a LOCAL unix socket and relays every connection through the SSH
channel to the remote unix socket. No TCP listener is created on the client,
on the agent, or anywhere between — there is no port to open and no firewall
to change, because no TCP exists in this path.

SURF-B2 addresses the socket in `/run/bunker/<agent-id>/` instead of the
shared `/run/user/<uid>/` because that directory is the per-agent boundary
the daemon ALREADY provisions on every spawn: `internal/agent` creates it
with mode 0700, chowns it to the agent user, and STAT-verifies both
properties before spawn proceeds (NET-BUNKER-007, `sockdir_linux.go`). The
surface reuses that boundary instead of inventing a second one.

## 2. Auth model: the uid IS the auth; the SSH key remains the credential

Three properties, none of which are new credentials:

1. **Remote end — the uid is the auth.** The socket file is created by the
   agent's own systemd user manager (the agent's uid) inside a 0700
   directory owned by that same uid. Only a process running AS the agent's
   uid can connect to it locally. There is no socket-level credential, no
   token, no password — POSIX file ownership IS the access control, and the
   `socket_meta`/`dir_meta` checks (`bunker surface connect --check`)
   verify it instead of assuming it.
2. **Transport — the SSH key remains the credential.** A remote client
   reaches the socket only through the agent's own SSH identity: the key
   spawn persisted for the agent (`~/.bunker/keys/<agent-id>` client-side)
   logging in as `bunker-<agent-id>@<agent-host>`. Nothing about the socket
   introduces a second secret.
3. **No new exposure.** No port is bound anywhere, so no firewall rule
   changes. A client that cannot SSH to the agent cannot reach the surface,
   exactly as before this feature existed.

## 3. Lifecycle: systemd --user socket activation, NOT a login hook

- The units are `toolsd.socket` (ListenStream + `Accept=yes`) and
  `toolsd@.service` (`ExecStart=%h/bin/toolsd mcp`, stdio=socket), written
  into the agent's `~/.config/systemd/user` THROUGH the agent's own exec
  context, then `daemon-reload` + `enable` + `start`. Every spawn enables
  lingering for the agent user, so the user manager is alive without any
  interactive login (INT-SPAWN-004 heritage).
- **Nothing is installed into the SSH login path.** No
  `authorized_keys` edit, no `.profile` hook, no `ForceCommand`, no PAM
  artifact — this is pinned by `TestBuildSurfaceInstallScriptNoLoginHook`
  because `bunker exec`, `cp`, and `deploy` WORK BY RUNNING COMMANDS OVER
  SSH; a login hook would break the most-used path in the product.
- **The socket is connection-activated:** zero `toolsd` processes exist
  before the first local connection (`toolsd_procs=0` is a --check
  criterion), one `toolsd mcp` process per accepted connection, process
  exits when the connection closes.
- **Local-end lifetime:** the forward exists exactly as long as the
  `bunker surface connect` process does. Ctrl-C tears down the ssh child
  and its process group (the GAP-079 tunnel contract), which removes the
  local socket. There is no separate disconnect command and no residue.
- **Remote-end lifetime:** the socket lives under the agent's user manager
  and dies with the agent (destroy removes the user and the runtime
  directory). `bunker surface remove` proves BOTH the runtime-dir socket
  and the legacy `%t` socket absent before reporting success, so an
  upgraded agent cannot keep a hidden listener.

## 4. Permissions contract (measured, never assumed)

| Surface | Property | Enforcement |
|---|---|---|
| `/run/bunker/<agent-id>/` (remote dir) | mode 0700, agent-owned | install script: `mkdir -p` + explicit `chmod 700` + stat read-back; ABORTS (exit 7) unless exactly `700` |
| `/run/bunker/<agent-id>/toolsd.sock` (remote socket) | mode 0600, agent-owned | `SocketMode=0600` in the unit; install proves existence (exit 8 when absent); --check stats owner+mode |
| local forward socket | mode exactly 0700 | `StreamLocalBindMask=0077` in the ssh argv AND a stat read-back — the command refuses to print READY on any wider mode |
| TCP listeners | zero on both ends | --check counts `ss -H -tln` on the agent; `unknown` (no ss) FAILS — unproven is not safe |

The fallback if `systemd --user` is unavailable (no user manager after
linger): `bunker surface install` REFUSES with a named error rather than
writing units nothing will ever load (the SURF-006 preflight contract,
unchanged). There is deliberately no TCP fallback — a TCP listener would
break the transport's security model.

## 5. Upgrade + verification

- **Migration:** the idempotent unit write replaces a pre-SURF-B2
  `ListenStream=%t` unit on the next `surface install`, because the new
  rendered unit differs byte-wise; the final `daemon-reload` moves the
  listener to the runtime-dir address.
- **Verify live:**
  ```
  bunker surface install <agent-id> --server <srv>
  bunker surface connect <agent-id> --check   # socket_active / socket_meta / dir_meta / toolsd_procs / tcp_listeners
  bunker surface connect <agent-id> &         # READY line names the local 0700 socket
  ss -tlnp | grep -c bunker                   # 0 locally; the agent side is --check's tcp_listeners=0
  ls -la /tmp/bunker-toolsd-<agent-id>.sock   # srwx------ (0700)
  # Ctrl-C the forward: the local socket is gone (session lifetime)
  ```
- **No-regression:** the install path cannot touch the SSH login path
  (unit test pin), so `bunker exec`/`cp`/`deploy` output is unchanged by
  construction; the E2E battery exercises those verbs over the same exec
  RPC this feature uses for unit installation.
