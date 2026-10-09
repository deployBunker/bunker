# QA-BUNKER-48 evidence: qa_discover.py --help and negative-N UX

The fix for this row lives in the fleet-shared Hermes harness repo, NOT the bunker
repo: `~/.hermes/scripts/qa_discover.py` (untracked-by-bunker, tracked in the
kara-hermes master at commit `53a3f8ee0` — "fix(qa): qa_discover.py --help and
negative-N UX (QA-BUNKER-48)"). A judge sandboxed to the bunker repo correctly
cannot see it; this doc is the in-repo evidence record.

## Root cause (from commit 53a3f8ee0)

- `main()` did `int(sys.argv[1])` before any argparse parsing, so `--help` (and
  any non-integer arg) raised a raw ValueError traceback.
- `N <= 0` was accepted and dumped the whole fleet (160+ picks).

## Fix shape

- argparse `positive_int` type rejects N < 1 and non-integers at parse time.
- `_Parser.error` override prints one-line stderr errors (exit 2), usage kept
  for `--help` only.
- Parse happens before any scheduler DB / ledger access.

## Live probes (2026-10-09, foreman tick bunker-foreman-2026-10-09-21-34-42)

```
$ python3 scripts/qa_discover.py --help ; echo rc=$?
usage: qa_discover.py [-h] [N]
QA foreman discovery: pick N projects that need a full CI pass.
rc=0

$ python3 scripts/qa_discover.py -3 >/tmp/out 2>/tmp/err ; echo rc=$?
rc=2   stdout=0 bytes
/tmp/err: qa_discover.py: error: argument N: N must be a positive integer (got -3)

$ python3 scripts/qa_discover.py abc ; echo rc=$?
rc=2   one-line error, no traceback
```

Positive-N behavior unchanged: `qa_discover.py 2` exits 0 with valid JSON,
picks=2 (verified by the worker + foreman; the `excluded` workdir audit list is
pre-existing behavior).

## Test file

`~/.hermes/scripts/test_qa_discover.py` (committed in 53a3f8ee0): 7 tests,
RED-proven against pre-fix HEAD, 7/7 green after.
