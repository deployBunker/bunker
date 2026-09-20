# Bunker Audit Trail — Querying and Verification

`bunkerd` writes an append-only JSONL audit trail of every authenticated RPC
(one record per request; file mode `0600`; token values are never written —
caller identity is derived from the authenticated claims). This document
covers the record format and the `bunker audit` command surface for
inspecting it.

- **`bunker audit verify`** — check the hash chain of a local log.
- **`bunker audit list`** — print matching records as a table.
- **`bunker audit export`** — stream matching records as JSONL (lossless).
- **`bunker audit status`** — one-glance local summary of the log's on-disk
  state: chain head, file/backup sizes, rotation count, and shipping state
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
| `summary` | short human-readable request summary (never request contents) |
| `hash` | SHA-256 hex digest of the canonical record line (hash field empty) |
| `prev_hash` | `hash` of the previous record in the chain (`""` for the first) |

## Rotation and the hash chain

The live log rotates at 5 MiB, keeping 3 backups: `audit.log` (live),
`audit.log.1` (most recent backup), `.2`, `.3` (oldest retained). Records
are hash-chained with SHA-256: every record's `hash` digests its own line
bytes, and `prev_hash` binds it to the previous record. The chain spans
rotations — the first record of a fresh file chains to the last record of
the rotated file. `bunker audit verify` checks both properties across the
whole retained chain; tampering is reported with the first bad record index
and a non-zero exit.

## Log status

`bunker audit status` prints a one-glance summary of the log's on-disk state
plus the retention-hardening state (remote shipping, rotation sealing).
Like `verify`, it is **local-only** — run it on the host that owns the log.
It also works on a host with no audit log at all: a missing log file reports
`enabled: false` and exits 0.

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
records:              4 (retained chain)
live size:            1442 bytes
backup .1 size:       5242880 bytes
backup .2 size:       5242880 bytes
rotations:            >= 2 (lower bound; files rotated beyond the 3-backup budget are gone)
last verify:          n/a
shipping:             enabled (https://collector.example.net/v1)
last ship attempt:    2026-09-14T12:00:00Z
last ship result:     ok
last ship success:    2026-09-14T12:00:00Z
ship retry queue:     2 segment(s)
rotation sealing:     true
```

Field notes:

- **chain head** — hash of the last record of the live file (empty when the
  live file is empty). A tampered or unparseable live file still reports:
  the head is the last parseable record's declared hash, and `bunker audit
  verify` is the verdict on the chain itself.
- **records** — total retained-chain record count (live file plus backups
  `.1`-`.3`), counted by re-reading the files.
- **backup sizes** — one line per backup that exists on disk; absent backups
  print no line.
- **rotations** — a **lower bound**: the number of backups present. Files
  rotated beyond the 3-backup budget no longer exist, so true rotation count
  can be higher.
- **last verify** — always `n/a`: verify history is intentionally not
  tracked anywhere.
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

With `--json` the same state is emitted as one indented JSON object
(`enabled`, `chain_head`, `records`, `live_size`, `backup_sizes` with `-1`
for absent backups, `rotations_lower_bound`, `shipping` (`ship_to`,
`last_attempt`, `last_result`, `last_success`, `queue_depth`),
`sealing_enabled`, and `last_seal` — the newest rotation seal's
`sealed_head`, `seal` HMAC, `ts` and `hash`, so an operator can re-derive
and compare it against shipped copies):

```json
{
  "Enabled": false,
  "ChainHead": "",
  "Records": 0,
  "LiveSize": 0,
  "BackupSizes": [-1, -1, -1],
  "RotationsLowerBound": 0,
  "Shipping": null,
  "SealingEnabled": false,
  "LastSeal": null
}
```

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

# Verify the local chain (host-side; covers rotated backups)
bunker audit verify --path /var/log/bunkerd/audit.log

# One-glance state of the local log (host-side): chain head, sizes, shipping
bunker audit status

# Same, machine-readable (feed into jq)
bunker audit status --json | jq '.chain_head, .shipping.queue_depth'

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
