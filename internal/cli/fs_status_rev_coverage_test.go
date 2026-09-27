package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// BFS-048, user-visible half: the coverage report must reach the screen a person
// reads, not only the JSON. `bunker fs status` is where a mount's invalidation
// state is read, so a git-tree mount answered by the last-resort revision poll
// must print what that poll can and cannot vouch for — and a mount answered by
// another mechanism must print exactly what it printed before this row.
func TestFSStatusPrintsTheRevisionCoverageWhenTheRevTierIsInForce(t *testing.T) {
	base := func() *fsclient.Status {
		st := &fsclient.Status{
			Mount:      "m1",
			Mode:       "poll",
			Endpoint:   "http://127.0.0.1:8443/dav",
			Mountpoint: "/home/agent/tree",
		}
		st.Invalidation = fsclient.InvalidationState{Mode: "poll", Mechanism: "rev", Seq: 12}
		return st
	}

	rev := base()
	rev.Invalidation.RevKind = fsclient.RevKindGit
	rev.Invalidation.RevVouchesFor = fsclient.RevVouchesForCommits
	rev.Invalidation.RevGap = fsclient.RevGapUncommittedWrites
	var buf bytes.Buffer
	printStatus(&buf, rev)
	out := buf.String()
	t.Logf("BFS-048 status line: %s", coverageLine(out))
	if !strings.Contains(out, "kind=git vouches_for=commits gap=uncommitted_working_tree_writes") {
		t.Fatalf("the status output does not report the revision's coverage: %q", out)
	}

	// Non-vacuity in the other direction: with no revision declared (the mount
	// is answered by the per-path poll, or by the push stream) the output must
	// be exactly what it was before — no stray "unknown"/"none" claim either.
	plain := base()
	plain.Invalidation.Mechanism = fsclient.MechanismEvents
	var plainBuf bytes.Buffer
	printStatus(&plainBuf, plain)
	if strings.Contains(plainBuf.String(), "rev coverage") {
		t.Fatalf("a non-rev mount printed a revision coverage line: %q", plainBuf.String())
	}
	if strings.Contains(plainBuf.String(), "gap=") {
		t.Fatalf("a non-rev mount printed a gap: %q", plainBuf.String())
	}
}

// coverageLine returns the rev coverage line of a rendered status, so the test
// log (and the evidence file) carry the same text a person would see.
func coverageLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "rev coverage") {
			return line
		}
	}
	return "(none)"
}
