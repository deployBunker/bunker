package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// BFS-063, user-visible half: the resume declaration must reach the screen a
// person reads, not only the JSON record. `bunker fs status` is where a mount's
// invalidation state is read, and the difference between "my view is the tree as
// of this cursor" and "I hold no observation, so the server is telling me it
// cannot vouch for the interval" is exactly the difference this row exists to
// make visible.
//
// The mechanism gate is asserted, not assumed: the declaration belongs to the
// events poll, so a mount whose mechanism is the revision poll (or the push
// stream, once it is served) must print exactly what it printed before.
func TestFSStatusPrintsTheResumeDeclarationForTheEventsTier(t *testing.T) {
	base := func() *fsclient.Status {
		st := &fsclient.Status{
			Mount:      "m1",
			Mode:       "poll",
			Endpoint:   "http://127.0.0.1:8443/dav",
			Mountpoint: "/home/agent/tree",
		}
		st.Invalidation = fsclient.InvalidationState{Mode: "poll", Mechanism: fsclient.MechanismEvents, Seq: 7}
		return st
	}

	held := base()
	cursor := int64(7)
	held.Invalidation.ResumeSeq = &cursor
	var buf bytes.Buffer
	printStatus(&buf, held)
	if !strings.Contains(buf.String(), "resume       : seq=7") {
		t.Fatalf("a mount that holds an observation does not report its resume cursor: %q", buf.String())
	}

	none := base()
	none.Invalidation.ResumeSeq = nil
	var noneBuf bytes.Buffer
	printStatus(&noneBuf, none)
	if !strings.Contains(noneBuf.String(), "holds no observation") {
		t.Fatalf("a mount that holds no observation reports nothing: %q", noneBuf.String())
	}
	if strings.Contains(noneBuf.String(), "resume       : seq=") {
		t.Fatalf("a mount that holds no observation printed a cursor: %q", noneBuf.String())
	}

	// Non-vacuity in the other direction: another mechanism prints neither line.
	rev := base()
	rev.Invalidation.Mechanism = fsclient.MechanismRev
	rev.Invalidation.ResumeSeq = nil
	var revBuf bytes.Buffer
	printStatus(&revBuf, rev)
	if strings.Contains(revBuf.String(), "resume") {
		t.Fatalf("a revision-tier mount printed a resume line: %q", revBuf.String())
	}
}
