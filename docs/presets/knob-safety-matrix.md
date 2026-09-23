# Knob-safety matrix — measured safety-vs-experience evidence per knob (GAP-114)

Evidence base for the GAP-119 tier expansion. Every number in this document was
captured on this host during the GAP-114 session (2026-09-22); nothing is
quoted from documentation. Each section names the probe script in
`probes/knob-safety/` that produces its evidence; re-run the script to
reproduce the output verbatim.

Partner row: GAP-113 (trust-tier preset matrix) should cite this file instead
of guessing knob defaults. GAP-116 (preset plumbing) and GAP-117 (standard =
today's five knobs: CPUQuota 2.0, MemoryMax 4GiB, TasksMax 4096, LimitNOFILE
65536, LimitFSIZE 20GiB via `DefaultDiskBytes`) are already landed —
`internal/config/config.go:655-660`, `internal/agent/gap117_test.go:248-260`.

## Environment & method

| fact | value (measured) |
|---|---|
| host kernel | 7.0.0-31-generic |
| systemd | 259 (259.5-0ubuntu3.4), user manager accepted all probes |
| docker (server) | 29.1.3, cgroup v2 drivers (memory + pids + io controllers present) |
| CPU / RAM / swap | 16 cores, 59Gi RAM, 55Gi swap (2 swapfiles), NVMe root (`/dev/nvme0n1`, 1.8T) |
| session privileges | unprivileged user `kara` (uid 1000), docker group; NO sudo used |
| dmesg | `dmesg_restrict=1` — kernel "fork rejected by pids controller" lines NOT readable; errno-level evidence used instead |
| unprivileged userns | BLOCKED host-wide (`kernel.apparmor_restrict_unprivileged_userns=1`; `unshare --user --map-root-user` -> `write failed /proc/self/uid_map: Operation not permitted`) |
| delegated cgroupfs | controller knob writes DENIED even inside `Delegate=yes` scopes with uid-owned files (measured; see OOM-group section) |

Method notes:

- Container-level knobs were applied with `docker run` limits (they are the
  same cgroup-v2 files systemd writes: `-m` == `MemoryMax=`,
  `--memory-swap==--memory` == `MemorySwapMax=0`, `--pids-limit` ==
  `TasksMax=`, `--device-write-bps` == `IOWriteBandwidthMax=`).
- Unit-level knobs were applied with `systemd-run --user` transient units.
- Workloads read their OWN cgroup counters (`/proc/self/cgroup`-relative
  `memory.peak`, `memory.events`, `pids.current`) — kernel measurements, not
  process guesses.
- Probe scripts are re-runnable and fail loudly: an unexpected exit code
  aborts the script (`PROBE-FAIL`), so a silently-passing cell is impossible.
- Abuse cases used throwaway docker resources, all removed on exit (verified:
  `docker container list -a` shows zero `gap114-*` containers after runs;
  dd artifacts and the toolchain image were cleaned; the toolchain image
  rebuilds itself on the next probe run).

## Measured corrections to the brief's assumptions

These were measured during probing; the doc records them so GAP-119 does not
ship on the wrong mechanism story:

1. **memlock overshoot is ENOMEM, not EPERM** (on modern kernels): a tiny
   `RLIMIT_MEMLOCK` makes `mlock`/`mlockall` fail with `Cannot allocate
   memory` (ENOMEM), aborting the workload with exit 1. The EPERM story is
   the pre-2.6.9 behavior. Failure outcome is identical (abort), errno story
   differs.
2. **docker `--memory-reservation` is a cgroup-v2 no-op**: with
   `--memory-reservation=100m` requested, the container's `memory.high`
   stayed `max` (host view and in-container view both). It did not throttle a
   308MiB-peak workload at all (`high 0` events). The knob GAP-119 should use
   is systemd `MemoryHigh=` (measured effective below).
3. **ProtectSystem silently no-ops at the user-manager level** on this host:
   /etc writes fail EACCES with AND without the knob (plain uid ownership),
   and /tmp stayed writable under `ProtectSystem=strict`. The EROFS mechanism
   is real but only measurable where the knob is real (container layer /
   root units) — reproduced via `docker run --read-only`.
4. **ftruncate past the LimitFSIZE cap is itself EFBIG-capped**: the
   linuxserver/.NET sparse-reservation pattern (ftruncate a 2TiB sparse file
   before first write) dies at the `ftruncate`, not at the first write. Same
   crash-loop, earlier in the failure chain.
5. **`io.max` rejects partitions**: `--device-write-bps /dev/nvme0n1p2:...`
   fails container create with `no such device` (cgroup v2 `io.max` takes the
   whole disk `259:0`, not the partition `259:2`). Bunker spawn code must
   resolve the whole-disk device, not the root filesystem's partition.
6. **SystemCallFilter deny-lists SIGSYS-kill, docker seccomp returns EPERM**:
   the same "deny mkdir" idea kills the process under systemd (exit 159 =
   128+31) but returns clean `Operation not permitted` under docker seccomp.
   A deny-listed agent workload dies mysteriously under systemd but fails
   legibly under docker.

---

## Per-knob sections (GAP-119 candidates)

### MemoryMax (existing; measured for headroom calibration)

- **Axis / mechanism**: hard memory ceiling; kernel OOM-kills the cgroup when
  usage hits it (cgroup v2 `memory.max`; docker `-m`).
- **Probe**: `probes/knob-safety/probe-a-memmax.sh` (executed this session).
- **Measured effect on a build-like workload** (allocator peaking at
  300MiB target, kernel-measured):

  ```
  OBSERVED label=a-tight-075x exit=137 oomkilled=true wall=1.9s
  OBSERVED label=a-tight-100x exit=137 oomkilled=true wall=2.5s
  WLK-DONE peak_target=300MiB ramp=2.1s pid=1        (1.25x run)
  WLK-PEAK memory.peak=331935744 memory.events=low 0 high 0 max 0 oom 0 oom_kill 0 ...
  OBSERVED label=a-tight-125x exit=0 oomkilled=false wall=5.6s
  WLK-PEAK memory.peak=331227136 ...                 (1.5x run)
  OBSERVED label=a-tight-150x exit=0 oomkilled=false wall=5.6s
  ```

  The workload's true peak was **316.5MiB** (`memory.peak=331935744` ≈ 317MiB,
  i.e. ~5% above the allocator's own 300MiB target — runtime overhead is
  real and must be budgeted). 1.0x of the *allocator* target died; **1.25x
  was the minimum safe multiple**.
- **Measured abuse containment**: see abuse section — an 8GiB memory bomb
  under a 1GiB MemoryMax was OOM-killed at the cap; host MemAvailable moved
  +118MiB (host unaffected).
- **Verdict**: open = generous/no limit; standard = 4GiB (today's default;
  ~12x headroom over the measured 317MiB realistic peak); guarded = 2GiB;
  hostile = 1GiB. OOM death is instant and the workload is innocent-safe
  (kernel kills the cgroup, not the host).

### MemorySwapMax (new; measured via `--memory-swap == --memory`)

- **Axis / mechanism**: swap allowance on top of MemoryMax; cgroup v2
  `memory.swap.max`. Setting it to 0 (= `--memory-swap == --memory`) bars
  swap entirely.
- **Probe**: `probes/knob-safety/probe-b-swapmax.sh` (executed this session).
- **Measured**:

  ```
  === b-swapbarred-100x (memory=300m memory-swap=300m) ===  -> exit=137 oomkilled=true  wall=3.0s
  === b-swapallowed-100x (memory=300m memory-swap=600m) ===
  WLK-PEAK memory.peak=314572800 swap.current=17973248 swap.peak=17973248
  WLK-EV memory.events=... max 53 oom 0 oom_kill 0 ...      -> exit=0 wall=6.7s
  === b-swapallowed-050x (memory=150m memory-swap=450m) ===
  WLK-PEAK memory.peak=157286400 swap.current=174313472 swap.peak=174510080
  WLK-EV memory.events=... max 637 oom 0 oom_kill 0 ...     -> exit=0 wall=6.4s
  === b-swapbarred-050x (memory=150m memory-swap=150m) ===  -> exit=137 oomkilled=true  wall=1.6s
  ```

- **Reading**: with swap barred, a workload at exactly its limit dies LOUDLY
  and fast (OOM at the cap). With swap allowed, the same under-sizing is
  MASKED: the 1.0x run completed with 17MiB of swap absorbing the overshoot
  (`max 53` hits, no kill), and a 0.5x run completed only by swapping
  **166MiB — more than half the workload** — with no kill. On this NVMe host
  the swap-slowdown was modest (6.4s vs 6.7s); on a slower-disk bunker the
  same masking becomes swap-thrash that ALSO degrades host responsiveness.
- **Verdict**: standard/guarded/hostile = **bar swap** (`MemorySwapMax=0`):
  under-provisioning must fail loudly, not silently page the host.
  open = leave host default (swap allowed). WARNING for GAP-113: bar-swap
  must NOT be applied to `open`, where the measured outcome is turning a
  would-have-completed run into an OOM.

### MemoryHigh (new; measured via systemd `MemoryHigh=`)

- **Axis / mechanism**: soft throttle ceiling — allocation is slowed (and
  spills to swap) but nothing is killed (cgroup v2 `memory.high`).
- **Probe**: `probes/knob-safety/probe-c-memhigh.sh` (executed this session).
- **Measured** (same 300MiB-target workload under a user-level unit):

  ```
  MemoryHigh=none : wall=0.6s  peak=342728704  events high 0
  MemoryHigh=250M : wall=0.7s  peak=262402048 (swap 84.2M)  events high 171
  MemoryHigh=150M : wall=0.9s  peak=157548544 (swap 183.4M) events high 601
  MemoryHigh=80M  : wall=1.0s  peak=84148224  (swap 253.5M) events high 492
  ```

  The peak pins EXACTLY at the cap (80.2M vs 80M), excess spills to swap,
  the kernel raises the throttle counter (`high` 171-601 events), and the
  workload finishes — **+51% wall at the most aggressive cap, zero kills**.
  Note the +51% is on a 0.4s synthetic ramp; for real minutes-long builds
  the percentage cost of a 90%-of-Max cushion is far smaller.
- **Verdict**: default-on candidate for standard/guarded/hostile as a
  cushion slightly under MemoryMax (e.g. `MemoryHigh=90% of MemoryMax`):
  measured graceful, no failure mode, converts "sudden OOM" into "throttle
  then OOM only if truly hopeless". open = off.

### MemoryOOMGroup (new; UNMEASURED-here — do not default on)

- **Axis / mechanism**: `memory.oom.group=1` reaps the whole cgroup subtree
  atomically on OOM instead of one victim; prevents half-dead process groups.
- **Probe**: `probes/knob-safety/probe-d-oomgroup.sh` (executed; mechanism A
  portion records refusals and re-runs them).
- **Measured contrast of what it would prevent** (docker pair, independent
  cgroups — no group semantics at docker level):

  ```
  OBSERVED victim: state=exited oomkilled=true exit=137
  OBSERVED sibling: state=running (still Up despite the pair)
  ```

  And the flag people reach for instead is dead on v2:

  ```
  OBSERVED docker-run-with-oom-kill-disable rc=0 stderr=WARNING: Your kernel does not support OomKillDisable. OomKillDisable discarded.
  OBSERVED protected-victim: state=exited oomkilled=true exit=137
  ```

- **Why the knob itself is UNMEASURED-here** (all three unprivileged paths
  captured live, 2026-09-22):

  ```
  OBSERVED systemd-run --user -p MemoryOOMGroup=yes ->
  Unknown assignment: MemoryOOMGroup=yes
  ```

  (refused by the systemd 259 *user* manager, with or without Delegate=yes)

  ```
  /usr/bin/sh: 1: cannot create /sys/fs/cgroup/user.slice/.../leafog/memory.oom.group: Permission denied
  OG-WRITE-DENIED
  ```

  (delegated scope: mkdir works, every controller write EACCES — this host's
  kernel denies knob writes on user-delegated cgroups even with uid-owned
  files; enabling `+memory` in `cgroup.subtree_control` also fails). Docker
  exposes no equivalent, and host cgroupfs is out of bounds by probe safety
  rules.
- **Verdict**: open/standard/guarded = **off**; hostile = opt-in only.
  Under the experience-budget rule this knob is **blocked from default-on
  until measured on a host where the user manager accepts the property or a
  root unit can set it** (unblock path: run `probe-d-oomgroup.sh` on a bunker
  agent host with a permissive kernel).

### IOWeight (new; measured INERT on this host's NVMe)

- **Axis / mechanism**: proportional-share weight (`io.weight`; systemd
  `IOWeight=`; docker `--blkio-weight`). Shapes the split only under
  contention.
- **Probe**: `probes/knob-safety/probe-e-iobounds.sh` (executed).
- **Measured** (two concurrent 1.2GiB direct-I/O writers, weights 900 vs
  100, zero idle host):

  ```
  1258291200 bytes (1.3 GB) copied, 3.52283 s, 357 MB/s   (weight 900)
  1258291200 bytes (1.3 GB) copied, 3.55809 s, 354 MB/s   (weight 100)
  OBSERVED weight-900 io.stat: 259:0 ... wbytes=1258291200 wios=9600
  OBSERVED weight-100 io.stat: 259:0 ... wbytes=1258291200 wios=9600
  OBSERVED pair wall=3.6s
  ```

  Identical throughput: **on an uncontended NVMe the weight shapes
  nothing**. (Also measured: docker maps `--blkio-weight 150` to
  `io.weight default 1415` — the mapping exists but had no effect without
  contention.)
- **Verdict**: do NOT ship as a tier knob on NVMe hosts. Opt-in only, for
  spinning-disk/shared-bus hosts where contention exists (UNMEASURED there).
  GAP-119 should prefer bandwidth bounds (next row) which measured effective.

### IO bandwidth bounds (new; measured effective and exact)

- **Axis / mechanism**: hard bytes/s ceiling per device (cgroup v2 `io.max`
  wbps; systemd `IOWriteBandwidthMax=`; docker `--device-write-bps
  /dev/WHOLEDISK:<bps>` — partition names are rejected, see corrections).
- **Probe**: `probes/knob-safety/probe-e-iobounds.sh` (executed).
- **Measured** (1.2GiB `dd oflag=direct` writes):

  ```
  e-unbounded : 320 MB/s   wall=4.6s
  e-wbps-50MiBs: 52.6 MB/s wall=24.5s
  e-wbps-20MiBs: 21.0 MB/s wall=60.4s
  in-container io.max: 259:0 rbps=max wbps=52428800 riops=max wiops=max
  ```

  The cap is enforced exactly (52.6MB/s against a 50MiB/s cap). Cost is a
  PROPORTIONAL slowdown of the bounded workload — no failure, no kill.
  Host protection: the unbounded run's IO pressure spiked to
  `host io-PSI-full-max-avg10=7.94%` in-run; a bounded hog structurally
  cannot push host IO wait like that.
- **Verdict**: guarded = 100MiB/s, hostile = 20-50MiB/s (default-on — the
  slowdown IS the containment and matches the tier's threat model);
  standard = opt-in; open = off.

### TasksMax (existing knob; measured failure mode + trip point)

- **Axis / mechanism**: cap on tasks in the cgroup (cgroup v2 `pids.max`;
  systemd `TasksMax=`; docker `--pids-limit`). Excess `fork()` gets EAGAIN —
  the workload survives with a visible error; nothing is killed.
- **Probe**: `probes/knob-safety/probe-f-pids.sh` (executed this session).
- **Measured failure signature** (limit=16, forker targets 40):

  ```
  FORKER-EAGAIN after=15 children err=[Errno 11] Resource temporarily unavailable
  OBSERVED stage1 container rc=0 (0 = the forker SURVIVED the rejection)
  ```

  (dmesg is restricted on this host, so the kernel's
  "cgroup: fork rejected by pids controller" line is not readable; errno 11
  plus `pids.current`/`pids.max` carry the evidence.)
- **Measured sweep** (forker targets 200 tasks under limits 128..16):

  ```
  limit=128 -> forked=127 stopped_by=11 ... pids.max=128 pids.current=128
  limit=64  -> forked=63  stopped_by=11 ... pids.max=64  pids.current=64
  limit=48  -> forked=47  stopped_by=11 ... pids.max=48  pids.current=48
  limit=40  -> forked=39  stopped_by=11 ... pids.max=40  pids.current=40
  limit=32  -> forked=31  stopped_by=11 ... pids.max=32  pids.current=32
  limit=24  -> forked=23  stopped_by=11 ... pids.max=24  pids.current=24
  limit=16  -> forked=15  stopped_by=11 ... pids.max=16  pids.current=16
  ```

  Enforcement is EXACT: at limit L exactly L-1 children fork (the forker
  itself is the Lth task), `pids.current == pids.max`, and the stop is a
  clean EAGAIN. A compose-like stack of 6 services x 4 workers (~25 tasks
  with supervisor) therefore trips any TasksMax <= ~26; the shipped default
  4096 leaves ~160x headroom over that stack.
- **Measured abuse containment**: a 4095-fork theoretical fork bomb under
  `--pids-limit=256` filled the cgroup to exactly 256 tasks and stopped
  (host pid count unchanged before/after) — see abuse section.
- **Verdict**: open = 16384; standard = 4096 (today's default; measured
  safe); guarded = 1024; hostile = 256 (measured: still absorbs a full fork
  bomb while leaving room for a ~30-task stack).

### LimitFSIZE (existing knob; the crash-loop class — reproduced live)

- **Axis / mechanism**: per-FILE size cap (`RLIMIT_FSIZE`; systemd
  `LimitFSIZE=`; docker `--ulimit fsize=<cap>`). Writes past the cap on ANY
  single file get EFBIG and the process is killed with SIGXFSZ. **It is a
  per-FILE cap, NOT a usage quota** — measured directly:

  ```
  file-a ok: 1610612736 bytes
  file-b ok: 1610612736 bytes
  TOTAL written: 3GiB > 2GiB per-file cap — no error
  OBSERVED stage4 exit=0 (0 = per-FILE cap proven)
  OBSERVED du of both files: 3.1G
  ```

- **Probe**: `probes/knob-safety/probe-limitfsize.sh` (executed this
  session; all four stages below are its output).
- **Measured crash signature** (2GiB cap; python writer and C writer):

  ```
  OSError: [Errno 27] File too large                    <- ftruncate past cap dies (EFBIG)
  File size limit exceeded (core dumped)                <- SIGXFSZ, C-level writer
  dd-exit=153                                           <- 128+25 = SIGXFSZ
  OBSERVED file size after crash: 0 bytes
  OBSERVED real blocks on disk: 0 (512B blocks)
  ```

- **Measured crash loop + empty-config-dir outcome** (synthetic s6-style
  supervisor restarting the failing app):

  ```
  supervisor: starting app (attempt 1) ... app exited rc=1 — restarting in 1s
  ... (attempts 2, 3, 4, 5 identical) ...
  supervisor: giving up after 5 attempts (crash loop)
  OBSERVED config-dir contents after crash loop: data.bin:0B — zero usable bytes = blank app (linuxserver signature)
  ```

  This is the linuxserver-.NET failure: the app ftruncates a 2TiB sparse
  file before first write, dies at the ftruncate (EFBIG), the supervisor
  restarts it, the loop repeats, and the config dir stays EMPTY — the user
  sees a blank app with no visible disk-usage cause (usage never exceeded
  anything; the per-file reservation did).
- **Measured control** (`fsize=-1`): same workload writes past the cap
  point with exit 0 — the knob, not the workload, is the cause.
- **Verdict**: hostile = tight cap ON (breakage is the intended containment;
  hostile-tier workloads are assumed adversarial). guarded = ON only with an
  app audit (any sparse-reserving app must be exempted or capped above its
  reservation). standard = keep today's 20GiB (`DefaultDiskBytes`) — high
  enough that legitimate big files pass, low enough to stop runaway logs
  inside one file. open = generous (100GiB) or off. NEVER lower a tier's cap
  without auditing the images that tier runs for ftruncate/sparse patterns.

### LimitMEMLOCK (new; the memlock class — reproduced live)

- **Axis / mechanism**: cap on mlock-able bytes (`RLIMIT_MEMLOCK`; systemd
  `LimitMEMLOCK=`; docker `--ulimit memlock=<cap>`). Overshooting the cap
  makes `mlock`/`mlockall` fail and the workload aborts.
- **Probe**: `probes/knob-safety/probe-memlock.sh` (executed this session).
- **Measured** (static C helper, docker `--ulimit memlock`):

  ```
  host RLIMIT_MEMLOCK (soft hard): 8192 8192                  <- 8MiB host default
  memlock probe: mlockall failed: Cannot allocate memory      <- 64KiB cap -> abort (ENOMEM)
  OBSERVED ml-tiny-64k-mlockall exit=1
  memlock probe: mlock(8MiB) failed: Cannot allocate memory   <- 64KiB cap, 8MiB lock
  OBSERVED ml-tiny-64k-mlock8m exit=1
  memlock probe: mlock(8MiB) OK                               <- 64MiB cap
  OBSERVED ml-default-64m-mlock8m exit=0
  memlock probe: mlockall OK                                  <- unlimited
  OBSERVED ml-unlimited-mlockall exit=0
  OBSERVED ulimit memlock=65536 -> in-container "ulimit -l" reports: 64
  OBSERVED ulimit memlock=-1   -> in-container "ulimit -l" reports: unlimited
  ```

  (Correction to the brief's errno expectation: the failure is ENOMEM
  "Cannot allocate memory", not EPERM — see corrections section. The abort
  outcome is identical.) This is why `docker compose up` dies on rootless
  docker: its tiny default memlock makes a workload that mlocks abort at
  startup.
- **Verdict**: standard/guarded = 64MiB (docker's default — measured no UX
  cost for CLI/compile workloads, unblocks the common `mlock` case).
  open = host default. hostile = 64MiB with **opt-in unlimited only for
  rootless-docker-in-agent workloads** (measured: `mlockall` needs
  unlimited). Never default unlimited — it removes the kernel's guard on
  unswappable memory.

### ProtectSystem (the read-only-filesystem class — mechanism reproduced live, scope measured)

- **Axis / mechanism**: makes the whole file hierarchy read-only (EROFS)
  except `/dev,/proc,/sys`; `ReadWritePaths=` re-opens paths. At the spawn
  level on ROOT units it also breaks `useradd` locking `/etc/passwd` with a
  misleading lock-contention error (documented history from the bunker
  bootstrap work; the root-unit reproduction itself is UNMEASURED here — no
  root units in this session, refusal captured below).
- **Probe**: `probes/knob-safety/probe-protectsystem.sh` (executed this
  session).
- **Measured part 1 — user-level units: the knob does NOTHING here.**

  ```
  etc-strict:   /usr/bin/sh: 1: cannot create /etc/gap114-ps-write: Permission denied
  etc-control:  /usr/bin/sh: 1: cannot create /etc/gap114-ps-write: Permission denied
  tmp-strict:   WRITE-OK-tmp-UNDER-STRICT
  ```

  Identical EACCES with and without the knob (plain uid ownership, no EROFS
  anywhere), and /tmp stayed writable under strict. Conclusion: for
  user-level agents, ProtectSystem buys nothing and must not be counted as a
  control.
- **Measured part 2 — container level: the EROFS mechanism reproduced**
  (`docker --read-only` + `--tmpfs`, the same read-only-bind-mount
  mechanism ProtectSystem uses):

  ```
  sh: 1: cannot create /etc/gap114-ro-write: Read-only file system   <- EROFS
  WRITE-FAILED-etc: 2
  WRITE-OK-tmp-exempt                                                <- --tmpfs /tmp:rw exemption works
  ```

- **Measured part 3 — root-unit useradd reproduction: UNMEASURED.**

  ```
  Failed to start transient service unit: Access denied as the requested operation requires interactive authentication...
  ```

  (no root units available unprivileged). The documented history stands
  cited but unverified this session; re-run on a root-capable host.
- **Verdict**: standard/guarded/hostile = apply at the CONTAINER/hosted
  runtime layer only (measured effective there; `/etc` EROFS, `--tmpfs`
  exemptions work); never claim it from a user-unit knob list. open = off.

### Unit-sandbox set (new; one measured classification per knob)

Probe: `probes/knob-safety/probe-g-sandbox.sh` (executed this session).
Host facts shaping these results: unprivileged userns is blocked host-wide
(`kernel.apparmor_restrict_unprivileged_userns=1`), so
RestrictNamespaces has nothing left to take away HERE.

| knob | measured this session | classification for rootless-docker-in-agent | tier verdict (o/s/g/h) |
|---|---|---|---|
| NoNewPrivileges | `NoNewPrivs: 1` under the knob, `0` in control; workload unaffected (`MKDIR-OK`) | SAFE | on / on / on / on (default-on candidate) |
| SystemCallFilter deny-list (`~mkdir`) | `MKDIR-BROKEN:159` — SIGSYS killed the offender (159 = 128+31); docker-side same deny = clean EPERM | BREAKS (and dies invisibly under systemd) | off / off / opt-in / opt-in — never default |
| SystemCallFilter allow-list (`@system-service`) | `NoNewPrivs: 1, Seccomp: 2, Seccomp_filters: 3`, `ALLOWLIST-EXEC-OK`, `MKDIR-OK` — CLI-grade workloads survive | NEEDS-EXCEPTION (units needing io_uring/ exotic syscalls need additions) | off / off / on (audited) / on (audited) |
| RestrictNamespaces | knob ACCEPTED by user manager; differentiation UNMEASURED-here (host already blocks unprivileged userns at baseline) | NEEDS-EXCEPTION for rootless-docker (it requires userns) | off / off / off / opt-in |
| ProtectKernelTunables | ACCEPTED by user manager; deep effect NOT verifiable at user level (UNMEASURED effect) | LOW-RISK-expected, UNMEASURED effect | off / off / on-after-remeasure / on |
| ProtectKernelModules | ACCEPTED; same UNMEASURED-effect caveat | LOW-RISK-expected, UNMEASURED | off / off / on-after-remeasure / on |
| ProtectControlGroups | ACCEPTED; same caveat; NOTE: agent needs cgroup writes for its own delegation — audit before enabling | LOW-RISK-expected, UNMEASURED; possible agent conflict | off / off / on-after-remeasure / on |
| RestrictSUIDSGID | ACCEPTED; same caveat (blocks setuid-binaries; agent images rarely need them) | LOW-RISK-expected, UNMEASURED | off / off / on-after-remeasure / on |
| CapabilityBoundingSet (drop all) | ACCEPTED; same caveat (user units have no caps to drop anyway — likely a no-op at user level; effective in root units/containers) | LOW-RISK-expected, UNMEASURED at effect level | off / off / on-after-remeasure / on |

Measured g5 acceptance lines (verbatim):

```
OBSERVED ProtectKernelTunables=yes    -> ACCEPTED (user manager took the property; deep effect NOT verified at this level)
OBSERVED ProtectKernelModules=yes     -> ACCEPTED (...)
OBSERVED ProtectControlGroups=yes     -> ACCEPTED (...)
OBSERVED RestrictSUIDSGID=yes         -> ACCEPTED (...)
OBSERVED CapabilityBoundingSet=       -> ACCEPTED (...)
```

Measured g6 docker rootless-style posture (no-new-privileges + cap-drop ALL
+ seccomp deny mkdir/unshare/mount), with control:

```
deny profile : mkdir: cannot create directory '/tmp/x': Operation not permitted / MKDIR-BROKEN:1 ; UNSHARE-BROKEN:1
control      : MKDIR-OK ; UNSHARE-BROKEN:1 (userns blocked by host, unrelated to seccomp)
```

The docker-side legibility contrast (clean EPERM vs systemd SIGSYS) is in
the corrections section.

---

## Abuse-case column (one measured run per abuse class)

Probe: `probes/knob-safety/probe-abuse.sh` (executed this session).

- **Fork bomb (pids)** — exponential forker (2 children/generation, depth
  12, theoretical 4095 forks) under `--pids-limit=256`:

  ```
  BOMB-DONE theoretical=4095 actual_forks=2 pids.max=256 pids.current=256
  OBSERVED host pid-count before=1015 after=1050 (host unaffected; container removed)
  ```

  Contained: the cgroup pinned at exactly its 256-task cap (the bomb's
  counter only sees its own 2 direct forks — the cap did the counting);
  host pid population unchanged (±35 is normal churn).
- **Memory bomb (alloc loop)** — attacker targets 8GiB under
  `--memory=1g --memory-swap=1g`:

  ```
  OBSERVED bomb container rc=137 oomkilled=true
  OBSERVED host MemAvailable before=50836448kB after=50958292kB (delta=118MiB)
  ```

  Contained at the cap by kernel OOM-kill; host memory untouched.
- **IO hog (dd)** — measured in `probe-e-iobounds.sh` and cross-referenced:
  unbounded hog ran 320MB/s and pushed host IO pressure to 7.94% avg10
  in-run; the 20MiB/s cap held the same hog to 21.0MB/s. The cap is the
  containment.

---

## Explicit default-on candidates (NO measured UX cost)

Measured-zero-cost knobs, safe to default per tier:

1. **NoNewPrivileges** — engaged (`NoNewPrivs: 1`) with zero workload effect.
   All tiers.
2. **MemoryHigh as a cushion** (e.g. 90% of MemoryMax) — measured graceful
   (throttle + swap spill, no kill); cost only materializes when the
   workload would otherwise have OOM'd. standard/guarded/hostile.
3. **TasksMax at a generous multiple** — measured failure mode is a clean
   EAGAIN the workload survives; 4096 default has ~160x headroom over a
   realistic multi-process stack. All tiers (value shrinks with tier).
4. **LimitFSIZE at the generous 20GiB default** — measured no effect on
   anything under the cap (a 3GiB total/2-file workload sailed through);
   breakage only hits apps exceeding a single file's cap. standard keeps
   20GiB.
5. **LimitMEMLOCK 64MiB** — measured zero cost for CLI/compile workloads,
   unblocks the common `mlock` case. standard/guarded.
6. **IO bandwidth caps for guarded/hostile only** — the "cost" is the
   intended containment (proportional slowdown of the bounded workload);
   included here because it has no cost for *well-behaved* workloads that
   stay under the cap.

## Knobs that MUST stay opt-in (measured or budget-blocked from default-on)

1. **SystemCallFilter deny-lists** — measured SIGSYS kill (exit 159) under
   systemd: the workload dies without a legible error. Opt-in only, and
   prefer docker-seccomp shape (clean EPERM) when possible.
2. **MemoryOOMGroup** — UNMEASURED-here: user manager refuses the property;
   delegated cgroup writes denied; docker has no equivalent (all three
   refusals captured). Blocked from default-on until measured on a capable
   host.
3. **IOWeight** — measured INERT on uncontended NVMe (900 vs 100 weights,
   identical throughput). No measured benefit; opt-in for contended hosts.
4. **RestrictNamespaces** — breaks rootless-docker by design (needs userns);
   differentiation unmeasurable on this host (userns already blocked).
   NEEDS-EXCEPTION; opt-in.
5. **ProtectSystem from user units** — measured silent no-op at user level.
   Ship it only at the container/hosted layer; never in a user-unit knob
   list where it would advertise protection it does not deliver.
6. **LimitMEMLOCK unlimited** — measured required only for `mlockall`
   (rootless-docker); defaulting it removes the unswappable-memory guard.
7. **ProtectKernelTunables / ProtectKernelModules / ProtectControlGroups /
   RestrictSUIDSGID / CapabilityBoundingSet** — ACCEPTED by the user manager
   but effect-level UNMEASURED there; ship as default only after effect
   measurement on root/hosted units (bunker battery), and audit
   ProtectControlGroups against the agent's own cgroup delegation needs.
8. **MemorySwapMax=0 for the open tier** — measured: it converts a
   would-have-completed run (17MiB overshoot absorbed by swap, exit 0) into
   a hard OOM. Bar-swap belongs to standard and above.

## Tier matrix (per-knob verdicts at a glance)

Verdicts per tier — open / standard / guarded / hostile. "today" marks the
GAP-117 shipped standard values. Cells marked `remeasure` carry an
UNMEASURED-here caveat from the sections above.

| knob | open | standard | guarded | hostile |
|---|---|---|---|---|
| MemoryMax | no limit | 4GiB (today) | 2GiB | 1GiB |
| MemoryHigh (new) | off | 90% of Max | 75% of Max | 50% of Max |
| MemorySwapMax (new) | host default | 0 (bar) | 0 (bar) | 0 (bar) |
| MemoryOOMGroup (new) | off | off (UNMEASURED) | off (UNMEASURED) | opt-in (UNMEASURED) |
| IOWeight (new) | off | off (inert on NVMe) | opt-in (remeasure under contention) | opt-in |
| IO write bandwidth (new) | off | opt-in | 100MiB/s | 20-50MiB/s |
| TasksMax | 16384 | 4096 (today) | 1024 | 256 |
| LimitFSIZE | 100GiB or off | 20GiB (today) | 20GiB, app-audited | 5GiB, app-audited |
| LimitMEMLOCK | host default | 64MiB | 64MiB | 64MiB + opt-in unlimited for rootless |
| ProtectSystem | container-layer off | on at container layer only | on | on |
| NoNewPrivileges | on | on | on | on |
| SystemCallFilter | off | off | @system-service audited, opt-in | @system-service audited, opt-in |
| RestrictNamespaces | off | off | off (rootless exception) | opt-in |
| ProtectKernelTunables/Modules | off | off | on (remeasure) | on (remeasure) |
| ProtectControlGroups | off | off | on (remeasure + audit vs agent delegation) | on (remeasure) |
| RestrictSUIDSGID | off | off | on (remeasure) | on (remeasure) |
| CapabilityBoundingSet | off | off | drop-all (remeasure; no-op at user level) | drop-all (remeasure) |
| CPUQuota (existing; not re-measured this session) | — | 2.0 (today) | — | — |
| LimitNOFILE (existing; not re-measured this session) | — | 65536 (today) | — | — |

## Findings for GAP-113 (no code changes made — evidence task)

This task is evidence-only; zero product-code changes were made or are
required by the measurements. Findings to resolve in GAP-113/119:

1. **Tier-name reconciliation**: GAP-117's code vocabulary is
   `open/standard/hardened` (`SafetyPresetHardened` in
   `internal/config/config.go:59`); this matrix and the GAP-114 brief use
   `open/standard/guarded/hostile`. GAP-113 must pick one vocabulary before
   shipping differentiated knob sets, or map them explicitly.
2. **MemorySwapMax=0 is a standard-and-above knob, never open** (measured
   OOM-vs-complete flip at exactly-peak workloads).
3. **IOWriteBandwidthMax over IOWeight for tier enforcement**: bandwidth
   measured effective and exact; weight measured inert on NVMe. Also:
   resolve the whole-disk device (io.max rejects partitions — measured
   ENODEV on `nvme0n1p2`).
4. **ProtectSystem belongs in the container-layer knob set**, not the
   user-unit set (measured user-level no-op).
5. **MemoryOOMGroup needs a capable-host measurement run** before any tier
   defaults it on; `probe-d-oomgroup.sh` is the ready-made re-run script
   (its mechanism-A section records the refusals loudly on this host).
6. **User-level sandbox knobs (ProtectKernel*, RestrictSUIDSGID,
   CapabilityBoundingSet) need an effect-level battery on a root/hosted
   unit** before guarded/hostile default them on; acceptance is measured,
   effect is not.

## UNMEASURED register (exact reasons; scripts ship re-runnable)

| knob/case | script | exact reason | unblock path |
|---|---|---|---|
| memory.oom.group mechanism | probe-d-oomgroup.sh | user manager refuses `MemoryOOMGroup=` ("Unknown assignment"); delegated-scope controller writes EACCES (kernel denies knob writes on user cgroups, uid-owned files included); docker exposes no equivalent; host cgroupfs out of probe bounds | run on a host whose user manager accepts the property, or a root unit |
| RestrictNamespaces differentiation | probe-g-sandbox.sh | host blocks unprivileged userns at baseline (`apparmor_restrict_unprivileged_userns=1`) — the knob has nothing measurable to take away here | run on a host permitting unprivileged userns |
| ProtectKernelTunables/Modules/ControlGroups, RestrictSUIDSGID, CapabilityBoundingSet (effect level) | probe-g-sandbox.sh (g5) | user manager ACCEPTS the properties but their namespace/privilege effects are not observable from a user unit on this host | effect battery on root units / bunker agents |
| ProtectSystem root-unit useradd lock breakage | probe-protectsystem.sh (part3) | no root units creatable unprivileged ("requires interactive authentication" — refusal captured) | root-capable host, `systemd-run --system` |
| IOWeight shaping under contention | probe-e-iobounds.sh | this host's NVMe showed zero contention during the window (baseline PSI ~0.7-3.75 avg10 is background noise) | spinning-disk or deliberately contended host |
