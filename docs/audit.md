# Bunker Audit Trail — Querying and Verification

`bunkerd` writes an append-only JSONL audit trail of every authenticated RPC
(one record per request; file mode `0600`; token values are never written —
caller identity is derived from the authenticated claims). This document
covers the record format and the `bunker audit` command surface for
inspecting it.

- **`bunker audit verify`** — check the hash chain **and the anchor** of a
  local log.
- **`bunker audit list`** — print matching records as a table.
- **`bunker audit export`** — stream matching records as JSONL (lossless).
- **`bunker audit status`** — one-glance local summary of the log's on-disk
  state: chain head, **chain length and integrity verdict**, **anchor state**,
  file/backup sizes, rotation count, and shipping state
  (see [Log status](#log-status) below).

The three query subcommands (`verify`, `list`, `export`) read the **local**
log at `--path` by default (`/var/log/bunkerd/audit.log`). `list` and
`export` also accept `--server` to query a **remote daemon** over the
`QueryAudit` RPC instead; `verify` and `status` are local-only (run them on
the host that owns the log).

## Record format

One JSON object per line, with the same keys the daemon writes. A record is
written after every authenticated request completes (outcome included):

```json
{"ts":"2026-08-20T12:00:00.123456789Z","caller":"master","method":"/bunker.v1.Bunkerd/SpawnAgent","remote_addr":"10.0.0.5:54321","agent_id":"abc123","duration_ms":42,"outcome":"ok","summary":"SpawnAgent agent_id=abc123","hash":"9f2c…","prev_hash":"1a8b…"}
```

| Key | Meaning |
|-----|---------|
| `ts` | RFC3339Nano UTC timestamp of request completion |
| `caller` | authenticated identity: `master`, `agent:<id>`, `agent:<id> key:<keyid>`, `key:<keyid>`, or the JWT subject — never the raw token |
| `method` | full connect procedure, e.g. `/bunker.v1.Bunkerd/SpawnAgent` |
| `remote_addr` | client address as seen by the server |
| `agent_id` | target agent of the request (`""` when none) |
| `duration_ms` | wall time of the request in milliseconds |
| `outcome` | `ok`, or the connect error code (e.g. `not_found`, `unauthenticated`) |
| `summary` | short human-readable request summary (never request contents); prefixed by a transport marker when the record describes an unverified session or a plaintext listener (see below) |
| `hash` | SHA-256 hex digest of the canonical record line (hash field empty) |
| `prev_hash` | `hash` of the previous record in the chain (`""` for the first) |

## TLS verification markers

A record's `summary` is prefixed with one or both of these markers when the
request's transport was not verified. Both ride the existing `summary` field, so
no parser that reads the documented keys breaks, and both are part of the hashed
canonical line — a marked record still verifies.

| Marker | What it means | Where it comes from |
|--------|---------------|---------------------|
| `[INSECURE-PLAINTEXT]` | the request arrived over a non-loopback listener with TLS disabled (`tls.enabled: false` + `tls.insecure_dev: true`) | the daemon's audit log, which stamps every record it writes while that opt-in is active (GAP-126) |
| `[TLS-UNVERIFIED]` | the CLIENT declared that its TLS session skipped certificate verification — i.e. it dialed the daemon with `tls_insecure: true` / `--tls-insecure` under `BUNKER_ALLOW_TLS_INSECURE=1` | the audit interceptor, from the `X-Bunker-TLS-Unverified: 1` request header the CLI sends on every request of such a session (GAP-141) |

The two are independent and can appear together: a request sent over a plaintext
listener *by* a client that also skipped verification carries both.

`[TLS-UNVERIFIED]` is stamped on **every** record written for the request — the
per-RPC record **and** the correlated `/command` exec/run record (GAP-142) — so a
filtered query cannot miss the command an unverified session issued:

```bash
# Which sessions did not verify the daemon, and what did they run?
bunker audit export --path /var/log/bunkerd/audit.log | grep TLS-UNVERIFIED
```

The marker is a client **declaration**, not a proof: nothing in the TLS protocol
tells a server how its peer chose to verify it, so a client could omit the
header. What is provable is that the record is append-only and hash-chained — a
session cannot remove the mark after the fact. Read it as "this session said it
did not verify me", and treat an unmarked record from an untrusted network as
unproven rather than as verified.

## Rotation and the hash chain

The live log rotates at 5 MiB, keeping 3 backups: `audit.log` (live),
`audit.log.1` (most recent backup), `.2`, `.3` (oldest retained). Records
are hash-chained with SHA-256: every record's `hash` digests its own line
bytes, and `prev_hash` binds it to the previous record. The chain spans
rotations — the first record of a fresh file chains to the last record of
the rotated file. `bunker audit verify` checks both properties across the
whole retained chain; tampering is reported with the first bad record index
and a non-zero exit.

## Anchoring and tamper-evidence

Hash chaining alone detects *accidental* corruption, not an attacker. The chain
is unkeyed, so anyone who can write `audit.log` can recompute every hash and
`prev_hash`; and removing trailing records leaves a shorter chain that still
verifies from genesis. Both holes are closed by the **anchor**, which is **on by
default — no config flag enables it**.

Files the audit trail owns (all mode 0600):

| File | What it is |
|------|------------|
| `<audit.path>` | the log itself: one hash-chained JSON record per request |
| `<audit.path>.anchor` | the chain head observed after the most recent record, authenticated with HMAC-SHA256 under the anchor key |
| `<audit.path>.sealkey` | the auto-provisioned anchor key (32 random bytes, hex) |
| `<audit.path>.1` … `.3` | rotated backups of the log |
| `<audit.path>.shipstate` | the last remote-ship attempt (only when `audit.ship_to` is configured) |

How it works:

- The writer refreshes `<audit.path>.anchor` after **every** record, naming the
  record it just wrote. The file is published by `rename`, so a reader sees the
  previous anchor or the new one — never a half-written file (a torn anchor
  would read as tampering).
- The anchor key is `audit.seal_key` when that is configured; otherwise it is
  provisioned once at `<audit.path>.sealkey`. An explicitly configured
  `audit.seal_key` is deliberately **not** copied next to the log.
- `bunker audit verify` fails (non-zero) when the anchored head is no longer
  present anywhere in the retained chain — **truncation** — and when the
  anchor's MAC does not validate — **forgery**. `bunker audit status` reports
  the same verdict, with the reason, in its `chain status` / `gaps` fields.
- If the key cannot be resolved (or an explicit key is configured and the
  verifier is not told it), the anchor is written — or reported — as
  **unkeyed** / **unverified**: truncation is still detectable, the missing MAC
  check is stated rather than hidden, and the trail keeps working.

### What the anchor proves, and what it does not

| Detects | Does not detect |
|---------|-----------------|
| a record edited, inserted, reordered or **removed by anything that can write the log but cannot read the key** — a compromised agent, a rogue component, a buggy writer | an attacker with **root on this host**, who can rewrite the log, the anchor and the key together |
| a **truncated tail**: records removed from the end of the live file or of a rotated backup | deletion of the anchor file itself — reported honestly as *unanchored* (`anchor … NONE`), not as verified |
| a chain recomputed to hide a record (the re-anchored head cannot be authenticated without the key) | loss of records rotated away beyond the three-backup budget |

**The tamper-evidence claim is conditional.** A local anchor is not a
tamper-*proof* record; it makes a silent edit detectable and a deletion visible
rather than invisible. For a claim that survives root on the host, configure
`audit.ship_to` so rotated segments are shipped **off the box** (GAP-073): that
copy is the anchor an attacker on this host cannot rewrite.
`bunker audit status` says plainly, in those words, when no off-host anchor is
configured, and its `health` verdict can only be `healthy` when an off-host
anchor exists.

Anchor states reported by both `verify` and `status`:

| State | Meaning |
|-------|---------|
| `keyed` | anchor present and its MAC validated under the anchor key |
| `unkeyed` | anchor present with no MAC (no key was available when it was written): truncation detectable, forgery not |
| `unverified` | the anchor declares a MAC but the matching key was not available, so it could not be checked (a host configured with `audit.seal_key` reads this way unless the config describes the log being verified) |
| `none` | no anchor file: the chain is unanchored and a truncated tail verifies |

### How to verify chain integrity

```sh
# Chain + anchor, human-readable. Exits non-zero on any failure.
bunker audit verify --path /var/log/bunkerd/audit.log

# The verdict as data: a broken or truncated chain shows chain_ok=false with the
# gap named, and the anchor state says how strongly it could be checked.
bunker audit status --json | jq '{chain_ok: .ChainOK, chain_length: .ChainLength, gaps: .Gaps, anchor: .Anchor.State, health: .Health}'

# Use it as a gate (status itself always exits 0 — it is a report, not a gate):
bunker audit status --json | jq -e '.ChainOK' >/dev/null && echo chain-ok || echo CHAIN-BROKEN
```

A file that verified under the old (unanchored) rules but has lost its tail now
fails with `chain truncated: anchored head <hash> is not present in the retained
chain`; an anchor whose head was rewritten fails with `anchor MAC mismatch
(anchor tampered: head …)`.

## Log status

`bunker audit status` prints a one-glance summary of the log's on-disk state
plus the retention-hardening state (remote shipping, rotation sealing).
Like `verify`, it is **local-only** — run it on the host that owns the log.
It also works on a host with no audit log at all: a missing log file reports
`enabled: false`, `health: disabled` and exits 0.

Unlike a stored "last run" timestamp, its integrity fields are a **verdict
computed by the command itself**: the retained chain is walked on the pass that
produces the report, so `chain status`, `gaps` and `health` can never describe
an older state than the one on disk. Status always exits 0 (it is a report, not
a gate); scripts that want a boolean read `--json` and check `.ChainOK` /
`.Health`.

Flags:

| Flag | Meaning |
|------|---------|
| `--path <file>` | log tree to inspect (default: the daemon config's `audit.path`, else `/var/log/bunkerd/audit.log`; rotated backups `.1`-`.3` are included) |
| `--json` | emit machine-readable JSON instead of plain text |

Plain-text output, one field per line:

```text
audit log:            /var/log/bunkerd/audit.log
enabled:              true
chain head:           9f2ca1b7…full 64-hex digest of the last live record…
chain length:         4 records (retained chain)
last anchor hash:     9f2ca1b7…same hash the anchor names…
records:              4 (retained chain)
live size:            1442 bytes
backup .1 size:       5242880 bytes
backup .2 size:       5242880 bytes
rotations:            >= 2 (lower bound; files rotated beyond the 3-backup budget are gone)
chain status:         OK — 4 records verified against the hash chain
gaps:                 none
local anchor:         keyed — MAC validated, key id 1a2b3c4d5e6f7081, head 9f2ca1b7…, written 2026-09-14T12:00:00.123456789Z
off-host anchor:      off-host anchor configured (https://collector.example.net/v1), last shipped 2026-09-14T12:00:00Z
health:               healthy — chain verified, anchor MAC validated, off-host anchor configured
last verify:          n/a (verify history is not recorded; the chain status line above is this run's verification)
shipping:             enabled (https://collector.example.net/v1)
last ship attempt:    2026-09-14T12:00:00Z
last ship result:     ok
last ship success:    2026-09-14T12:00:00Z
ship retry queue:     2 segment(s)
rotation sealing:     true
```

On a host with no off-host anchor the last three integrity lines read:

```text
chain status:         OK — 4 records verified against the hash chain
gaps:                 none
local anchor:         keyed — MAC validated, key id 1a2b3c4d5e6f7081, head 9f2ca1b7…, written 2026-09-14T12:00:00.123456789Z
off-host anchor:      no off-host anchor configured
health:               degraded — chain verified and locally anchored, but no off-host anchor configured
```

and a chain that no longer verifies reads (status still exits 0):

```text
chain status:         BROKEN at record 3 — /var/log/bunkerd/audit.log: record 3: hash mismatch (tampered)
gaps:                 /var/log/bunkerd/audit.log: record 3: hash mismatch (tampered)
health:               broken — chain integrity failure: …
```

Field notes:

- **chain head** — hash of the last record of the live file (empty when the
  live file is empty). A tampered or unparseable live file still reports:
  the head is the last parseable record's declared hash, and `bunker audit
  verify` is the verdict on the chain itself.
- **chain length** — the same count as `records`, named for the status
  contract; **last anchor hash** is the same value as `chain head`, named as
  the hash the anchor refers to.
- **chain status / gaps** — the verdict of a real verification performed by
  this run (`OK — N records verified…`, or `BROKEN at record N — <reason>` when
  the chain itself failed, or `BROKEN — <reason>` when the anchor check failed).
  `gaps` lists the same failure; `none` is the only clean value.
- **local anchor** — state (`keyed` / `unkeyed` / `unverified` / `none`), the
  key id and the anchored head when it is keyed, and the reason whenever the
  anchor is weaker than a validated MAC. Read it with
  [Anchoring and tamper-evidence](#anchoring-and-tamper-evidence).
- **off-host anchor** — `off-host anchor configured (<target>), last shipped …`
  when `audit.ship_to` is set, otherwise the literal
  `no off-host anchor configured` (read from the ship-state file, or from the
  daemon config when that config describes this log).
- **health** — one word plus its reason: `healthy` only with a verified chain,
  a validated anchor MAC **and** an off-host anchor; `degraded` when the chain
  verifies but the strongest claim does not hold here; `broken` on any chain
  integrity failure; `disabled` when no log exists at the path.
- **records** — total retained-chain record count (live file plus backups
  `.1`-`.3`), counted by re-reading the files.
- **backup sizes** — one line per backup that exists on disk; absent backups
  print no line.
- **rotations** — a **lower bound**: the number of backups present. Files
  rotated beyond the 3-backup budget no longer exist, so true rotation count
  can be higher.
- **last verify** — always `n/a` for the *stored* verdict: verify history is
  intentionally not recorded anywhere. The verdict is `chain status` above,
  which is computed on this run.
- **shipping** — read from the ship-state file the daemon writes next to the
  log (`<path>.shipstate`) when remote shipping is configured. `last ship
  result` is `ok` or `error: …`; `last ship success` prints `never` until
  the first successful shipment; the retry queue counts rotated segments
  waiting to be shipped. Without a ship-state file the line reads
  `shipping: disabled`.
- **rotation sealing** — `true` when any retained record is a rotation seal
  record. Note: a daemon configured with `audit.seal_key` that has not
  rotated yet shows `false` — there is nothing on disk to prove sealing
  from.

With `--json` the same state is emitted as one indented JSON object. The
report struct carries no JSON tags, so its keys are the Go field names
(`ChainHead`, `Records`, …) while the nested ship state is snake_case
(`ship_to`, `queue_depth`):

```json
{
  "Enabled": true,
  "ChainHead": "9f2ca1b7…",
  "Records": 4,
  "LiveSize": 1442,
  "BackupSizes": [5242880, 5242880, -1],
  "RotationsLowerBound": 2,
  "Shipping": {"ship_to": "https://collector.example.net/v1", "last_result": "ok", "queue_depth": 2},
  "SealingEnabled": true,
  "LastSeal": null,
  "StoragePath": "/var/log/bunkerd/audit.log",
  "ChainLength": 4,
  "LastAnchorHash": "9f2ca1b7…",
  "ChainOK": true,
  "FirstBadRecord": 0,
  "ChainError": "",
  "Gaps": [],
  "Anchor": {
    "Present": true,
    "State": "keyed",
    "Head": "9f2ca1b7…",
    "TS": "2026-09-14T12:00:00.123456789Z",
    "KeyID": "1a2b3c4d5e6f7081",
    "MACValid": true,
    "Note": "",
    "OffHost": true,
    "OffHostTarget": "https://collector.example.net/v1",
    "OffHostLastSuccess": "2026-09-14T12:00:00Z",
    "Statement": "off-host anchor configured (https://collector.example.net/v1), last shipped 2026-09-14T12:00:00Z"
  },
  "Health": "healthy",
  "HealthReason": "chain verified, anchor MAC validated, off-host anchor configured"
}
```

The GAP-136 fields are the ones to script against: `ChainOK`, `ChainLength`,
`Gaps`, `Anchor.State`/`Anchor.MACValid`/`Anchor.OffHost`, `Health`. Script
`jq -e '.ChainOK'` rather than the exit code — status always exits 0.

## Filters

`list` and `export` accept the same filters; all are ANDed, empty filters
match everything:

| Flag | Meaning |
|------|---------|
| `--agent <id>` | exact match on `agent_id` |
| `--method <substr>` | substring match on the procedure (e.g. `--method SpawnAgent`) |
| `--since <RFC3339>` | records at or after this timestamp (inclusive) |
| `--until <RFC3339>` | records at or before this timestamp (inclusive) |
| `--limit <n>` | max records, keeping the **newest** matches |
| `--path <file>` | local log to read (default `/var/log/bunkerd/audit.log`) |
| `--server <alias>` | query the named daemon via the `QueryAudit` RPC; `--path` is then ignored |

Records are returned oldest first (chain order: `.3` → `.2` → `.1` → live
file). Records whose `ts` does not parse are excluded whenever a time filter
is set, and included otherwise. `--server` resolves the daemon exactly like
`bunker list` does (server alias from `bunker connect`, bearer token from
the stored entry / `BUNKER_TOKEN`).

## Examples

```sh
# Local: last 20 records for one agent
bunker audit list --agent abc123 --limit 20

# Local: all spawns in the last hour
bunker audit list --method SpawnAgent --since "$(date -u -d '1 hour ago' +%Y-%m-%dT%H:%M:%SZ)"

# Remote: same query against the daemon registered as "prod"
bunker audit list --server prod --method SpawnAgent --since 2026-08-20T00:00:00Z

# Lossless export → feed into jq
bunker audit export > audit.jsonl
bunker audit export --agent abc123 | jq -r '.method' | sort | uniq -c
bunker audit export --server prod --until 2026-08-21T00:00:00Z | jq -c 'select(.outcome != "ok")'

# Verify the local chain AND its anchor (host-side; covers rotated backups)
bunker audit verify --path /var/log/bunkerd/audit.log

# One-glance state of the local log (host-side): integrity, anchor, sizes, shipping
bunker audit status

# Same, machine-readable (feed into jq)
bunker audit status --json | jq '.ChainHead, .Anchor.State, .Health, .Shipping.queue_depth'

# Gate on it: a truncated or edited chain makes ChainOK false
bunker audit status --json | jq -e '.ChainOK' >/dev/null || echo CHAIN-BROKEN

# Inspect a specific log tree, e.g. a relocated audit dir
bunker audit status --path /var/log/bunkerd/audit.log.1
```

## Notes

- **Local vs remote:** without `--server` the CLI reads the log file itself
  (`--path`); with `--server` it calls the daemon's `QueryAudit` RPC, which
  reads the daemon's own configured `audit.path`. Local and remote output
  are byte-identical in content. `--server` applies to `list` and `export`
  only — `verify` and `status` are local-only.
- **Export format:** JSONL (one record per line), matching the on-disk log
  format; `hash` and `prev_hash` are preserved, so an export can be
  re-verified or replayed losslessly.
- **Audit disabled:** a daemon started with `audit.enabled: false` answers
  `QueryAudit` with `unavailable` and a clear message. `bunker audit
  status` still works on such a host: a missing log file reports
  `enabled: false`.
- **Read-only surface:** `bunker audit` never modifies the trail — no
  deletion, truncation, or pruning. `bunker audit verify` only reads.
