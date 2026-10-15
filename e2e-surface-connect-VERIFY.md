# SURF-008 — End-to-end remote file tools over the toolsd socket: LIVE VERIFY

**Date:** 2026-10-15
**Host:** bunker-mvp (100.115.67.116), fresh agent `surf008-e2e` (spawned for this row, TTL 2h)
**Binary under test:** `/tmp/bunker-surf008` — `go build ./cmd/bunker` at branch `surf-008-surface-connect`
**toolsd artifact:** `/home/kara/coding-hermes-tools/dist/toolsd-linux-amd64`
  sha256 `4eddac2c896b5e2344c567f58873de45ea8f5f7cbfb9f2538d86b74212538258` (statically linked)

Every command below ran through `bunker surface connect` / `--check` / `surface install` /
`agent-tools` — one command per step, no hand-written files on the agent.

## 1. Fresh agent, one-command install (SURF-006 path, the connect precondition)

```
$ bunker surface install surf008-e2e --server bunker-mvp
surface installed for agent surf008-e2e (units written)
  socket : /run/user/1393/toolsd.sock (active)
  owner  : bunker-surf008-e2e, mode 600
  units  : $HOME/.config/systemd/user + $HOME/.config/systemd/user
bunker: WARNING toolsd is absent at the agent's $HOME/bin/toolsd — the socket is up but
        will activate nothing until the binary is delivered: bunker agent-tools surf008-e2e --install
```

(The WARNING is the honest dependency probe: units management ≠ binary delivery.)

```
$ bunker agent-tools surf008-e2e --server bunker-mvp --install --binary \
    /home/kara/coding-hermes-tools/dist/toolsd-linux-amd64
delivered toolsd: toolsd version v0.2.0-181-gb50808c (built 2026-10-09T19:38:56Z)
  toolsd   present  yes   toolsd version v0.2.0-181-gb50808c
  rg       present  yes   ripgrep 14.1.0
```

Side proof of the delivery guard: the first attempt with the locally-built (dynamically
linked) toolsd was REFUSED by name — "refusing to deliver /home/kara/go/bin/toolsd: it is
DYNAMICALLY linked …". The static dist artifact was required.

## 2. Idempotence: re-install changes nothing

```
$ bunker surface install surf008-e2e --server bunker-mvp
surface installed for agent surf008-e2e (units unchanged (already current))
```

Byte/mtime proof across the re-run (stat + sha256sum on the agent, before vs after):

```
/home/bunker-surf008-e2e/.config/systemd/user/toolsd.socket  1792072521  159   (mtime unchanged)
/home/bunker-surf008-e2e/.config/systemd/user/toolsd@.service 1792072521  159   (mtime unchanged)
8d7f9da0d8d3a2d81ee8e665ba38a4beb275898fba6aaa47cc7e42166a60e824  toolsd.socket    (identical)
d8992dccb5b8c9f691fcca439374024333e2043d05e70a4a9fdae37eaa71ac80  toolsd@.service  (identical)
```

## 3. `bunker surface connect <id> --check` — the recipe's three live checks

```
$ bunker surface connect surf008-e2e --server bunker-mvp --check
surface check for agent surf008-e2e:
  socket unit : active
  socket file : owner bunker-surf008-e2e, mode 600
  toolsd procs: 0 (connection-activated: 0 before the first connect)
  result      : OK — ready for `bunker surface connect surf008-e2e`
CHECK_RC=0

$ bunker surface connect surf008-e2e --server bunker-mvp --check --json
{"agent":"surf008-e2e","socket_active":"active","socket_owner":"bunker-surf008-e2e",
 "socket_mode":"600","toolsd_procs":"0","ok":true}
```

- `systemctl --user is-active toolsd.socket` → active
- `stat -c '%U %a' /run/user/1393/toolsd.sock` → `bunker-surf008-e2e 600` (the AGENT, 0600)
- `pgrep -c -x toolsd` → **0 before any connect** — connection-activated proof

## 4. `bunker surface connect` — the forward comes up

```
$ bunker surface connect surf008-e2e --server bunker-mvp      # background
forwarding agent surf008-e2e toolsd socket
  local  : /tmp/bunker-toolsd-surf008-e2e.sock
  remote : /run/user/1393/toolsd.sock (via bunker-surf008-e2e@100.115.67.116)
  the forward runs until interrupted; Ctrl-C is the disconnect
READY: /tmp/bunker-toolsd-surf008-e2e.sock is forwarding the agent toolsd surface (Ctrl-C to stop)
```

Host resolution came from the AGENT RECORD (sshfs mount line
`bunker-surf008-e2e@100.115.67.116`), the uid was discovered by asking the agent
(`id -u` → 1393), and the ssh child ran with the agent's own key:

```
159582  /tmp/bunker-surf008 surface connect surf008-e2e --server bunker-mvp
160744  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
        -o IdentitiesOnly=yes -o ExitOnForwardFailure=yes \
        -i /home/kara/.config/bunker/keys/surf008-e2e -N \
        -L /tmp/bunker-toolsd-surf008-e2e.sock:/run/user/1393/toolsd.sock \
        bunker-surf008-e2e@100.115.67.116
local:  srw------- 1 kara kara /tmp/bunker-toolsd-surf008-e2e.sock
```

(No root, no daemon host — the exact shape recipe Step 4 prescribes, as a supported command.)

## 5. JSON-RPC over the forwarded socket: initialize → tools/list → write/read

```
INITIALIZE: {"name": "toolsd", "version": "v0.2.0-181-gb50808c"}
TOOLS_LIST_COUNT: 22
HAS_READ_WRITE: ['toolsd_read', 'toolsd_write']
HAS_ADVANCED: ['toolsd_apply', 'toolsd_diff3', 'toolsd_lease', 'toolsd_patch']
```

Real round-trip (argv-shaped args; `--root` points at `$HOME/surf008-root`, created via
`bunker exec` first — recipe gotcha #6). toolsd_write consumes content from stdin, so the
call carries `stdin` (the stdin contract, CHT-061):

```
toolsd_write {"args":["--root","/home/bunker-surf008-e2e/surf008-root","surf008-verify.txt"],
              "stdin":"written through the SURF-008 forward\n"}
WRITE_ISERROR: False
WRITE_TEXT: wrote 37 byte(s) to surf008-verify.txt

toolsd_read {"args":["--root","/home/bunker-surf008-e2e/surf008-root","surf008-verify.txt"]}
READ_TEXT: 'written through the SURF-008 forward\n'
```

## 6. Confinement refusal through the socket

A read whose path escapes the root is REFUSED with the named cause:

```
toolsd_read {"args":["--root","/home/bunker-surf008-e2e/surf008-root","../../etc/passwd"]}
ESCAPE_ISERROR: True
ESCAPE_TEXT: toolsd: read: fsops: path ../../etc/passwd: fsops: path outside root
```

(Control: `--root /etc passwd` reads fine — /etc EXISTS, so it is a valid confinement
boundary, and /etc/passwd is world-readable to any user. Confinement is relative to the
given root; the refusal arm is a path OUTSIDE that root. The recipe's gotcha #6 — a
non-existent root is refused by design — was respected by creating the root first.)

## 7. Per-connection activation, as the agent user

After two JSON-RPC connections came and went, ON the agent:

```
journalctl --user -u "toolsd@*.service":
  uid=1393 euid=1393 cwd=/home/bunker-surf008-e2e cmd=/home/bunker-surf008-e2e/bin/toolsd mcp
  Started toolsd@1-24785-0.service - toolsd MCP (one process per connection, as the agent user)
procs_after connections: 0        # each connection's process exited with it
stat: bunker-surf008-e2e 600      # socket still agent-owned, 0600
```

## 8. Ctrl-C is the disconnect: the subtree is reaped

`SIGTERM` to the connect CLI (the test suite proves the same contract for SIGINT/SIGKILL;
GAP-079):

```
$ kill -TERM 159582   # the `bunker surface connect` process
CLI_ALIVE:  no
SSH_ALIVE:  no        # the ssh forward child died with it (group teardown + Pdeathsig)
/tmp/bunker-toolsd-surf008-e2e.sock: No such file or directory   # local socket removed
```

No orphaned ssh, no residual forward, no leftover socket. There is no separate
`disconnect` verb by design: the forward lives exactly as long as the process does.

## Verdict

PASS — every acceptance gate executed live: one-command install, idempotence (hash+mtime),
socket 0600 agent-owned, 0 pre-connect toolsd processes, initialize + 22-tool tools/list +
write/read round-trip over the forwarded socket, named confinement refusal, clean
signal-driven teardown. The unit-test suite additionally proves host resolution from the
agent record, uid discovery, arg building, fail-closed binding (GAP-093), and child reaping
through the real binary.
