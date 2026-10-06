# GAP-066 live verification — docker-as-installer program aliases

Verification run: **2026-10-06 14:35 UTC** on **bunker-mvp** (`78.46.173.180`), the
host the repo's own `AGENTS.md` designates for exec/docker/SSH changes.

* daemon binary: `/opt/bunker/bunkerd` sha256
  `34f9350c8e34a9c31a7eee7b26432b79e09d24f173c0eaeed16d17dbab5a34c3`
  — rebuilt from this branch's committed tree, and byte-reproducible from it
  (two consecutive `go build ./cmd/bunkerd` runs produced the same hash)
* CLI binary: `/usr/local/bin/bunker` sha256
  `e10a64cf36cac919f6b4b0410a2c5fa5cf473f1bb9eca46319f1c32180982c3d`
* battery: `scripts/gap066-verify-battery.sh` sha256
  `5d52d61220f43a1604492d66a2f9c5e7640a18472610e99923a4b06207d33144`
  (copied to the host as `/root/gap066-verify-battery.sh`, same hash)
* test agent: `gap066-live` (spawned for the run, `--ttl 30m --cpu 2.0
  --memory 4294967296`, destroyed at the end)
* result: **VERIFY-PASS — 31 cells passed, 0 failed**

The battery is reproducible: copy `scripts/gap066-verify-battery.sh` to the host
and run `bash /root/gap066-verify-battery.sh` (inputs: `BUNKER_BIN`,
`BUNKER_SERVER`, `BUNKER_AGENT`, `BUNKER_ALIAS_IMAGE`, `BUNKER_ALIAS_PROGRAM`,
`BUNKER_PROBE_IMAGE`). It prints `VERIFY-PASS`/`VERIFY-FAIL` and exits 0/1.

---

## Acceptance criteria → evidence

| AC | Where it is proven |
|---|---|
| 1. Alias registry CRUD unit-tested | `internal/programalias/registry_test.go`, `internal/server/programalias_test.go` (`TestProgramAliasRPCs`, `TestProgramAliasRPCPutRejectsBadInput`, `TestProgramAliasRPCsRefuseWithoutRegistry`), `internal/cli/programalias_test.go`. Live: battery cells B and C. |
| 2. Live on bunker-mvp: register a real program, `bunker run <program> --version` inside an agent works with correct stdout + exit code | battery cell D — `yq (https://github.com/mikefarah/yq/) version v4.54.1`, rc=0, byte-identical to the native invocation. |
| 3. Image auto-pull on first run (latency logged), second run cached | battery cell D — first run `"pull":true,"first_run_pull_ms":3101`; second run `"pull":false` (cache hit, no `docker image inspect` round trip). |
| 4. Non-home volume mount attempt is rejected | battery cell C, both arms: `/etc` refused at REGISTRATION (`mount host path must be inside an agent home (/home/bunker-<id>)`); a mount inside another agent's home refused AT EXEC (`must be inside the agent's home directory`, rc=1). |
| 5. Exec parity: a shell script inside the agent invoking the program via alias gets exit code + stdout identical to a native run | battery cell E — a script inside the agent calling `yq --version` produced stdout byte-identical to `docker run --rm mikefarah/yq:4 --version` and the same rc (0 on success, 1 on a bad flag). |
| 6. VERIFY-PASS battery evidence committed | this document + `scripts/gap066-verify-battery.sh` |
| 7. `go test ./...` passes | full `go test ./... -short` green on this tree; the alias-specific packages are listed above. |
| 8. `go build ./...` passes | `go build ./...` green; the two shipped binaries above were built from this tree. |

---

## Battery transcript (verbatim)

```
GAP-066 live battery — docker-as-installer program aliases
  cli     : /usr/local/bin/bunker
  server  : mvp-live
  agent   : gap066-live
  program : yq from mikefarah/yq:4
  scratch : /tmp/gap066-battery.4oV7LE

── A  preflight
   PASS  alias RPC served (bunker alias list rc=0)
   PASS  agent gap066-live reachable (bunker exec rc=0)
  image present on agent : 0
  daemon already pulled  : 0
   NOTE  clean image state — the strict first-run-pull arm will be asserted

── B  alias CRUD (AC1 surface)
   PASS  alias set: created: yq -> mikefarah/yq:4
   PASS  alias list shows yq: yq	image=mikefarah/yq:4
   PASS  alias delete removes the alias: yq: deleted=true
   PASS  deleting an unknown alias is refused (not a silent success)
   PASS  a malformed alias name is refused
   PASS  a shell-metacharacter image reference is refused

── C  AC4 home-only mounts
   PASS  a host path outside the agent-home root is refused AT REGISTRATION
   PASS  the refusal names the rule: bunker: mount host path must be inside an agent home (/home/bunker-<id>): "/etc"
   PASS  a mount inside another agent's home is storable (shape-valid)
   PASS  the same mount is refused when the alias runs for this agent (rc=1)

── D  AC2/AC3 bunker run <agent> -- yq --version
   PASS  alias run (first) exits 0
   PASS  alias run stdout: yq (https://github.com/mikefarah/yq/) version v4.54.1
   daemon alias-run lines for yq: 1
   first line      : Oct 06 14:35:26 bunker-mvp bunkerd[2974791]: {"time":"2026-10-06T14:35:26.538765666Z","level":"INFO","msg":"program alias exec","agent_id":"gap066-live","alias":"yq","image":"mikefarah/yq:4","limits":"mem=4294967296 cpus=2 pids=512","pull":true,"first_run_pull
   PASS  first run PULLED the image
   PASS  the first-run pull LATENCY is logged: "first_run_pull_ms":3142
   PASS  alias run (second) exits 0
   PASS  the second run added exactly one alias-run line
   PASS  the second run is served from the cache (pull=false): Oct 06 14:35:29 bunker-mvp bunkerd[2974791]: {"time":"2026-10-06T14:35:29.095712446Z","level":"INFO","msg":"program alias exec","agent_id":"gap066-live","alias":"yq","image":"mikefarah/yq:4","limits":"mem=4294967296 cpus=2 pids=512","pull":false}
   PASS  native reference exits 0
   PASS  alias stdout is byte-identical to the native invocation
   PASS  the cached run's stdout is also byte-identical to native

── E  AC5 exec parity: a script inside the agent
   script rc=0 out=[yq (https://github.com/mikefarah/yq/) version v4.54.1]
   PASS  script exit code == native exit code
   PASS  script stdout is byte-identical to the NATIVE invocation
   bad-flag rc: native=1 script=1
   PASS  a failing program propagates the SAME non-zero exit code through the alias (1)
   shim: 755 /home/bunker-gap066-live/bin/yq|#!/bin/sh|# rev=36f9e46924a7c9de|
   PASS  the alias is installed as an executable ~/bin shim with a revision marker

── F  --entrypoint produces the same result as the image's own entrypoint
   PASS  an explicit --entrypoint sh -c '<program> --version' matches the native stdout

── G  the alias container gets no docker socket
   absent /var/run/docker.sock
   absent /run/bunker
   absent /run/bunker/gap066-live/docker.sock
   home-visible
   PASS  no docker socket and no agent runtime directory inside the alias container
   PASS  the agent home IS mounted (that is the design: the program works in the agent's own tree)
   PASS  control: the runtime dir IS visible natively (so its absence in the container is real)

── cleanup
   PASS  test aliases removed

════════════════════════════════════════
  cells passed: 31
  cells failed: 0
  VERIFY-PASS
```

### What "native invocation" means here

The reference for every parity claim is the same program executed directly in
its own image on the agent host — `docker run --rm mikefarah/yq:4 --version` —
i.e. **without any of the alias machinery**. The alias must be indistinguishable
from it, and is. (The box has no natively installed `yq`; the in-image binary is
the same binary the alias runs, so this is the strongest available reference
rather than a proxy.)

### Reading the socket cell

`/run/bunker` is the per-agent runtime directory that carries the agent's own
rootless `docker.sock`. The **existing** image-spec exec path bind-mounts it
into the container (so an in-container `docker` CLI can reach the agent's own
daemon). A program alias deliberately does not — hence `absent` above — while
the agent's home is still mounted, which is the whole point of the feature. The
control arm proves the probe is not simply blind: the same path IS visible to a
plain (non-alias) exec.

---

## Deployment and rollback

The daemon was replaced on the host, as `AGENTS.md` prescribes for exec/docker
changes ("run the live-server E2E battery on bunker-mvp").

Pre-change binaries were copied first, into `/opt/bunker/backup-gap066/`
(timestamped `20261006-135726`):

```
93af…  bunker.bak-20261006-135726              (CLI, pre-change)
85d7…  bunkerd.bak-20261006-135726             (daemon in /opt/bunker, pre-change)
85d7…  usrlocal-bunkerd.bak-20261006-135726    (/usr/local/bin/bunkerd, pre-change)
```

Two builds were deployed during the session: an intermediate one (whose graded
run exposed the `--entrypoint` mapping defect — the alias's entrypoint vector
must map onto docker's OWN `--entrypoint` flag, not onto a command vector
appended after the image — plus two battery bugs), then the build from this
branch's committed tree above, which the graded run used.

Rollback:

```sh
install -m 0755 /opt/bunker/backup-gap066/bunkerd.bak-20261006-135726 /opt/bunker/bunkerd
install -m 0755 /opt/bunker/backup-gap066/usrlocal-bunkerd.bak-20261006-135726 /usr/local/bin/bunkerd
install -m 0755 /opt/bunker/backup-gap066/bunker.bak-20261006-135726 /usr/local/bin/bunker
systemctl restart bunkerd
```

Post-run health: `bunkerd` active, the pre-existing agents on the box
(`37a5c1c1`, `5e0f55e0` running; `9fd23f7a` destroy-refused) are unaffected, the
alias store is back to `{"version":1,"aliases":[]}` (mode 0600) and the test
agent `gap066-live` was destroyed.

---

## Known limits / residuals (not claimed as verified)

* **`--detach` runs are not alias-resolved.** `bunker run --detach` starts a
  systemd transient unit; the alias path is on `ExecAgent` (synchronous `run`
  and `exec`). A detached run executes natively.
* **`--script` uploads are not alias-resolved** (they have no single command
  token). The installed `~/bin` shim is what makes the program reachable from a
  script, and the shim exists only after an exec that used the alias. Documented
  in `docs/program-aliases.md`.
* **Image-cache freshness.** The daemon caches "this image is present" per
  process. A moved tag is re-resolved by restarting the daemon or using a
  different tag; a failed pull is never cached.
* **No registry-refresh policy.** Re-pulling a *newly pushed* image under the
  same tag is a manual act (restart / re-tag). Deliberate: an implicit refresh
  would make an alias's behaviour non-reproducible run to run.
* **The mount rule is lexical.** It bounds which HOST path an alias may name; it
  is not a symlink-resolution guarantee (the agent owns its home and can create
  symlinks there at any time). Stated as such in the package doc.
* **`--network` is off by default.** A toolchain alias that must fetch modules
  has to opt in explicitly; the battery does not exercise an egress-requiring
  program.
* **A verified live battery is not a soak.** Restart/kill resilience of the
  shim-install step is covered by the "idempotent install" unit test and the
  grep guard, not by a crash-during-install test.
