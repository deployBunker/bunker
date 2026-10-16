# SURF-009 — toolchain provisioning: live proof

**Task:** SURF-009 — *toolchain provisioning for agent build capability* (the
offload blocker: a fresh agent could not build anything but Python).
**Date:** 2026-10-16 (all timestamps UTC).
**Branch:** `wt/SURF-009` @ `374db471` + the change under test
(`docs/provisioning.md`, `internal/provisioning/*`, `internal/cli/spawn.go`,
`internal/cli/toolchain_check.go`).
**Driver:** `bunker` built from this worktree with a bare
`go build -o bunker-surf009 ./cmd/bunker` (19,997,475 bytes,
sha256 `d41b62d9…`). No `.go` file was newer than that binary when the proof
ran, so the binary under test is exactly the tree committed with this change.
(It is a dev build: its embedded version stamp is the bare-build VCS
fallback, `bunker 0.2.0 / commit bf9a707`, which does not identify a
revision — the build is identified by the tree and hash above.)
**Agent:** `surf009-goclass`, uid 1359 on **bunker-mvp** (`78.46.173.180`,
tailnet `100.115.67.116`), spawned fresh for this proof with a 2h TTL.

Every transcript below is the agent's or the CLI's own output, pasted
verbatim. The proof exercises the class API: `spawn --toolchains=go`,
`toolchain-check --class go|full`, `--json`, `--report-only`, `--deliver`.

---

## 1. What the change adds

A toolchain **class** is the "can this agent build me?" noun:

| class | requires |
|---|---|
| `minimal` | `python3` (the stock image; what a spawn without `--toolchains` is) |
| `go` | `python3`, `go`, `make` |
| `full` | `python3`, `go`, `gcc`, `make`, `node`, `cargo` |

`toolchain-check` reports the catalog **by name** and its exit code is the
gate (0 = every toolchain the class requires is present, 1 = any missing,
`--report-only` = always 0). `--deliver` installs the class's missing tools
into the agent's own `$HOME` with no root, and the verdict is a **re-probe**,
never the installer's word. Full documentation:
[`docs/provisioning.md`](../provisioning.md).

## 2. Fresh agent of the `go` class

```
$ bunker spawn --server bunker-mvp --agent-id surf009-goclass --ttl 2h --toolchains=go
Creating agent...
Agent created: surf009-goclass
  Expires:      2026-10-16T05:23:15Z
  SSHFS Mount:  sshfs … bunker-surf009-goclass@100.115.67.116:/home/bunker-surf009-goclass …
═ Use `bunker exec` to run commands in this agent ═
Provisioning build toolchains for class "go" (python3, go, make)...
bunker: every toolchain class "go" requires is already present; nothing to deliver
```

The `go` class needed no delivery **on this host**: bunker-mvp's fresh agents
already carry the stock userland layer (`golang-go`, `make`, `python3`, `gcc`
— see `internal/imagespec` `StockToolchainPackages`). That is exactly the kind
of thing the check is for: it reported what is *actually* there instead of
assuming. §4 and §5 provision/verify from that measured base, and §6 proves the
missing half (`node`, `cargo`) is really installed by the delivery path.

## 3. The check, by name (acceptance criterion 2)

```
$ bunker toolchain-check --server bunker-mvp surf009-goclass --class go
bunker: reading server "bunker-mvp"

Declared build toolchains (surf009-goclass, class: go — Go projects: go build / go test plus the make driver)

  TOOL     STATUS   REQ  VERSION / WHAT IT BUILDS
  python3  PRESENT  yes  Python 3.12.3
  go       PRESENT  yes  go version go1.22.2 linux/amd64
  gcc      PRESENT  no   gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0
  make     PRESENT  yes  GNU Make 4.3
  node     ABSENT   no   JS/TS builds (npm run build) and npm-installed toolchains
  cargo    ABSENT   no   Rust binaries (cargo build / cargo test)

  class go: every required toolchain is PRESENT (python3, go, make) — this agent can build Go projects
$ echo $?
0
```

One line per tool, `PRESENT`/`ABSENT` by name, and the class requirement
marked (`REQ`) so an absent tool the class does not need is visibly not a
failure.

The same agent against `full` is the **gate** (exit 1, missing names named):

```
$ bunker toolchain-check --server bunker-mvp surf009-goclass --class full; echo "exit=$?"
…
  class full: MISSING on the agent: cargo, node
  Deliver in place (no root):   bunker toolchain-check surf009-goclass --deliver --class full
  Or bake into the image at spawn (--image-spec directive):
    {"packages":[{"manager":"apt","packages":["gcc","g++","make"]}]}
bunker: toolchain class "full" incomplete on agent "surf009-goclass": missing cargo, node
exit=1

$ bunker toolchain-check --server bunker-mvp surf009-goclass --class full --report-only; echo "exit=$?"
… (same report) …
exit=0
```

`--json` carries the machine shape — class, the class's `required` list,
per-tool `status`/`version`, `missing`, `all_present`:

```json
{
  "agent": "surf009-goclass",
  "class": "go",
  "class_note": "Go projects: go build / go test plus the make driver",
  "set": "SURF-009 build toolchains",
  "required": ["python3", "go", "make"],
  "tools": [
    {"name": "python3", "status": "PRESENT", "version": "Python 3.12.3", "kind": "base-image", "required": true},
    {"name": "go",      "status": "PRESENT", "version": "go version go1.22.2 linux/amd64", "kind": "pinned-tarball", "required": true},
    {"name": "gcc",     "status": "PRESENT", "version": "gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0", "kind": "apt-deb-extract", "required": false},
    {"name": "make",    "status": "PRESENT", "version": "GNU Make 4.3", "kind": "apt-deb-extract", "required": true},
    {"name": "node",    "status": "ABSENT",  "version": "", "kind": "pinned-tarball", "required": false},
    {"name": "cargo",   "status": "ABSENT",  "version": "", "kind": "pinned-rustup", "required": false}
  ],
  "missing": [],
  "all_present": true,
  "exit_code": 0
}
```

## 4. A real Go project built AND tested on the agent (criteria 1 and 3)

The fixture is a real module — `main.go` importing `buildproof/internal/greeting`,
plus `main_test.go` — deployed with `bunker deploy` (SCP -r), so the source is
on the agent and the build has no local compute involved:

```
$ bunker deploy --server bunker-mvp --ssh-key ~/.config/bunker/keys/surf009-goclass \
    ./fixture surf009-goclass:/home/bunker-surf009-goclass/surf009-fixture
Deployed ./fixture to surf009-goclass:/home/bunker-surf009-goclass/surf009-fixture
```

Then, executed **on the agent** with `bunker exec --script`:

```
===WHO===
hostname: bunker-mvp
user:     bunker-surf009-goclass
uid:      1359
home:     /home/bunker-surf009-goclass
cwd:      /home/bunker-surf009-goclass
kernel:   Linux 6.8.0-142-generic x86_64
===PATH-PROBES===
go PRESENT /usr/bin/go
gcc PRESENT /usr/bin/gcc
make PRESENT /usr/bin/make
node ABSENT
cargo ABSENT
python3 PRESENT /usr/bin/python3
===VERSIONS===
go version go1.22.2 linux/amd64
GNU Make 4.3
Python 3.12.3
===FIXTURE===
total 24
drwxrwxr-x 3 bunker-surf009-goclass bunker-surf009-goclass 4096 Oct 16 03:24 .
drwxr-x--- 10 bunker-surf009-goclass bunker-surf009-goclass 4096 Oct 16 03:24 ..
-rw-r--r-- 1 bunker-surf009-goclass bunker-surf009-goclass   27 Oct 16 03:24 go.mod
drwxrwxr-x 3 bunker-surf009-goclass bunker-surf009-goclass 4096 Oct 16 03:24 internal
-rw-r--r-- 1 bunker-surf009-goclass bunker-surf009-goclass  311 Oct 16 03:24 main.go
-rw-r--r-- 1 bunker-surf009-goclass bunker-surf009-goclass  409 Oct 16 03:24 main_test.go
===BUILD===
build_rc=0
-rwxrwxr-x 1 bunker-surf009-goclass bunker-surf009-goclass 1911970 Oct 16 03:24 hello
===RUN===
hello, agent — built by SURF-009
run_rc=0
===TEST===
?   	buildproof/internal/greeting	[no test files]
=== RUN   TestGreeting
--- PASS: TestGreeting (0.00s)
PASS
ok  	buildproof	0.007s
test_rc=0
===GO-VET===
vet_rc=0
===END===
$ echo $?   # the exec itself
0
```

* The compiler ran **on the agent** (`hostname: bunker-mvp`, user
  `bunker-surf009-goclass`), producing a 1,911,970-byte ELF there.
* `go test` compiled and ran the module's own test binary on the agent
  (`ok buildproof 0.007s`), and `go vet` was clean.

## 5. Delivery actually installs (the absent half)

```
$ bunker toolchain-check --server bunker-mvp surf009-goclass --deliver --class full
bunker: reading server "bunker-mvp"
bunker: delivering node (pinned-tarball) into the agent's $HOME/bin
bunker: node delivered (v20.20.2)
bunker: delivering cargo (pinned-rustup) into the agent's $HOME/bin
bunker: cargo delivered (cargo 1.89.0 (c24e10642 2025-06-23))

Declared build toolchains (surf009-goclass, class: full — every build path SURF-009 measured absent: Go, C/C++, Make, JS/TS, Rust)

  TOOL     STATUS   REQ  VERSION / WHAT IT BUILDS
  python3  PRESENT  yes  Python 3.12.3
  go       PRESENT  yes  go version go1.22.2 linux/amd64
  gcc      PRESENT  yes  gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0
  make     PRESENT  yes  GNU Make 4.3
  node     PRESENT  yes  v20.20.2
  cargo    PRESENT  yes  cargo 1.89.0 (c24e10642 2025-06-23)

  class full: every required toolchain is PRESENT (python3, go, gcc, make, node, cargo) — this agent can build Go, C/C++, Make, JS/TS and Rust projects
```

Nothing was copied as a bare binary: `node` is the checksum-pinned
nodejs.org release tarball, `cargo` the pinned rustup installer
(`--profile minimal`), `gcc`/`make` come from the apt archive
(`apt-get download` + `dpkg-deb -x` into `$HOME`), and every stage caches
under `$HOME/.bunker/toolchains` — no root, nothing outside the agent's home.

Then the **delivered** tools were driven on the agent (each build's artifact
is *run*, so the verdict is a program's output, not a version string):

```
===WHO===
hostname: bunker-mvp
user:     bunker-surf009-goclass
===DELIVERED-VERSIONS===
v20.20.2
10.8.2
cargo 1.89.0 (c24e10642 2025-06-23)
rustc 1.89.0 (29483883e 2025-08-04)
gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0
GNU Make 4.3
===NODE-RUNS===
node ran: 42
node version: v20.20.2
node_rc=0
===C-BUILD-VIA-MAKE===
make_rc=0
C built by SURF-009
c_run_rc=0
c_test_rc=0
===RUST-BUILD-VIA-CARGO===
cargo_rc=0
Rust built by SURF-009
rust_run_rc=0
===END===
$ echo $?
0
```

## 6. The committed tree stayed clean (criterion 4)

`git status --porcelain` in the worktree, captured immediately before the
first live `exec` and again after the spawn, deploy, both exec proofs and the
full delivery — **byte-identical**:

```
 M README.md
 M internal/cli/spawn.go
 M internal/cli/toolchain_check.go
 M internal/cli/toolchain_check_test.go
 M internal/provisioning/probe.go
 M internal/provisioning/provisioning_test.go
 M internal/provisioning/toolchains.go
?? docs/provisioning.md
```

Those are the change set itself (seven modified files + the new
`docs/provisioning.md`; this proof doc is the only file written after the
capture, and it is documentation, not part of the proof run). The proof run
added and modified **nothing** in the repository. All fixtures live under
`/tmp/surf009-proof/` on the driver and in the agent's own `$HOME`; the agent
never wrote into the mount or the tree. At dispatch the worktree had **0**
dirty paths.

## 7. Acceptance criteria

| criterion | evidence |
|---|---|
| A fresh agent of the `go` class builds AND tests a real Go project remotely | §2 spawn of `surf009-goclass` with `--toolchains=go`; §4 `build_rc=0`, binary produced, `go test` `PASS`/`ok buildproof`, `vet_rc=0` |
| `toolchain-check` reports the set present/absent by NAME, one line per tool | §3: six named lines with `PRESENT`/`ABSENT`, `REQ` marker, JSON `required`/`missing`/`all_present` |
| The build ran on the agent (hostname confirms) | §4 `hostname: bunker-mvp`, `user: bunker-surf009-goclass`, uid 1359, artifact written in the agent's `$HOME` |
| The committed tree stayed clean throughout | §6: identical `git status` before/after the whole proof; 0 dirty paths at dispatch |
| `go test ./...` passes on the provisioned toolchain code | full-suite run recorded in the commit; `internal/provisioning` + `internal/cli` green |

## 8. Honest notes, deviations and findings

1. **The `go` class needed no delivery on bunker-mvp** (§2): that host's fresh
   agents already carry `python3`/`go`/`gcc`/`make` from the stock userland
   layer, so the row's "all absent" premise is **host-dependent** (it was
   measured elsewhere). The check reports what is really there, and §5 shows
   the delivery path doing real work where tools *are* missing.
2. **Exit-code contract.** The brief asks for exit 1 when a required tool is
   missing. That is now the **default** (the gate, §3), with `--report-only`
   preserving the "a finding is data, exit 0" contract that `bunker
   agent-tools` uses. This is a deliberate, documented divergence from the
   older convention for this new command; both are tested.
3. **`python3` is in the catalog** as a `base-image` entry with no delivery
   stage, so `--class minimal` is a real check (it verifies the interpreter)
   rather than a class that can never fail. An agent missing `python3` cannot
   have it delivered — the verdict names it honestly instead of inventing a
   install path.
4. **Status spelling** is `PRESENT`/`ABSENT` on both surfaces (human table and
   JSON `status`), per the brief; the probe's own wire lines stay lowercase
   internally.
5. **Image-path gap (named, not skipped).** The `full` class's apt half can be
   baked at spawn (`{"packages":[{"manager":"apt","packages":["gcc","g++","make"]}]}`);
   the image-spec managers have no directive for a **node** or **rustup**
   *runtime*, so those two can only be delivered in place today. Fix proposal:
   a vendor-tarball image-spec directive with a checksum (same shape as the
   apt/go managers) — a spec change in `specs/image-spec.md`, deliberately not
   smuggled into this task. Documented in `docs/provisioning.md`.
6. **Finding — `bunker exec --script` mangles single quotes.** The first
   version of the second proof script used
   `node -e 'console.log("node ran:", 6 * 7)'` inside the uploaded script and
   the agent's shell reported
   `/home/bunker-surf009-goclass/.bunker/exec-script.sh: 39: Syntax error:
   Unterminated quoted string`. Shipping the JS as a file (what §5 does) works.
   This is a defect of the `--script` upload/quoting path, not of the
   toolchain work; it is worth a row of its own.
