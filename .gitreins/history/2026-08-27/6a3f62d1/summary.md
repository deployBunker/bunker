# Verdict: GAP-058

**Task:** Stale ~/go/bin binaries + README freshness note
**Evaluated:** 2026-08-27T18:41:03.589608
**Result:** ✓ PASS

## Criteria

- ✓ **PASS: ~/go/bin/bunker --version shows commit >= c578969 (not b7d3bb6); ~/go/bin/bunkerd --version prints the version block and exits 0 without binding; README quickstart carries a binary-commit-vs-HEAD freshness note.**
  - (1) `~/go/bin/bunker --version` outputs commit 4af949d, built 2026-08-27T18:39:44Z, exit 0; git log confirms 4af949d is a descendant of c578969 (which is a descendant of b7d3bb6), so commit >= c578969 and not b7d3bb6. (2) `~/go/bin/bunkerd --version` prints the full version block (bunkerd 0.1.3, commit 4af949d, built, go version, platform) and exits 0 without binding. (3) README.md line 98 carries the 'Freshness check' note: if `bunker --version`'s `commit:` field doesn't match `git rev-parse HEAD`, the binary is stale — rebuild with `make build` (added in commit ef09713).

## Summary

Judge Result: GAP-058

Tier 2 (Agentic Evaluator): COMPLETE
  ✓ PASS: ~/go/bin/bunker --version shows commit >= c578969 (not b7d3bb6); ~/go/bin/bunkerd --version prints the version block and exits 0 without binding; README quickstart carries a binary-commit-vs-HEAD freshness note.: (1) `~/go/bin/bunker --version` outputs commit 4af949d, built 2026-08-27T18:39:44Z, exit 0; git log confirms 4af949d is a descendant of c578969 (which is a descendant of b7d3bb6), so commit >= c578969 and not b7d3bb6. (2) `~/go/bin/bunkerd --version` prints the full version block (bunkerd 0.1.3, commit 4af949d, built, go version, platform) and exits 0 without binding. (3) README.md line 98 carries the 'Freshness check' note: if `bunker --version`'s `commit:` field doesn't match `git rev-parse HEAD`, the binary is stale — rebuild with `make build` (added in commit ef09713).

Overall: PASS ✓
