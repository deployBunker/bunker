ch:trace row=REVIEW-BUNKER-1 spec=none:bunker-ac-clause wave=bunker-foreman-2026-10-22-13-19-40#REVIEW-BUNKER-1 test=internal/server/service_test.go::TestDestroyAgent_RefusalStatuses doc=docs/evidence/REVIEW-BUNKER-1-evidence.md prompt=none:foreman-inline evidence=58fb6b57b3f298adbfb931bfe75d0b9ce84b3b34 witness="--- PASS: TestDestroyAgent_RefusalStatuses (0.00s); ok github.com/deployBunker/bunker/internal/server" verdict=none:pending-foreman-trace commit=58fb6b57b3f298adbfb931bfe75d0b9ce84b3b34 memory=none:tick-scoped
STATUS: complete
TIER: T2 (invoked: go build ./... PASS; go vet ./... PASS; go test ./internal/server ./internal/agent ./internal/cli -count=1 all ok; observed: internal/server 17.332s ok, internal/agent 149.224s ok, internal/cli 36.717s ok; new test 5/5 subtests PASS incl. live_processes/home_retained -> CodeFailedPrecondition, not_found -> CodeNotFound, unnamed -> CodeInternal; GitReins Tier 1 guards PASS on commit)
EVIDENCE: commit 58fb6b57b3f298adbfb931bfe75d0b9ce84b3b34 (internal/server/service.go DestroyAgent refusal-status mapping + internal/server/service_test.go TestDestroyAgent_RefusalStatuses; gates: build/vet/tests green).

Scope note: this commit implements the CODE leg of REVIEW-BUNKER-1 only (named
state, never internal). Leak reaping on the live host (6 leaked agents) is the
foreman's separate evidence step - no live host was touched.