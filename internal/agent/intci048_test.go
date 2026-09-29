package agent

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// INT-CI-048 — the product side of "the auto-ID spawn printed its banner and
// returned no id".
//
// regression-tests.sh section 4b runs `bunker spawn` with NO agent id and
// parses the id out of the printed bundle. On CI run 36292076555 that cell
// reported an EMPTY id while the CLI's GAP-023 banner ("Creating agent...",
// printed before the RPC) was present. Two culprits were on the table — the
// cell's extractor or the CLI's auto-ID path — and neither is at fault: the
// daemon REFUSED the spawn because the shared runner already carried nine
// registered leftover agents (INT-CI-012 keeps them) against a scratch daemon
// configured for ten, so HasCapacity(1) was false at Step 1.5 and the request
// died BEFORE any side effect could create an agent. No agent => no id to
// print => the cell was right to fail and the CLI was right to print nothing
// but its banner.
//
// The regression suite no longer starves itself (its daemon is sized from the
// residue it replays) and its section-4b diagnostic now reports the whole
// captured output. This test pins the daemon-side contract both fixes depend
// on, including the detail that makes the refusal identifiable in a log:
//
//   - the refusal happens at the CAPACITY stage, before any side effect;
//   - the id it names is the one the manager GENERATED at Step 1 (the first
//     UUID segment), NOT spawnStageErr's "<unassigned>" placeholder — the
//     placeholder only appears when generation itself fails. A live log line
//     "spawn 9eb02c7c failed at stage capacity: …" is therefore proof that an
//     auto-ID spawn was refused after an id existed but before an agent did;
//   - the tracker is unchanged: nothing was created, so there is no agent and
//     no id for the CLI to print.
func TestSpawn_AutoIDPathCapacityRefusalNamesTheStage(t *testing.T) {
	m := newCoverageTestManager(1)
	if err := m.tracker.Register(&resource.AgentRecord{AgentID: "existing"}); err != nil {
		t.Fatalf("register fixture: %v", err)
	}

	// Empty AgentId: the auto-ID path the regression cell exercises.
	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{})
	if err == nil {
		t.Fatal("Spawn succeeded on a full tracker, want the capacity refusal")
	}

	// The whole refusal, with the generated 8-hex short id pinned by shape:
	// this is the evidence the CI log never carried, because the cell's
	// first-line-only diagnostic reported the progress banner instead.
	refusal := regexp.MustCompile(`^spawn [0-9a-f]{8} failed at stage capacity: capacity full: 1/1 agents$`)
	if !refusal.MatchString(err.Error()) {
		t.Errorf("error %q does not match the auto-ID capacity refusal %q", err, refusal.String())
	}
	// Substrings an operator greps for, kept explicit so a reworded cause or
	// stage still fails here rather than in a live run.
	for _, want := range []string{
		"capacity full: 1/1 agents", // both numbers: the tracker state
		"stage capacity",            // MUST NOT be port-alloc/useradd/dockerd
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
	// "<unassigned>" would mean the id was never generated — a different
	// failure (Step 1) and a different fix.
	if strings.Contains(err.Error(), "<unassigned>") {
		t.Errorf("error %q carries the placeholder id; an auto-ID capacity refusal names the GENERATED id", err)
	}

	// No side effect: the refusal happens before the tracker grows, so the
	// count stayed where the fixture put it and a later spawn against a
	// tracker with room is unaffected.
	if got := m.tracker.Count(); got != 1 {
		t.Errorf("tracker count = %d after a refused auto-ID spawn, want 1 (no agent may be created)", got)
	}
}
