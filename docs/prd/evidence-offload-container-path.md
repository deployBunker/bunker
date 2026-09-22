# Evidence: the offload works TODAY — via containers, not native toolchains

Measured 2026-09-22 on a **bare `bunker spawn`** (no `--image-spec`, the default path)
against the live daemon (`bunker 0.1.4`, server `bunker-las-02`). Every line below is
output from that run; the agent was destroyed afterwards.

This corrects the framing of the offload blocker: the box has **no native compilers**,
which is true, but that does **not** block offloading a build — a container supplies
whatever toolchain the project needs, and a real Go build was completed and run this way.

## 1. What is on a fresh box (bare spawn)

```
identity : bunker-offload-proof-<pid>
host     : bunker-las-02
cpus     : 6          mem: 31Gi          disk: 64 GB quota
go       ABSENT       gcc  ABSENT        cc     ABSENT
make     ABSENT       node ABSENT        cargo  ABSENT
rustc    ABSENT       rg   ABSENT
python3  /usr/bin/python3   (3.13.5)
git      /usr/bin/git       (2.47.3)
jq, curl present      docker present (in the agent's own $HOME/bin)
```

So: **no compiler for Go, C, Rust, Node — Python only.** That part of SURF-009 is real
and unchanged.

## 2. Interpreter offload works (the compute really runs remotely)

python3 on the box: `3.13.5 x86_64`, ran an 8,000,000-iteration loop in **1.02s**, on
`bunker-las-02`. The control machine did no work — this is the local-cost-per-project
argument holding up: the box does the computing, the local side only drives.

## 3. A container supplies a toolchain the box does not have

```
docker version --format ...   ->  client 29.8.1 / server 29.8.1
docker run --rm golang:1.26-alpine go version
   -> Digest: sha256:8ac98ca5...  (pulled)
   -> go version go1.26.8 linux/amd64
```

The box has a **working Docker daemon** (not just a client), so any toolchain is one
image pull away.

## 4. A REAL offloaded build: container compiles, the box runs the artifact

```
--- compiling inside golang:1.26-alpine ---
    build rc=0
--- running the artifact back on the box (no container) ---
  built+ran in a container on linux/amd64 (go1.26.8)
  fib(32)=2178309 computed in 12ms using 6 cpus
--- artifact on the box ---
-rwxr-xr-x 2422606 bytes  .../build2/app   (owned by the agent user)
```

A 2.4 MB Go binary was compiled from source that lives on the box, landed on the box,
and executed there on 6 CPUs. **The offload is not blocked.**

## What this changes

- **SURF-009 (unchanged in fact, reframed in consequence).** "A fresh agent has no
  build toolchain" remains true and worth fixing for native speed and for projects that
  cannot be containerised. But its inference — that "the bunker does the building" is
  not yet true — is **wrong**: it IS true today through the container path. The row
  should say the blocker is *native* toolchains, not the offload.
- **GAP-066 is the offload mechanism, not a convenience.** "Docker-as-installer
  execution mode" is currently P2/`pending`. On this evidence it is the thing that turns
  the model from aspirational into working, and it should be promoted accordingly: the
  capability exists, but only by hand-invoking `docker run` through `bunker exec`.
  Productising it means the toolchain question stops being a per-project blocker.
- **GAP-065 (container-mode lifecycle + storage/network)** gains weight for the same
  reason: once builds run in containers, the container's lifecycle and storage semantics
  are the thing a project feels.
- **SURF-010 (persistent project dev box)** is unaffected and still open — a box that
  can build today is still a box that expires by default.

## Honest caveats (measured, not assumed)

- The image pull consumed a real slice of the window; a cold cache is not free. Any
  "instant" claim for a container build must account for the pull.
- **This is not an isolation story.** The container runs as / under the agent user; it is
  a toolchain-delivery mechanism, not a security boundary.
- Artifacts land on the box as the agent user (2.4 MB here). The `.gitignore` rule in
  `coding-hermes-remote-bunker` still applies — a built artifact in a mounted tree is
  visible to `git status` on the control side.
- Nothing above is wired into the CLI. Every step was a manual `bunker exec` +
  `docker run`; that gap is exactly GAP-066.
