# Dogfood run 2026-09-25b — the TLS trust surface (TOFU pinning + the insecure knob)

**Run:** coding-hermes-dogfood cron, 19th run on this repo. Angle: the surfaces runs 1–18 never
touched — the README's "secure path is the easy path" promise (GAP-127 TOFU pinning, GAP-141
`tls_insecure` knob) and the daemon side of TLS. Prior runs covered CLI lifecycle, REST protocol,
fresh-machine install, TTL/registry durability, agent isolation/mount, key lifecycle, remote-dev
loop, agent-tools/stop-start, and the raw-REST credential surface (that same morning).

**Promise under test:** *"A self-signed daemon is the normal way to run Bunker on a host you own:
the CLI pins the certificate on first connect (TOFU), refuses a changed certificate loudly, never
silently skips verification, marks unverified sessions in the audit trail, and the REST surface
rejects unauthenticated POSTs with 401 (405 before auth)."*

**Method:** scratch `bunkerd` built from HEAD `a4e98ce` (`make build`, 8s), run as root from
`/tmp/df0925/bunkerd.yaml` exactly per the README's inline config example (self-signed TLS,
REST :18093 / gRPC :19093, private ssh_dir/registry/audit paths, port pool 28000-28999 — disjoint
from the fleet daemon's 30000-30999). Fleet daemon (`/opt/bunker/bunkerd`, 509fc42) untouched
throughout: NRestarts stayed 0. Nine-arm TLS battery (below), REST method matrix, audit-chain
verify, perf measurements, ephemeral-bunker install leg on las-bunker-03.

## The nine-arm TLS battery — results

| Arm | Command shape | Expected | Got | Verdict |
|---|---|---|---|---|
| A1 TOFU first connect | `connect --tls self-signed https://127.0.0.1:18093 --token <tok>` (fresh BUNKER_HOME) | banner, fingerprint printed, pin stored | fingerprint `9743b182…`, entry written with `tls_mode: self-signed` + `cert_pin` + `cert_pin_set_at` | ✅ |
| A2 wrong token | `connect … --token wrong` | auth error | `unauthenticated: invalid token`, exit 1 — **but the TOFU banner still printed "stored as cert_pin in ~/.bunker/config.yaml" while NOTHING was stored** (verified: empty BUNKER_HOME, real config untouched) | ⚠️ DF-BUNKER-64 |
| B1 cert change | re-keyed daemon (new cert dir), `status`/`list` | loud refusal naming both fingerprints + re-pin cmd | refusal names pinned `9743…` vs presented `b1698d…`, `--accept-cert` remedy, MITM warning. `list` exits 1; `status` exits **0** | ✅ msg / ⚠️ exit (DF-BUNKER-64) |
| B2 deliberate re-pin | `connect --tls self-signed --accept-cert` | pin replaced | pin now `b1698d…`, `cert_pin_set_at` updated | ✅ |
| C1 no-ack insecure | `connect --tls-insecure` (no env) | refusal | `tls_insecure requires an explicit acknowledgement`, exit 1 | ✅ |
| C2 ack'd insecure | `BUNKER_ALLOW_TLS_INSECURE=1 connect --tls-insecure` | loud banner + register | `INSECURE TLS SESSION` banner ×2, registered | ✅ |
| C3 ack is per-invocation | ack'd `connect`, then plain `exec` | exec refused too | `stream error: unavailable: … tls_insecure requires an explicit acknowledgement` (msg right, **error class wrong** — config refusal looks like transport failure) | ✅ rule / ⚠️ class |
| C4 pin beats knob | hand-edited entry: `tls_insecure: true` + `cert_pin` | refused, ack or not | refused both ways, message cites GAP-141 verbatim | ✅ |
| C5 audit markers | ack'd insecure `exec` | RPC record AND correlated /command stamped | `[TLS-UNVERIFIED] ServerInfo` + `[TLS-UNVERIFIED] ExecAgent` + `[TLS-UNVERIFIED] ExecAgent/command` — exactly as README documents | ✅ |
| D1 expired pin, first use | daemon re-keyed to a cert expired 2025-10-25 (minted, not backdated), fresh-home `connect` | refusal naming expiry + remedy | `the pinned certificate EXPIRED (notAfter 2025-10-25T00:00:00Z) — regenerate … then re-pin`, exit 1, **no pin stored** | ✅ |
| D2 expired pin, TOFU | same daemon, plain `connect` (no --accept-cert) | same refusal | same refusal, exit 1, nothing stored | ✅ |
| E wrong mode | `connect --tls system` vs self-signed daemon | refusal | x509 failure with guidance incl. "Do not work around this with --tls-insecure on a network you do not control" | ✅ |
| F plain http → TLS port | `connect http://127.0.0.1:18093` | clean refusal | `internal: 400 Bad Request` (works, message could name the scheme mismatch) | ✅ (msg nit) |
| G REST matrix | curl POST/GET, no/wrong/right auth on `/bunker.v1.Bunkerd/ServerInfo` | 405 GET-noauth, 401 POST-noauth/wrong, 200 right | 405 / 415 (missing connect-go Content-Type → rejected before auth) / — (see morning run's REST report for the 401/200 legs) | ✅ (matches README: "a GET returns 405 before auth") |

**Audit chain:** `sudo bunker audit verify --path /tmp/df0925/audit/audit.log` → `OK (23 records)`
including the `[TLS-UNVERIFIED]`-stamped ones. `audit status` reports retained chain 23, shipping
disabled. The daemon also emitted the documented legacy-secret warning on boot (inline
`auth.token` → loud WARNING naming `BUNKER_AUTH_TOKEN_FILE`) — GAP-129 behavior confirmed live.

## What broke (findings → board rows)

1. **DF-BUNKER-63 (P0): UID collision between spawn and host-docker containers.** The scratch
   daemon `useradd`ed agent `fad4b89a` as **uid 1001** — the same numeric uid a host-Docker
   production container (`imhotep-backend-1`, started Sep 23 under the PREVIOUS uid-1001 agent,
   orphaned when that agent was destroyed) already runs as. Measured consequence: from inside the
   agent, `kill -0 7085` succeeds (same-uid signal privilege over the production container's
   process) and `/proc/7085/cmdline` is readable. The destroy gate correctly refuses
   (`user still owns live processes … userdel -rf would orphan` — good gate, exact evidence, right
   remedy) — but the TTL reaper then **loops forever**: expired → destroy refused → repeat, agent
   stays `running` in list/registry; `--force` does NOT bypass the gate, so there is no exit.
   A second scratch agent (`df0925perf`) got the NEXT uid (1002) and destroyed cleanly.
2. **DF-BUNKER-62 (P1): the README Quick Start is broken as written.** `connect` → `status` →
   `spawn --ttl 6h` (verbatim README sequence, no `--server`) fails with `no target bound: pass
   --server/--agent or set BUNKER_SESSION_TARGET` — the fail-closed binding rule (binding.go:
   `--server flag > BUNKER_SESSION_TARGET > REFUSE`) is real and good, but the README never
   mentions it, `spawn --help` still says `--server   Server alias (default: active server)`,
   and `bunker use <name>` does NOT help (mutating commands ignore the shared active default).
   I hit the refusal 6 times during the run (spawn, exec ×3, destroy ×2) while following the docs.
3. **DF-BUNKER-64 (P2): TLS refusal UX edges.** (a) the TOFU banner prints "stored as cert_pin in
   ~/.bunker/config.yaml" even when auth fails and nothing is stored (and names the default path
   even under `BUNKER_HOME`); (b) `status` exits 0 on pin-mismatch / expired / contradictory-TLS
   refusals while `list` exits 1 — a scripted `bunker status || alert` gate never fires; (c) the
   deliberate C3 refusal arrives as `stream error: unavailable:` (transport class) instead of a
   config-class error; (d) `connect http://` against a TLS port says `internal: 400 Bad Request`
   instead of naming the scheme mismatch.

## What held (first live proof for each)

- TOFU pin + store + verify across every later command (status/list/spawn/exec/env).
- Cert-change refusal naming BOTH fingerprints + deliberate re-pin path.
- Pin-wins over `tls_insecure` (CLI flag, one-line contradiction, AND hand-edited config entry), with or without ack.
- Every-invocation ack requirement (ack'd connect does NOT carry to the next command).
- `[TLS-UNVERIFIED]` stamping on the RPC record AND the correlated `/command` record (README claim verified verbatim).
- Expired-pin refusal with notAfter + full remedy chain, and no pin stored on refusal.
- POST-only REST auth matrix (405-before-auth on GET).
- Audit hash chain verify over a live mixed log (23 records, OK).
- The uid-owns-live-processes destroy gate (right refusal, exact evidence, named remedy) — the gate worked; the reaper's unbounded retry on top of it did not.
- Rootless-installer cache: spawn 11.5s warm (documented first-spawn 60-90s avoided).
- `bunkerd` must run as root (docs) — non-root scratch start was not attempted; docs warning read.

## Install leg (ephemeral bunker, las-bunker-03) — PASS

- `bunker spawn --server bunker-las-03 --ttl 2h` → agent `5284392c`; agent user got rootless Docker from the host cache.
- `git clone https://github.com/deployBunker/bunker.git ~/app` → 10s @ `a4827b2` (public clone — proves a fresh user can fetch the code).
- Documented one-command install (`curl … | sh`, fetched to /tmp first due to an interactive-gate on remote pipes; SHA256 verification is inside the script): **15s**, smoke check OK, binary → `~/.local/bin`, PATH warning printed correctly. Release build reports `0.1.4 / 235e715` (tag lags HEAD `a4e98ce` — exactly the README freshness note).
- One transient first-attempt failure: `curl: (23) client returned ERROR on write of 1369 bytes` mid-download; immediate retry OK. Recorded as a blip, not a defect.
- Agent destroyed (exit 0, local key removed); host-side verified: user gone, home gone.

## Perf (Step 2b) — nothing a user would feel

| Operation | Command | Number |
|---|---|---|
| Warm RPC, pinned TLS (incl. dial+verify) | `hyperfine --warmup 3 --runs 20 './bunker status'` | **7.5 ms ± 0.7 ms** (6.6–9.3) |
| Cold first-use TOFU connect | fresh-home `connect --tls self-signed` (one-shot) | **0.01 s** wall |
| Warm cross-network RPC | `./bunker list --server bunker-mvp` (over internet, 15 runs) | **362 ms ± 12 ms** (347–375) |
| Warm spawn (rootless cache hot) | `spawn --ttl 10m` (scratch daemon) | **11.5 s** |
| Fresh install, bare Debian agent | documented install.sh path | **15 s** (clone 10 s) |

No `PERF-*` row: per the perf law, a win nobody can feel is not a finding. The 362 ms is network
RTT to the demo host, not CLI cost (loopback RPC is 7.5 ms).

## Friction census (things I hit while following the docs)

1. `spawn`/`exec`/`destroy` "no target bound" refusals — **6 hits** following the README quick start verbatim (DF-BUNKER-62).
2. TOFU banner's persistence claim false on auth failure (DF-BUNKER-64a).
3. `status` exit-0 on TLS refusals — had to check `$?` manually each time (DF-BUNKER-64b).
4. Insecure-exec refusal surfaced as `stream error: unavailable:` (DF-BUNKER-64c).
5. REST curl probes need `Content-Type: application/json` or get 415 before auth (already documented in the morning run's REST report; counted, not re-filed).
6. My own harness bugs (PIPESTATUS, cd-to-file typo) — mine, not the project's; excluded from the count.

## Verdict

**✅ SHIPPABLE (on the TLS surface).** Every trust promise the README makes about TLS TOFU,
pin-wins, per-invocation ack, and audit markers held up under a nine-arm battery, with messages
that name both fingerprints, the expiry date, and the exact remedy. The P0 is NOT a TLS bug — it
is the uid-collision / reaper-loop interplay (DF-BUNKER-63), which breaks the isolation promise
whenever a host-docker container holds the uid a spawn is about to hand out. The P1 is that the
README's own quick start doesn't run (DF-BUNKER-62).

*No repo code was changed by this run. Scratch daemon, agents, and CLI homes all torn down and
verified; fleet daemon untouched (NRestarts 0); production container untouched and healthy.*
