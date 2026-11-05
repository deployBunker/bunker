package fsclient

import "testing"

// INT-CI-088: beginFetch's (nil, false) is the SHUTDOWN sentinel — its bool
// means "not busy", never "you own a fresh fetch". launch must not treat a
// nil handle as a fetch it can end and complete: the pre-fix code fell into
// the close-after-begin branch and called endFetch(nil), which dereferenced
// h.sf[f.path] and SIGSEGV'd the test binary (CI run 42070219898).
//
// The state is reached deterministically: Close sets closed=true permanently
// (Close is idempotent and Resume refuses a closed manager), and it never
// nils baseCtx, so a launch issued after Close observes exactly the state the
// production race observes when shutdown lands between onTick's closed check
// and launch's beginFetch. No sleeps, no network: the fetch is never begun,
// so no server request can exist.
func TestLaunchAfterCloseNilFetchHandle(t *testing.T) {
	s := newHotSetup(t, newHotStub(t))
	if err := s.Manager.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Before the guard this line panicked inside endFetch's nil dereference.
	got := s.Manager.launch(&hotItem{path: "int-ci-088-shutdown.txt"})
	if got {
		t.Fatal("launch after Close must return false: the tick must stop scheduling")
	}
	// Nothing was begun, so no fetch record may have leaked into the
	// singleflight map (PR-8: entries return to 0 at rest).
	if n := s.Manager.SingleflightEntries(); n != 0 {
		t.Fatalf("singleflight entries after shutdown launch = %d, want 0", n)
	}
}
