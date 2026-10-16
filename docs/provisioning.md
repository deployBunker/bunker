# Toolchain provisioning (SURF-009)

Bunker's compute-offload model only becomes real when the *agent* can build.
A mount gives the agent a native tree and your forge credentials; it does not
give it a compiler. SURF-009 measured a fresh agent and found
`go`, `gcc`, `make`, `node` and `cargo` **all absent** — `python3` only — so
"the bunker does the building" was untrue for every Go/Rust/C/JS project.

This page documents the declared toolchain **classes**, the one command that
checks them by name, and the two ways to provision them. The live proof is
[`docs/dogfood/2026-10-16-surf-009-toolchain-proof.md`](dogfood/2026-10-16-surf-009-toolchain-proof.md).

## Classes

A class is the answer to *"can this agent build me?"* as a noun:

| class | requires | what it builds |
|---|---|---|
| `minimal` | `python3` | Python only. The stock image; what a spawn without `--toolchains` is. |
| `go` | `python3`, `go`, `make` | Go projects (`go build` / `go test`). |
| `full` | `python3`, `go`, `gcc`, `make`, `node`, `cargo` | Go, C/C++, Make, JS/TS and Rust — every path SURF-009 measured absent. |

`full` is the default for `toolchain-check`: a check that defaulted to
`minimal` could never fail, and the question being asked is whether the agent
can build. Provisioning is opt-in, so a spawn that asks for nothing is
`minimal`.

The catalog the classes draw from:

| tool | delivered as | source (publisher-verified pin) |
|---|---|---|
| `python3` | *(base image)* | the stock image's own interpreter — no stage by construction |
| `go` | `pinned-tarball` | `dl.google.com/go` release tarball, sha256-pinned |
| `gcc` | `apt-deb-extract` | the release `gcc` package family from the apt archive, unpacked with `dpkg-deb -x` |
| `make` | `apt-deb-extract` | the apt `make` package (single-binary extract) |
| `node` | `pinned-tarball` | `nodejs.org` release tarball, `SHASUMS256.txt`-pinned |
| `cargo` | `pinned-rustup` | `rustup` installer from `static.rust-lang.org`, sha256-pinned, `--profile minimal` |

Nothing is delivered by copying a binary at runtime: every tool comes from
its distribution's own signature-verified archive (apt) or its publisher's
checksummed release artifact, exactly the division of labour in
`docs/prd/SPEC-agent-tool-delivery.md` — a tool a distribution ships is
installed from its registry, only tools we build ourselves are copied. Each
stage caches under `$HOME/.bunker/toolchains` and links into `$HOME/bin`, so
re-runs are no-ops and nothing lands outside the agent's own home (no root).

## Checking

```bash
bunker toolchain-check <agent-id>                 # class full (default)
bunker toolchain-check <agent-id> --class go      # a Go build box?
bunker toolchain-check <agent-id> --class go --json
bunker toolchain-check <agent-id> --report-only   # never fail on a finding
```

One line per tool, `PRESENT` or `ABSENT`, by name, with the class's
requirement marked. This is the live output from the proof run
(agent `surf009-goclass` on `bunker-mvp`):

```
Declared build toolchains (surf009-goclass, class: go — Go projects: go build / go test plus the make driver)

  TOOL     STATUS   REQ  VERSION / WHAT IT BUILDS
  python3  PRESENT  yes  Python 3.12.3
  go       PRESENT  yes  go version go1.22.2 linux/amd64
  gcc      PRESENT  no   gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0
  make     PRESENT  yes  GNU Make 4.3
  node     ABSENT   no   JS/TS builds (npm run build) and npm-installed toolchains
  cargo    ABSENT   no   Rust binaries (cargo build / cargo test)

  class go: every required toolchain is PRESENT (python3, go, make) — this agent can build Go projects
```

**Exit code is the gate.** `0` when every toolchain the class requires is
present; `1` when any is missing — the names are printed and carried in the
JSON `missing` array, so a driver can gate on it:

```bash
bunker toolchain-check "$AGENT" --class go && ./ship-the-build.sh
```

`--report-only` turns the same check into a pure report (always exit `0`) for
callers that want the answer as data. A probe that could not *run* exits
non-zero either way: a check that did not run is not a finding. An absent
tool outside the class is reported but is not a failure (`REQ no`).

The probe is one remote shell program executed over the same audited exec
path as `bunker exec` — no new RPC, no new credential, and it needs no
network access on the agent (it is `command -v` plus a version line).

## Provisioning

Two paths, per the ownership rule:

**In place, no root** — `--deliver` installs the class's missing toolchains
into the agent's own home and then **re-probes**: the verdict is the fresh
probe, never the installer's word.

```bash
bunker toolchain-check <agent-id> --deliver --class go
bunker spawn --toolchains=go ...        # same delivery, right after creation
bunker spawn --toolchains ...           # bare flag = class full
```

A failed delivery during `spawn` is a **warning**, not a spawn failure: the
agent is up and the delivery is resumable with
`bunker toolchain-check <agent-id> --deliver`.

**Baked into the image at spawn** — agents with the image-spec surface can
add the apt-served half to the image instead:

```bash
bunker spawn --image-spec '<the directive toolchain-check prints>'
# class full  → {"packages":[{"manager":"apt","packages":["gcc","g++","make"]}]}
# class minimal / go → {"packages":[]}  (the stock userland layer already
#                      carries python3, golang-go and make)
```

### Known gap on the image path

The image-spec managers are package managers (`apt`, `go install`, `npm`,
`pip`, `pipx`, `cargo install`, `gem`, `composer`). `apt` can express
`gcc`/`g++`/`make`, but nothing in that list installs a **node** or **rustup**
runtime, so those two halves of the `full` class cannot be baked through a
directive today — they are delivered in place by the pinned tarball/rustup
stages. Closing it means a new image-spec directive for a runtime toolchain
(vendor tarball + checksum), which is a spec change in its own right
(`specs/image-spec.md`) and is deliberately not smuggled into this work.
