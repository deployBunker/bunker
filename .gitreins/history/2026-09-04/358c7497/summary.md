# Verdict: QA-BUNKER-4

**Task:** Port-range allocator leak: non-force destroy returns not_found after userdel failure WITHOUT freeing the range
**Evaluated:** 2026-09-04T23:02:29.164351
**Result:** ✓ PASS

## Criteria

- ✓ **Non-force Destroy of an agent whose userdel -rf fails must return Status=not_found AND return the agent's port range to the portAlloc pool (idempotent Free in the early-return path); force-mode destroy with failing userdel must also free; TTL-reaper path (expired agent with running process) must not leak a range; unit tests cover all three; go build/vet/test green; VERIFY-PASS battery evidence committed**
  - All sub-requirements verified. (1) Non-force userdel-fail path: internal/agent/manager_destroy.go !force branch calls m.portAlloc.Free(agentID) (idempotent, no-ops for unknown IDs per internal/resource/portalloc.go:87) before returning Status=not_found. (2) Force-mode: userdel failure logs & continues to bottom Free, returns destroyed. (3) TTL reaper (manager.go:77 reapExpiredAgents) calls Destroy(...,false) so routes through the non-force free path; waitAgentProcessesExit hardening added. Tests TestDestroy_UserdelFail_NotFound_FreesPortRange, TestDestroy_UserdelFail_Force_FreesPortRange, TestTTLReaper_UserdelFail_NoPortRangeLeak all PASS (go test ./internal/agent/ -run ... ok 0.125s). go build ./... exit 0; go vet ./... exit 0; go test ./... -count=1 all packages ok. VERIFY-PASS battery evidence committed in docs/dogfood/2026-09-04-qa-bunker-4-e2e.md ('STATUS: ALL CORE TESTS PASS / VERIFY-PASS'); .gitreins/tasks.yaml QA-BUNKER-4 status complete.

## Summary

Judge Result: QA-BUNKER-4

Tier 2 (Agentic Evaluator): COMPLETE
  ✓ Non-force Destroy of an agent whose userdel -rf fails must return Status=not_found AND return the agent's port range to the portAlloc pool (idempotent Free in the early-return path); force-mode destroy with failing userdel must also free; TTL-reaper path (expired agent with running process) must not leak a range; unit tests cover all three; go build/vet/test green; VERIFY-PASS battery evidence committed: All sub-requirements verified. (1) Non-force userdel-fail path: internal/agent/manager_destroy.go !force branch calls m.portAlloc.Free(agentID) (idempotent, no-ops for unknown IDs per internal/resource/portalloc.go:87) before returning Status=not_found. (2) Force-mode: userdel failure logs & continues to bottom Free, returns destroyed. (3) TTL reaper (manager.go:77 reapExpiredAgents) calls Destroy(...,false) so routes through the non-force free path; waitAgentProcessesExit hardening added. Tests TestDestroy_UserdelFail_NotFound_FreesPortRange, TestDestroy_UserdelFail_Force_FreesPortRange, TestTTLReaper_UserdelFail_NoPortRangeLeak all PASS (go test ./internal/agent/ -run ... ok 0.125s). go build ./... exit 0; go vet ./... exit 0; go test ./... -count=1 all packages ok. VERIFY-PASS battery evidence committed in docs/dogfood/2026-09-04-qa-bunker-4-e2e.md ('STATUS: ALL CORE TESTS PASS / VERIFY-PASS'); .gitreins/tasks.yaml QA-BUNKER-4 status complete.

Overall: PASS ✓
