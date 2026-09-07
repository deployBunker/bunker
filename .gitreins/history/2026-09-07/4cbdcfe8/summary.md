# Verdict: DF-BUNKER-2

**Task:** [dogfood:P1] specs/api.md REST table is wrong: GET endpoints return 405, API is POST-only
**Evaluated:** 2026-09-07T04:25:45.615916
**Result:** ✓ PASS

## Criteria

- ✓ **specs/api.md REST Mapping table lists every RPC as POST (no GET entries); a note explains connect-go serves all RPCs over POST only (405 on GET, 401 on unauthenticated POST); PASS: grep specs/api.md for 'GET' in the REST Mapping table returns 0 matches and the table lists all 10 Bunkerd RPCs as POST**
  - specs/api.md lines 314-355: REST Mapping table lists all Bunkerd RPCs (ServerInfo, ServerMetrics, SpawnAgent, DestroyAgent, ListAgents, GetAgent, AgentMetrics, ExecAgent, RunAgent, HeartbeatAgent, QueryAudit — 11, matching proto/bunker/v1/bunker.proto) and all Agent RPCs as POST. `grep -c GET` on table region (lines 314-355) returns 0 (exit 1); whole-file grep for GET also returns 0. Note at lines 344-351 states connect-go serves all RPCs over POST only, non-POST returns 405 Method Not Allowed, unauthenticated POST returns 401 Unauthenticated. Commit c1695c6 (docs(specs): DF-BUNKER-2) confirms GET rows changed to POST + note added. Documentation-only task — no test suite applicable.

## Summary

Judge Result: DF-BUNKER-2

Tier 2 (Agentic Evaluator): COMPLETE
  ✓ specs/api.md REST Mapping table lists every RPC as POST (no GET entries); a note explains connect-go serves all RPCs over POST only (405 on GET, 401 on unauthenticated POST); PASS: grep specs/api.md for 'GET' in the REST Mapping table returns 0 matches and the table lists all 10 Bunkerd RPCs as POST: specs/api.md lines 314-355: REST Mapping table lists all Bunkerd RPCs (ServerInfo, ServerMetrics, SpawnAgent, DestroyAgent, ListAgents, GetAgent, AgentMetrics, ExecAgent, RunAgent, HeartbeatAgent, QueryAudit — 11, matching proto/bunker/v1/bunker.proto) and all Agent RPCs as POST. `grep -c GET` on table region (lines 314-355) returns 0 (exit 1); whole-file grep for GET also returns 0. Note at lines 344-351 states connect-go serves all RPCs over POST only, non-POST returns 405 Method Not Allowed, unauthenticated POST returns 401 Unauthenticated. Commit c1695c6 (docs(specs): DF-BUNKER-2) confirms GET rows changed to POST + note added. Documentation-only task — no test suite applicable.

Overall: PASS ✓
