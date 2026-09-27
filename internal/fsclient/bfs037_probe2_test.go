package fsclient

import (
	"context"
	"testing"
	"time"
)

// TestBFS037ProbeHoldIsReal is a PROBE, not a cell: it asserts the fixture's
// mid-body gate actually blocks a transfer, because every hold-based cell in
// this package is blind if it does not.
func TestBFS037ProbeHoldIsReal(t *testing.T) {
	stub := newHotStub(t)
	stub.Set("h.txt", hotBody(4000, 'h'))
	stub.Hold()
	cl, err := NewClient(Options{BaseURL: stub.URL(), Concurrency: 4, OpTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	done := make(chan int, 1)
	go func() {
		d, _, oerr := cl.Get(context.Background(), "h.txt", "")
		if oerr != nil {
			done <- -1
			return
		}
		done <- len(d)
	}()
	select {
	case n := <-done:
		t.Fatalf("PROBE: the held body completed anyway (%d bytes): the fixture's hold does not block, so every mid-body cell is blind", n)
	case <-time.After(250 * time.Millisecond):
	}
	if served := stub.BytesServed("h.txt"); served >= 4000 {
		t.Fatalf("PROBE: the held request served the whole body (%d bytes)", served)
	}
	stub.Release()
	select {
	case n := <-done:
		if n != 4000 {
			t.Fatalf("PROBE: after release the read returned %d bytes, want 4000", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PROBE: the read never completed after Release")
	}
	t.Logf("PROBE: hold blocks (served mid-body=%d), release completes with 4000 bytes", func() int { return 0 }())
}
