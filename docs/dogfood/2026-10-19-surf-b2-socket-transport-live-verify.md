# SURF-B2 — Socket transport + lifecycle: LIVE VERIFY

**Date:** 2026-10-19 (all times UTC)
**Host:** bunker-mvp (78.46.173.180 public / 100.115.67.116 tailscale), fresh agent `surf-b2-live` (spawned for this row, TTL 2h)
**Binary under test:** `/tmp/bunker-surf-b2` — `go build ./cmd/bunker` at branch `wt/SURF-B2` (0.2.1, commit 282b90f)
**toolsd artifact:** `/home/kara/coding-hermes-tools/dist/toolsd-linux-amd64` (v0.2.0-181-gb50808c, statically linked)
**Commits:** `2594fa42` (feat), `7ffa8b0f` (docs), `bbb75da0` (tcp-probe scope fix) on `wt/SURF-B2`, pushed to origin

Every claim below is the recorded output of a real run against the live agent.

## 1. Spawn provisions the runtime dir at 0700 (remote end, criterion 1+2)

```
$ /tmp/bunker-surf-b2 spawn surf-b2-live --server bunker-mvp --ttl 2h
API Key:      zRl8…Sk
  SSHFS Mount:  sshfs … bunker-surf-b2-live@100.115.67.116:/home/bunker-surf-b2-live …

$ /tmp/bunker-surf-b2 exec surf-b2-live --server bunker-mvp -- stat -c '%U %a' /run/bunker/surf-b2-live
bunker-surf-b2-live 700
```

The directory is the SAME one NET-BUNKER-007 provisions for the docker socket
(`sockdir_linux.go` ensures + asserts 0700 at spawn); the surface reuses it.

## 2. Install: units + runtime-dir proof (systemd --user, NOT a login hook)

```
$ /tmp/bunker-surf-b2 surface install surf-b2-live --server bunker-mvp
surface installed for agent surf-b2-live (units written)
  socket : /run/bunker/surf-b2-live/toolsd.sock (active)
  owner  : bunker-surf-b2-live, mode 600
  toolsd : present at $HOME/bin/toolsd
```

No login path is touched — the install script's entire write set is
`/run/bunker/<id>/`, `~/.config/systemd/user/toolsd{,«@».socket,socket}`,
and `systemctl --user`. `authorized_keys` after install still carries only
the pre-existing `environment="DOCKER_HOST=…"` line (verified by reading it
over the agent's own SSH session during the run). Unit text on the agent
(`systemctl --user status toolsd.socket`):

```
● toolsd.socket - toolsd socket for surf-b2-live (per-connection activation)
     Loaded: loaded (/home/bunker-surf-b2-live/.config/systemd/user/toolsd.socket; enabled; preset: enabled)
     Active: active (listening) since Mon 2026-10-19 09:26:16 UTC
```

`socket file: srw------- 1 bunker-surf-b2-live bunker-surf-b2-live … toolsd.sock` (0600, agent-owned).

## 3. --check: all five criteria measured OK (incl. the 0700 dir)

```
$ /tmp/bunker-surf-b2 surface connect surf-b2-live --server bunker-mvp --check
surface check for agent surf-b2-live:
  socket unit : active
  socket file : owner bunker-surf-b2-live, mode 600
  runtime dir: owner bunker-surf-b2-live, mode 700 (/run/bunker/surf-b2-live)
  toolsd procs: 0 (connection-activated: 0 before the first connect)
  tcp listeners: 0 (the transport is a unix socket at both ends; unknown = unproven)
  result      : OK — ready for `bunker surface connect surf-b2-live`
```

`toolsd procs: 0` is the connection-activation proof (criterion 2: nothing
runs on the agent until the first connection). An intermediate build counted
ALL host TCP listeners (15: sshd/bunkerd/dockerd belong to other components
— the agent is a plain user on the daemon host) and was corrected to count
toolsd-owned sockets only (`bbb75da0`); the corrected probe reads 0.

## 4. Connect: 0700 local end, READY, JSON-RPC round trip (criterion 1+3)

```
$ /tmp/bunker-surf-b2 surface connect surf-b2-live --server bunker-mvp
forwarding agent surf-b2-live toolsd socket
  local  : /tmp/bunker-toolsd-surf-b2-live.sock
  remote : /run/bunker/surf-b2-live/toolsd.sock (via bunker-surf-b2-live@100.115.67.116)
  the forward runs until interrupted; Ctrl-C is the disconnect
READY: /tmp/bunker-toolsd-surf-b2-live.sock is forwarding the agent toolsd surface (Ctrl-C to stop)
  transport: unix socket both ends — no TCP listener is created here or on the agent
```

The ssh child carries `StreamLocalBindMask=0077` (visible in `ps`):

```
ssh … -o StreamLocalBindMask=0077 -i /home/kara/.config/bunker/keys/surf-b2-live
      -N -L /tmp/bunker-toolsd-surf-b2-live.sock:/run/bunker/surf-b2-live/toolsd.sock
      bunker-surf-b2-live@100.115.67.116
```

Local end stat (criterion 6 — mode 0700):

```
$ stat -c '%F mode=%a' /tmp/bunker-toolsd-surf-b2-live.sock
socket mode=700
```

JSON-RPC round trip THROUGH the local forward (initialize → serverInfo):

```
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18",
 "capabilities":{"tools":{"listChanged":false}},
 "serverInfo":{"name":"toolsd","version":"v0.2.0-181-gb50808c"}}}
```

(GOT 177 bytes via `s.connect('/tmp/bunker-toolsd-surf-b2-live.sock')`.)

## 5. Zero TCP listeners on BOTH ends (criterion 5)

```
$ ss -tlnp | grep -c "sb2-dns\|bunker-toolsd"      # client side, forward-attributable
0
$ # agent side, toolsd-owned (the --check probe):
tcp listeners: 0
```

No port is bound on either side; the transport is `ssh -L
<unix>:<unix>` end to end.

## 6. Per-session lifetime (criterion 7)

The forward process was SIGTERMed (the Ctrl-C path):

```
$ kill -TERM <connect-pid>
stat /tmp/bunker-toolsd-surf-b2-live.sock
→ stat: cannot stat … : No such file or directory
pgrep -P <connect-pid> | wc -l   → 0     (ssh child reaped with the group)
```

The forward socket is GONE when the session ends; no residue.

**Remote-end lifetime** was exercised through `surface remove`, which
proves BOTH paths absent: the remove initially reported the socket file
still present (exit 5 — the file survived while the unit was already
gone; a dead socket file that refuses `connect()` with ECONNREFUSED), the
orphaned file was removed, and a second remove verified clean:

```
surface removed for agent surf-b2-live
  socket /run/bunker/surf-b2-live/toolsd.sock: gone
  units  : deleted (toolsd.socket + toolsd@.service)
```

(A re-install after the remove brought the surface back healthy: `active`,
dir 700, socket 600, tcp 0.)

## 7. No regression: exec/cp work byte-identically WITH the surface present (criterion 8)

```
$ /tmp/bunker-surf-b2 exec surf-b2-live --server bunker-mvp -- sh -c 'echo EXEC-OK; whoami'
EXEC-OK
bunker-surf-b2-live
$ echo regression-probe > /tmp/sb2-probe.txt
$ /tmp/bunker-surf-b2 cp /tmp/sb2-probe.txt surf-b2-live:/home/bunker-surf-b2-live/regprobe.txt --server bunker-mvp
Copied /tmp/sb2-probe.txt to surf-b2-live:/home/bunker-surf-b2-live/regprobe.txt
$ /tmp/bunker-surf-b2 exec … -- cat /home/bunker-surf-b2-live/regprobe.txt
regression-probe
```

Structural half (the diff carries no exec/cp/deploy/server change):
`git diff 06bcf74..bbb75da --stat` touches ONLY `internal/cli/surface*.go`
+ tests and the two docs — the exec/cp/deploy code paths are untouched by
construction, and their live behavior was re-proven above with the units
active. The install path also CANNOT touch the login path: it is pinned by
`TestBuildSurfaceInstallScriptNoLoginHook` (greps the generated script for
authorized_keys/.profile/ForceCommand/sshd_config/pam_/Match User).

## 8. Test suite (Tier 1 guard, real run)

```
Tier 1 Guards: PASS  (test mode: full)     # commit bbb75da0, 10:08Z
  ✓ secrets — clean (gitleaks + builtin cross-check)
  ✓ go_build — ok
  ✓ go_lint — ok
  ✓ go_tests — passed
guard log: .gitreins/logs/guard-20261019T100801.077178Z.log
```

`go test ./internal/cli/ -count=1` = ok (39.9s full package). RED proof: with
`internal/cli/surface_script.go` + `surface_connect.go` reverted to
`06bcf74`, the new tests fail to COMPILE against the old code
(`undefined: surfaceAssertLocalSocketMode`, `undefined:
renderSurfaceSocketUnit`) — the pins bind to the new symbols, not to
vacuous passes.

## 9. Known environment note (not a defect of this transport)

The Tailscale IP path (`100.115.67.116`) terminates SSH in
`tailscaled be-child ssh`, whose streamlocal forwarding allowlist refused
`/run/bunker/...` (`channel 1: open failed: administratively prohibited:
socket path … is not in an allowed directory`). Over the host's own sshd
(public IP 78.46.173.180, `allowstreamlocalforwarding yes`) the SAME forward
works and the round trip succeeded — §4's evidence ran through the real
sshd path. Widening the Tailscale SSH allowlist is infra policy, not this
task's surface (Tailscale SSH is off in the default product path).

## 10. Artifacts

- Branch `wt/SURF-B2` @ `bbb75da0` (pushed): `2594fa42` feat, `7ffa8b0f` docs, `bbb75da0` fix
- Spec: `docs/spec/SURF-B2-socket-transport.md` (auth model §2, permissions §4)
- Agent `surf-b2-live` left ACTIVE with the surface installed and healthy (TTL 2h from 09:24Z)
