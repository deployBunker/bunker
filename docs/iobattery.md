# I/O Measurement Battery (`bunker iobattery`) — BFS-057

**Status:** active harness. **Rows:** BFS-055 (transport lever) · BFS-057 (this battery) · BFS-059 (quick-win levers).

## Why it exists FIRST

No mount/FUSE performance change lands without a before/after number produced by
this battery on the hardware the change ships for. Assumptions, vendor claims,
and "should be faster" are not inputs; this harness is.

## Running against a real bunker mount

```bash
# 1. Mount the agent home (the mount chain under test)
bunker mount <agent-id> /mnt/bunker/<agent-id>

# 2. Run the battery against the mount point, report to a file
bunker iobattery --target /mnt/bunker/<agent-id> --out /tmp/bfs057-<date>.json

# 3. Make the lever change (e.g. BFS-055 transport), remount, run again with
#    the SAME flags, and diff the two reports family by family.
```

Expected runtime: a few seconds with the defaults (32 MiB payload, 2000 ops,
4 load workers). For mount scoring use `--size-mb 256 --ops 10000`; budget
about 2–5 minutes depending on the RTT to the DC host. Run each configuration
3 times and report the median — a single green run proves nothing about a
noisy network path.

## Fallback mode (no bunker agent available)

The target is any directory, so the battery runs against a plain local
directory as a smoke/fallback mode. This proves the harness end-to-end but the
numbers are NOT mount numbers:

```bash
bunker iobattery --target /tmp/io-scratch --out /tmp/bfs057-fallback.json
```

## Families and flags

| Family | What it measures | Headline metric |
|---|---|---|
| `throughput` | single-stream sequential write + read of `--size-mb` MiB | MB/s (write and read separately in `detail`) |
| `latency_under_load` | 4K create+unlink round-trips while `--load-workers` goroutines hammer the same dir | p99 ms (p50/p95 in `detail`) |
| `metadata_ops` | create / stat / rename / unlink cycles on small files | ops/s |
| `iops` | random 4K reads+writes (deterministic xorshift offsets, reproducible) | IOPS |
| `cpu_per_byte` | rusage (self+children) CPU time vs bytes moved through write+fsync+read | bytes per CPU-second |
| `negative_control` | the readahead lever, proven by measurement: buffered sequential read (kernel readahead active) vs O_DIRECT read (readahead bypassed) | MB/s + `readahead_lift_ratio` |

Flags: `--target` (required), `--out` (report file; default stdout),
`--size-mb`, `--ops`, `--load-workers`, `--no-control`.

## Whole-file-publish mounts (bunker-fs)

A bunker-fs / WebDAV-backed mount may accept whole-file publication while
rejecting append, in-place random writes, or chmod/metadata changes. Before
measuring, the battery creates a disposable probe file and tests append
(`O_APPEND`), in-place `WriteAt`, and `chmod`. Only explicit
`EOPNOTSUPP`/`ENOTSUP` responses mark a capability unsupported; `EIO`, permission
errors, and other failures are not excused by this preflight and remain visible
as ordinary measurement errors.

On a whole-file-publish-only target, the affected families are `throughput`
and `cpu_per_byte` (append-style write shape), `iops` (in-place random writes),
and `metadata_ops` (metadata operation). They remain in `measurements` with
`error` beginning `unsupported:` and `detail.status: unsupported`, plus a
reason naming the write shape. The target-level `write_shape` object records the
probe outcomes. `latency_under_load` retains its normal whole-file create/remove
path. A POSIX/ext4 target reports the same family measurements and pass/fail
semantics as before. `negative_control` is independently reported, including
its existing O_DIRECT availability reason.

Treat an explained unsupported result as a capability limitation, not a green
measurement: do not compare its metric with a supported run. A family with an
ordinary I/O error is still a failure that needs investigation.

## Report contract

One JSON document (`schema: bunker.iobattery.v1`). Every measurement carries:

- `count` — bytes moved or ops issued,
- `elapsed_ms` — measured wall time,
- `derived` / `derived_unit` — the headline metric,
- `command` — the exact invocation to reproduce the number,
- `error` — empty on success; a skipped or half-unavailable family NAMES the
  reason (e.g. O_DIRECT unsupported on tmpfs). An unexplained null is junk.

The negative control also records the mount's BDI `read_ahead_kb` (best-effort,
empty string when not resolvable) so the report names the knob it exercised.

## Interpreting the negative control

- On a lever-shaped target (FUSE/sshfs mount) buffered and O_DIRECT reads
  differ — that difference IS the lever working; `readahead_lift_ratio` is the
  ceiling a readahead raise can buy.
- On tmpfs O_DIRECT is unsupported and the family records
  `odirect_supported: false` + the error instead of faking a green control.

## Where the numbers go

Before/after pairs from this battery belong in the PR description (or the
board row's evidence block) for any performance row: BFS-055, BFS-059, and
anything else that touches `internal/fsmount/` or the mount drivers. Design
background: `docs/performance.md` and `docs/prd/SPEC-linux-io-max.md`.
