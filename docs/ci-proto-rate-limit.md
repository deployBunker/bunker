# CI protobuf step vs BSR rate limiting (INT-CI-024)

## The failure

Both `root-suite` and `regression` failed at the `Generate protobuf code` step
(`run: buf generate`) on:

- run 35496282840 (sha 3ae9a78)
- run 35496107562 (sha 43e1cf6)

with:

```
Failure: resource_exhausted: too many requests
Please see https://buf.build/docs/bsr/rate-limits/ for details about BSR rate limiting.
```

Concurrent jobs on the shared self-hosted runner IP trip the BSR rate limit.
Both runs still reported `conclusion=success` at run level (`regression` carries
`continue-on-error: true`; the `root-suite` failure did not propagate), so green
CI has been taken as evidence while the root-gated spawn/cgroup suite and the
live regression battery have not executed on any of the 6 most recent pushes.

## Mechanism chosen

`.github/scripts/ensure-proto.sh`, wired identically into all five jobs of
`.github/workflows/ci.yml` (`build-and-test`, `unit-tests`, `gitreins-guard`,
`regression`, `root-suite` — every job that ran `buf generate`). One shared
mechanism, not five hand-edited copies.

1. **Skip when not needed.** The workflow adds `actions/cache@v4` restoring
   `proto/` keyed on `hashFiles('**/*.proto', 'buf.gen.yaml')`
   (`proto-gen-v1-<hash>`). The script hashes the same inputs
   (`git ls-files -- '*.proto' '*.yaml'`), compares against the committed
   stamp `proto/.codegen-fresh`, and also honors the cache-hit output. If the
   committed generated files are already current, the job SKIPS `buf generate`
   entirely — no BSR call. The stamp is committed in this same change, so the
   first post-merge runs take the skip path.
2. **Retry with backoff.** When regeneration is needed, a BSR
   `resource_exhausted` / `too many requests` / rate-limit failure retries with
   exponential backoff plus jitter: 5 attempts, delays 3s, 6s, 12s, 24s
   (+0–4s jitter). Any OTHER buf failure fails the job immediately, and retry
   exhaustion fails it loudly with `::error::` — genuine breakage is not
   hidden. A failed attempt never writes the stamp.
3. **BUF_TOKEN:** the repo has none configured (verified with
   `gh secret list -R deployBunker/bunker` — empty). The script does not
   invent or require one; buf reads `BUF_TOKEN` from the environment natively,
   so adding a secret later needs no script change.

Self-hosted runners execute this step as root in an actions-runner-owned
checkout; every git call in the script carries `-c safe.directory='*'`
(command line, protected config) so the freshness check cannot become a new
"dubious ownership" failure there.

## Evidence that a rate-limited `buf generate` is now non-fatal

Tested in a scratch clone of this branch with a `buf` stub that records each
invocation and replays the observed failure text (`resource_exhausted: too
many requests`), via `/tmp/proto-test-024-runner.sh` — 15/15 checks pass:

- Regeneration needed + success → stamps, exit 0, exactly one `buf` call.
- Stamp current → exit 0 with **zero** `buf` calls even when the stub always
  rate-limits (skip path cannot touch the BSR).
- `PROTO_CACHE_HIT=true` → same skip with no stamp at all.
- Inputs changed → stamp mismatch → regeneration re-runs, re-stamps.
- Rate limit ×2 then success → 3 attempts, backoff+jitter sleeps observed
  (~6s), exit 0, stamped.
- Rate limit always → MAX_ATTEMPTS attempts, then exit 1 with `::error::` and
  `RATE-LIMIT-RETRY-EXHAUSTED`; **no stamp written**.
- Non-rate-limit failure → exactly 1 attempt, exit 1, no retry of genuine
  breakage.

With the REAL `buf` against the BSR (no stub): the script regenerated from
`proto/bunker/v1/bunker.proto` at rc=0 and the working tree showed **zero
modified tracked files** — committed generated code is byte-identical to
`buf generate` output, so CI's common path after merge is the skip path.

## How the change was validated structurally

- `actionlint v1.7.12` on `ci.yml`: exit 1 with ONLY the two pre-existing
  `runs-on: [self-hosted, Linux, bunker]` custom-label notices (regression +
  root-suite runner labels, unchanged by this branch). Byte-identical findings
  on the base revision `8316fea` — zero new findings introduced.
- `python3 -c "import yaml; yaml.safe_load(...)"` parses cleanly (script
  `/tmp/ci_check_024.py`): job names and order unchanged, exactly one new step
  (the cache restore) inserted immediately before the proto step in each job,
  every other step byte-identical, job-level `needs`/`if`/`continue-on-error`
  keys preserved, and the proto/cache step definitions byte-identical across
  all five jobs.

## Repo green on this branch

`go build ./...` OK, `go vet ./...` OK, `go test ./... -short -count=1` run —
log `/tmp/int-ci-024-gotest.log`. Existing CI wiring pins
(`internal/hostsetup/ciwiring_test.go`, which parses `ci.yml`) still pass.

## Known residuals (out of scope, unchanged)

- `continue-on-error: true` on `regression` and `root-suite` still masks a
  job-level failure as run-level success. That masking is exactly why the
  rate-limited runs read green; fixing it is a separate board row.
- If the stamp is ever stale relative to the committed generated code (manual
  edit of `*.pb.go`), the skip path would keep the stale files. The stamp is
  written only by the script after a successful `buf generate`, and the
  committed stamp was verified against a real regeneration (zero diff).
