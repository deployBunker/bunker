package fsclient

// ============================================================================
// BFS-039 CELL 1 — THE DELIBERATE CANCEL IS DISTINGUISHABLE FROM A FAILURE.
//
// THE DEFECT THIS CELL CATCHES, measured on the tree that landed BFS-046
// (docs/evidence/BFS-039-red.txt): a DELIBERATE cancel — what the kernel sends
// as a FUSE interrupt when the caller is killed, times out or is Ctrl-C'd — is
// classified by `classifyTransport` as a TRANSPORT FAULT, because a cancelled
// context surfaces as `context.Canceled` and NOTHING in this package asked
// whether the caller cancelled us. So the caller is told
// `unreachable_reset`/`ENOTCONN` for its own Ctrl-C: the same answer it gets
// when the far end dies mid-request, and a different answer from the one the
// recovery rule needs. PRD-bunker-invalidation §2.8 / R10: "cancelled must be
// distinguishable from failed in the error returned to the kernel (`EINTR` vs
// `EIO`), or callers cannot retry correctly".
//
// WHY THE VALUES ARE LITERALS HERE. This cell must run UNCHANGED on the tree
// before the fix and on the tree after it, or its red is a compile error rather
// than a measurement. `EINTR` is 4 and the cancel cause is "cancelled" on both
// sides; the named constants (`ErrnoEINTR`, `CauseCancelled`) are what the fix
// adds, and the assertions below are written so that the SAME text goes red on
// the pre-fix tree and green on the post-fix tree.
// ============================================================================

import (
	"context"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

// The two values this row's fix names, as literals so this file compiles on the
// pre-fix tree too.
const (
	bfs039EINTR        = syscall.Errno(4)
	bfs039CancelCause  = Cause("cancelled")
	bfs039CancelDetail = "the operation was cancelled by the caller (EINTR): nothing was refused, retry it"
)

// bfs039StallingServer answers only once the test releases it, so the test can
// cancel a request that is genuinely in flight.
func bfs039StallingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("X-Bunker-Tree", "tree-measure")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

func bfs039Client(t *testing.T, base string) *Client {
	t.Helper()
	c, err := NewClient(Options{BaseURL: base + "/dav", Concurrency: 4, OpTimeout: 30 * time.Second, BindTimeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// TestBFS039DeliberateCancelIsDistinguishableFromFailure is the cell. Three
// arms, one judgement: the CANCEL arm must report the cancel class and errno,
// and neither FAILURE arm may.
func TestBFS039DeliberateCancelIsDistinguishableFromFailure(t *testing.T) {
	// --- ARM 1: the DELIBERATE cancel, mid-flight. ---
	srv := bfs039StallingServer(t)
	c := bfs039Client(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *OpError, 1)
	go func() {
		_, err := c.Head(ctx, "src/main.go")
		done <- err
	}()
	time.Sleep(250 * time.Millisecond) // in flight, not yet answered
	cancel()
	cancelErr := <-done
	if cancelErr == nil {
		t.Fatal("a cancelled request reported success")
	}
	t.Logf("BFS039-MEASURE arm=deliberate-cancel errno=%s(%d) cause=%q detail=%q",
		ErrnoName(cancelErr.Errno), int(cancelErr.Errno), cancelErr.Cause, cancelErr.Detail)

	// --- ARM 2: a real SERVER FAILURE (a 500). ---
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	_, failErr := bfs039Client(t, failing.URL).Head(context.Background(), "src/main.go")
	if failErr == nil {
		t.Fatal("a 500 reported success")
	}
	t.Logf("BFS039-MEASURE arm=server-500 errno=%s(%d) cause=%q",
		ErrnoName(failErr.Errno), int(failErr.Errno), failErr.Cause)

	// --- ARM 3: a transport reset (the far end vanished mid-request). ---
	killed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker: the reset arm cannot be driven")
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer killed.Close()
	_, resetErr := bfs039Client(t, killed.URL).Head(context.Background(), "src/main.go")
	if resetErr == nil {
		t.Fatal("a reset connection reported success")
	}
	t.Logf("BFS039-MEASURE arm=transport-reset errno=%s(%d) cause=%q",
		ErrnoName(resetErr.Errno), int(resetErr.Errno), resetErr.Cause)

	// THE JUDGEMENT. The cancel arm is the only one the caller may retry on the
	// strength of the errno, so it must carry the cancel class AND the cancel
	// cause, and it must not be either failure class.
	if cancelErr.Errno != bfs039EINTR {
		t.Errorf("a DELIBERATE cancel reported errno=%s (%d), want EINTR (%d): the caller's own Ctrl-C is being reported as a transport fault, so it cannot tell a cancel from a fault and cannot retry correctly (PRD §2.8)",
			ErrnoName(cancelErr.Errno), int(cancelErr.Errno), int(bfs039EINTR))
	}
	if cancelErr.Cause != bfs039CancelCause {
		t.Errorf("a DELIBERATE cancel reported cause=%q, want %q", cancelErr.Cause, bfs039CancelCause)
	}
	if cancelErr.Errno == ErrnoEIO {
		t.Error("a deliberate cancel is reported as EIO: the two events the PRD separates are collapsed into one answer")
	}
	for name, e := range map[string]*OpError{"server-500": failErr, "transport-reset": resetErr} {
		if e.Errno == bfs039EINTR || e.Cause == bfs039CancelCause {
			t.Errorf("the %s arm was reported as a CANCELLED operation (%s/%s): a failure must never look like a cancel, and the recovery for the two is different", name, ErrnoName(e.Errno), e.Cause)
		}
	}
}
