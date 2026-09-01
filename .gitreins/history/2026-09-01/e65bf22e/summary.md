# Verdict: GAP-060

**Task:** bunker metrics can still silently report HOST memory (host-level fallback flag)
**Evaluated:** 2026-09-01T00:49:40.391016
**Result:** ✓ PASS

## Criteria

- ✓ **PASS: bunker metrics on a missing/degraded agent prints an explicit host-level fallback notice (or daemon log warns); host_level_fallback field present in regenerated bunker.pb.go and wired through service.go and cli/metrics.go; go build/vet/test all pass**
  - All elements verified in commit 61dafd2. (1) Explicit notice: internal/cli/metrics.go:171 prints 'NOTE: host-level fallback (agent cgroup unavailable — metrics are HOST values, not agent values)' when msg.HostLevelFallback is true; daemon warns at internal/server/service.go:244,249 ('agent user lookup failed; metrics will fall back to host level'). (2) host_level_fallback field present in regenerated proto/bunker/v1/bunker.pb.go:1191 (field 10, protobuf tag host_level_fallback). (3) Wired through service.go:257,266 (resp.HostLevelFallback set from cgroup metrics or true on host fallback) and cli/metrics.go:171. (4) go build ./... exit 0, go vet ./... exit 0, go test ./... -count=1 -timeout 120s exit 0 (all packages ok). New tests pass: TestMetricsCommand_AgentMetrics_HostFallbackNotice (cli), TestReadAgentCgroupMetrics_AgentPathWins/FallbackToHost/ZeroWhenBothUnreadable/MaxLimitHybrid (resource), TestAgentMetrics_HostFallbackWhenAgentUserAbsent (server).

## Summary

Judge Result: GAP-060

Tier 2 (Agentic Evaluator): COMPLETE
  ✓ PASS: bunker metrics on a missing/degraded agent prints an explicit host-level fallback notice (or daemon log warns); host_level_fallback field present in regenerated bunker.pb.go and wired through service.go and cli/metrics.go; go build/vet/test all pass: All elements verified in commit 61dafd2. (1) Explicit notice: internal/cli/metrics.go:171 prints 'NOTE: host-level fallback (agent cgroup unavailable — metrics are HOST values, not agent values)' when msg.HostLevelFallback is true; daemon warns at internal/server/service.go:244,249 ('agent user lookup failed; metrics will fall back to host level'). (2) host_level_fallback field present in regenerated proto/bunker/v1/bunker.pb.go:1191 (field 10, protobuf tag host_level_fallback). (3) Wired through service.go:257,266 (resp.HostLevelFallback set from cgroup metrics or true on host fallback) and cli/metrics.go:171. (4) go build ./... exit 0, go vet ./... exit 0, go test ./... -count=1 -timeout 120s exit 0 (all packages ok). New tests pass: TestMetricsCommand_AgentMetrics_HostFallbackNotice (cli), TestReadAgentCgroupMetrics_AgentPathWins/FallbackToHost/ZeroWhenBothUnreadable/MaxLimitHybrid (resource), TestAgentMetrics_HostFallbackWhenAgentUserAbsent (server).

Overall: PASS ✓
