# Rootless-install delivery: what the GAP-091 cache covers, what it cannot, and the PERF-023 marks (PERF-023)

Status: decision record. The attribution marks shipped in this change; the
byte-level coverage of the big payload is deliberately NOT shipped, and this
note names the mechanism and the reason so the next change is judged against a
fixed baseline.

## 0. The one-screen answer

The GAP-091 host cache covers ONLY the pinned rootless installer SCRIPT
(~9KB). The ~82MB a rootless install actually moves — `docker.tgz` and
`rootless.tgz` from download.docker.com — is fetched by the pinned script
ITSELF, inside the installer process, into a temp directory the script deletes
when it exits. Those bytes cannot be covered cleanly from the daemon without
either breaking the rendered artifact's supply-chain pin or shimming the
installer's own tooling; both are rejected here. What the daemon CAN do — and
now does — is attribute everything measurable per spawn: a delivery mark
(bytes + elapsed_ms + source) for the script, and an execution mark whose
elapsed time bounds the internal tarball fetch.

## 1. The problem (measured)

bunker-las-03, 2026-11-22, three sequential spawns on the same daemon: the
rootless-install stage cost 23.3s / 33.8s / 41.4s. Each spawn creates a NEW
agent user with a fresh home, so `installRootlessDocker` runs the installer
every time (the `dockerd-rootless.sh` presence check only short-circuits a
RE-spawn of the same agent). The variance is the download.

The spawn budget already reserves for this: `SpawnRootlessInstallBudget`
(internal/config/spawn_budget.go) allows 5 minutes for the ~93MB download,
unpack and setup on a cold, loaded host.

## 2. What the cache covers

`downloadRootlessInstallerCached` (internal/agent/rootless.go, GAP-091) caches
the output of `curl -fsSL <rootlessInstallURL>` — the pinned
rootless-install.sh template — under
`/var/cache/bunker/rootless-installer/` (config
`agent.rootless_installer_cache_dir`, env
`BUNKER_ROOTLESS_INSTALLER_CACHE_DIR`; explicit "" opts out). The cache key
carries the URL and the supply-chain pin's digest prefix, so a re-pin is a
miss by construction (GAP-135). On bunker-las-03 this file is ~9KB. A warm
spawn re-downloads NONE of it: the mark reads `source=cache`.

## 3. What the cache does NOT cover, and why

The installer, when it runs, executes (pinned script, `do_install`):

```sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
cd "$tmp"
curl -L -o docker.tgz   "$STATIC_RELEASE_URL"            # ~78MB
curl -L -o rootless.tgz "$STATIC_RELEASE_ROOTLESS_URL"  # ~15MB
tar zxf ... --strip-components=1   # into $HOME/bin
exec_setuptool
```

Unconditionally, every install — there is no skip-if-present check, and the
tarballs are deleted by the trap. The URLs are deterministic from the pin
(`STABLE_LATEST` is one of the three substituted placeholders, so the
tarball versions are pinned by construction), but the fetch itself happens:

1. as the agent user, in a session shell the daemon does not own;
2. into a private mktemp directory created after the Go path hands over;
3. through the script's own `curl`, which the daemon cannot redirect without
   changing the script.

Why each coverage mechanism was rejected:

- **Rewriting the URLs (file:// or a local mirror):** the rendered installer
  must keep verifying against the `ArtifactRootlessInstaller` pin. The
  template's only substitutable placeholders are the three `*_LATEST` version
  strings; any other edit makes the supply-chain gate (GAP-135) refuse the
  artifact, and rightly so — that gate is what proves the bytes the agent user
  executes are the pinned ones.
- **Pre-staging the agent home:** the script ignores anything already in
  `$HOME/bin` — it re-downloads into its own mktemp regardless. Pre-staging
  changes nothing.
- **PATH/curl shim or a caching proxy:** the fetch is plain HTTPS, so a proxy
  must TLS-intercept download.docker.com for the agent user (a CA in the
  agent's trust store is an injected security surface), or a curl shim must
  sit earlier on the installer's PATH (the same, plus a race with the
  prerequisites' own curl usage). Both put new machinery between the pinned
  artifact and its tooling to save ~30s — the wrong trade for a security
  boundary this repo treats as load-bearing.

The clean path that WOULD cover it (for the record): fork the install flow —
download the two tarballs in Go into a pin-keyed host cache (their digests are
derivable from the pin, same shape as the GAP-135 artifact table), extract
into `$HOME/bin` in Go, then run `dockerd-rootless-setuptool.sh install`
directly instead of the full script. That bypasses the pinned script's
`checks` (rootlesskit requirements, iptables, subuid hints) and moves the
trust story from "the pinned script did it" to "our code did it", so it needs
its own design and supply-chain review. It is a spawn-latency feature, not a
bug fix; it is not forced here.

## 4. What IS attributable now (PERF-023 marks)

Both marks follow the PERF-022 component-mark format (`elapsed_ms` integer
fields), so the spawn's phase table stays uniform:

- `rootless installer delivered` — `user`, `bytes` (the delivered script's
  size on disk), `elapsed_ms`, `source` ∈ network|cache|invalidated.
  - `cache`: served from the armed host cache.
  - `network`: downloaded — first fetch, a re-pin (different cache key), or a
    cache-disabled host on the legacy seam.
  - `invalidated`: downloaded because an existing cache entry failed its
    validation and was removed (partial or poisoned artifact). A re-pin is a
    MISS, not an invalidation — the old pin's entry is simply never consulted.
- `rootless installer executed` — `user`, `elapsed_ms` (first attempt through
  any retry), `note` stating plainly that the wall time includes the
  installer-internal tarball fetch, which is not byte-attributable from the
  daemon. This mark is the attributable bound on the ~82MB: when a spawn's
  rootless-install stage jumps 23s→41s, the executed mark carries it.

Not attributable, stated honestly: the per-tarball byte counts and the split
between docker.tgz and rootless.tgz. If that split ever matters, the fork in
§3 is the prerequisite, not more logging.

## 5. Verification

Tests next to `rootless_cache_arming_test.go` (rootless_delivery_mark_test.go)
prove, with every seam faked and no live network: the mark on a miss (network,
bytes, nonzero elapsed), the warm hit in-process (cache, same byte count, zero
extra fetches), the invalidated path (download re-records the delivery, cache
re-populated, next call a hit), the stale-pin key (miss by construction,
marked network), the legacy uncached path (marked network, bytes taken from
the delivered file — the legacy seam's return value is curl's output, not the
payload), and the full install path emitting both marks in order.

Still needing a live host: a real warm-spawn measurement on bunker-las-03
showing the delivered mark at `source=cache` and the executed mark carrying
the (unchanged, expected) ~23-41s — the cache cannot shrink that number until
the fork in §3 is designed and approved.
