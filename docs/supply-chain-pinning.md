# Supply-chain pinning (GAP-135)

**Status:** implemented. Authority: `internal/supplychain/manifest.json` (the
pins) and `internal/supplychain/supplychain.go` (the gate). This document is the
re-pinning procedure: read §4 before changing any digest.

See also: [`specs/image-spec.md`](../specs/image-spec.md) (the image-spec
surface whose `FROM` line is digest-pinned),
[`../specs/configuration.md`](../specs/configuration.md) §11 (the host-level
installer cache), [`compliance.md`](compliance.md) (the security posture this
sits in), `SECURITY.md`.

---

## 1. The problem this closes

Three inputs to a spawn used to be fetched and then executed or built from
without any check that they were the thing anybody had reviewed:

| surface | how it was fetched | why that was not a version |
|---|---|---|
| rootless install script | `curl https://get.docker.com/rootless`, then executed as the agent user | the URL is a moving target — it is "whatever Docker publishes today" |
| `slirp4netns`, `fuse-overlayfs` | `https://github.com/<project>/releases/latest/download/<binary>`, installed to `/usr/local/bin` | `latest/download` is a redirect, not a version; nothing recorded which build a host got |
| base image for a customized agent image | `FROM docker.io/library/ubuntu:24.04` | a tag is a mutable pointer; two builds of one spec were not the same image |

The script runs as the agent user and writes executables into that user's
`~/bin`; the two binaries are installed on the agent's `PATH`; the base image is
the root of every container the agent runs. Each of them was a supply-chain
input with no record and no check.

## 2. What is pinned now

One manifest, checked into the repo and embedded into the binary
(`//go:embed manifest.json`, the same pattern as
[`internal/fleet/inventory.yaml`](../internal/fleet/inventory.yaml)), carries a
row per artifact and per image.

### Artifacts (version + sha256)

| name | version | arch | sha256 |
|---|---|---|---|
| `rootless-installer` | `ada7a567e59b3935bd601bb21e422da50e80e376` (docker-install commit) | any | `dd5596…340e` |
| `slirp4netns` | `v1.3.6` | `x86_64` | `411167…1d48` |
| `slirp4netns` | `v1.3.6` | `aarch64` | `045f85…5817` |
| `fuse-overlayfs` | `v1.18` | `x86_64` | `56b0ae…a1e8` |
| `fuse-overlayfs` | `v1.18` | `aarch64` | `82fed7…0133` |

Transitive coverage, stated honestly: the pinned **script bytes** fix both
`SCRIPT_COMMIT_SHA` (which revision of `docker/docker-install` produced them) and
`STABLE_LATEST` (the `docker-ce-rootless-extras` version the script then
fetches). That archive is what supplies `dockerd-rootless.sh`,
`dockerd-rootless-setuptool.sh` and `rootlesskit`, so verifying the script's
digest is how those three are held to a version. The script itself has no
version flag to pass, so the digest IS the pin.

### Images (digest)

| reference | digest |
|---|---|
| `docker.io/library/ubuntu:24.04` (default) | `sha256:534baea6…30eb55` |
| `docker.io/library/ubuntu:22.04` | `sha256:5ec03bb3…26486401` |
| `docker.io/library/debian:12` | `sha256:2c037a04…12f9e858` |
| `docker.io/library/debian:11` | `sha256:6f519a81…f87fabe9` |

Each digest is the **multi-arch manifest list** the tag resolved to at pin time,
so the same reference builds the same image on `amd64` and `arm64`.

## 3. Where the gate runs

| surface | code | behaviour on mismatch |
|---|---|---|
| installer download | `internal/agent/rootless.go`, `downloadRootlessInstaller` | removes the download, fails the spawn with `supplychain.ErrChecksumMismatch` |
| installer cache read | `internal/agent/rootless.go`, `downloadRootlessInstallerCached` | refuses the entry and names the cache file to remove — it does **not** silently refetch |
| prebuilt prereq binary | `internal/agent/rootless_prereqs.go`, `installArtifact` | removes the partial download, never installs it to `/usr/local/bin`, records `verification failed: …` in the prerequisite outcome |
| base image | `internal/imagespec/parse.go`, `pinnedBaseRef` | `Parse` refuses an allowed-but-unpinned base; the `FROM` line carries `name@sha256:<digest>`, and docker itself fails the pull if the digest does not match |

Two properties are deliberately structural rather than conventional:

- **The cache key contains the pin.** `rootlessInstallerCacheKey()` embeds the
  pinned digest prefix, so a re-pin cannot be served the artifact the previous
  pin downloaded — a stale entry is a different file name, not a hit.
- **The image build identity contains the pin.** `Spec.CacheKey()` hashes the
  pinned reference, not the tag, so re-pinning invalidates the per-agent image
  cache instead of silently reusing the image built from the old digest.

Failures are **named errors**, not warnings: `ErrChecksumMismatch` with
`*MismatchError` (expected vs actual digest, artifact, version, path) and
`ErrNotPinned` with `*NotPinnedError` (the artifact and the names that *are*
pinned). Both are `errors.Is`/`errors.As`-able. Nothing is executed, extracted
or installed after either one.

## 4. Upgrade path — how to re-pin

Re-pinning is a deliberate, reviewed edit: the manifest is the contract, and
nothing in the product re-resolves a moving URL at runtime.

1. **Resolve the new value.**

   For a GitHub release asset (choose the release, never `latest`):

   ```sh
   curl -fsSL -o /tmp/asset \
     https://github.com/rootless-containers/slirp4netns/releases/download/vX.Y.Z/slirp4netns-x86_64
   sha256sum /tmp/asset
   ```

   For a base image, the registry API returns the manifest-list digest for a
   tag:

   ```sh
   TOKEN=$(curl -fsSL 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/ubuntu:pull' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
   curl -fsSI -H "Authorization: Bearer $TOKEN" \
     -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json' \
     https://registry-1.docker.io/v2/library/ubuntu/manifests/24.04 | tr -d '\r' | grep -i docker-content-digest
   ```

2. **Edit `internal/supplychain/manifest.json`** — the artifact's `version` and
   the variant's `url` + `sha256`, or the image's `digest`. Change the tag and
   the digest together: a tag whose digest is stale is worse than no pin,
   because it looks pinned.

3. **Run the gates.** `go test ./internal/supplychain/ ./internal/imagespec/
   ./internal/agent/` and then the full suite. The tests that must move with a
   deliberate re-pin are: the supplychain lookups
   (`TestArtifactLookupsFailClosed`), the image pin gate
   (`TestEveryAllowedBaseIsDigestPinned`, `TestDockerfileFromIsDigestPinned`),
   and `TestEveryPrerequisiteArtifactIsPinned`. A pin edit that changes
   behaviour without failing at least one of them means the gate has a hole —
   fix the test, not the assertion.

4. **Verify against upstream.** The live probe re-resolves every pinned URL and
   compares the bytes. It is skipped by default (no test-time downloads):

   ```sh
   BUNKER_LIVE_PIN_PROBE=1 go test ./internal/supplychain/ -run TestLiveArtifactPins -v
   ```

   A failure here after a re-pin means the manifest and upstream disagree; a
   failure with an unchanged manifest means upstream moved and the pin is doing
   exactly its job.

5. **Update this document's tables** and the `CHANGELOG.md` entry.

## 5. What is deliberately NOT pinned (and why)

- **`releases/latest` semantics anywhere.** The manifest validator refuses a URL
  containing `/latest/`, so the class cannot come back by accident.
- **A moving script URL's *content*.** `get.docker.com/rootless` is still the
  download source, because Docker does not version the endpoint; the digest is
  what makes the bytes immutable. Consequence to be honest about: when Docker
  republishes the script, a host that has not re-pinned fails the install with
  `ErrChecksumMismatch` until an operator re-pins. That is the intended
  trade — a refused install is visible; a silently different installer is not.
- **The BuildKit/dockerd binaries themselves.** They arrive inside the pinned
  `docker-ce-rootless-extras` archive (see §2) rather than from a URL this repo
  fetches.
- **A host-root attacker.** The manifest, the binary and the cache are all
  root-owned. This control is about what the *network* and the *distribution
  channel* hand a fresh host, not about a compromised root account.
- **The Go toolchain tarball for `agent-tools` installs.** It is pinned by
  sha256 in `internal/cli/agenttools_packages.go` (`goBootstrapVersion` +
  `goBootstrapSHA256AMD64/ARM64`) — the same discipline, in the place that
  renders that shell stage rather than in this manifest.
