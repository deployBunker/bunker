//go:build linux

package fsmount

import (
	"bytes"
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// DF-BUNKER-100: a refused write shape is NAMED, COUNTED and EXPLAINED.
//
// Before this row the in-place write path returned a bare `syscall.EOPNOTSUPP`,
// so the only thing anyone could learn from a failed write inside a mount was
// "Operation not supported" — no cause, no count, no mention of the shape that
// does work. That is the message dogfood run 27 hit with a shell redirect and
// with `git init` (docs/dogfood/2026-10-16b-bunker-fs-surface.md).
//
// The cells below pin both halves of the fix and one NEGATIVE control: the
// errno the kernel sees is unchanged (the wire behaviour must not move), the
// refusal now reaches the owner-facing status document with the path that was
// refused, the message carries the contract and the remediation, and a refusal
// that is NOT a write shape stays exactly as it was.

// TestInPlaceWriteIsANamedWriteShapeRefusal is the write half: `r+b` (or `>>`'s
// handle without the append state) opened on an EXISTING path, written through
// — the shape every in-place edit and every git write arrives through.
func TestInPlaceWriteIsANamedWriteShapeRefusal(t *testing.T) {
	m, target := testMount(t, boundTestShort)
	before := fileBytes(t, target)

	h := openThroughMount(t, m, "target.txt", syscall.O_RDWR)
	beforeReqs := m.client.Requests()

	n, errno := (&node{m: m, p: "target.txt"}).Write(context.Background(), h, []byte("XX"), 0)
	if errno != syscall.EOPNOTSUPP {
		t.Fatalf("an in-place write must still be refused EOPNOTSUPP (the kernel-visible behaviour is unchanged), got errno=%v", errno)
	}
	if n != 0 {
		t.Fatalf("a refused write must acknowledge no bytes, got %d", n)
	}
	if delta := m.client.Requests() - beforeReqs; delta != 0 {
		t.Fatalf("the refusal must be local (no request left the client), so %d request(s) means the write was attempted somewhere", delta)
	}
	if got := fileBytes(t, target); !bytes.Equal(got, before) {
		t.Fatalf("a refused in-place write changed the target: %q", got)
	}

	st := m.Status()
	if st.WriteShape.RefusalsTotal != 1 {
		t.Errorf("the refusal must be counted on the owner-facing surface, got %d", st.WriteShape.RefusalsTotal)
	}
	if !strings.Contains(st.WriteShape.Last, "target.txt") || !strings.Contains(st.WriteShape.Last, "write off=0") {
		t.Errorf("the status document must name the path and the offset that were refused, got %q", st.WriteShape.Last)
	}
	// FUSE carries no text back to the caller, so the explanation has to land
	// where the owner can read it: the recorded cause.
	if !strings.Contains(st.Transport.Cause, "write_shape_unsupported") {
		t.Errorf("the recorded cause must name the refusal class, got %q", st.Transport.Cause)
	}
	if !strings.Contains(st.Transport.Cause, "WHOLE FILES only") {
		t.Errorf("the recorded cause must carry the write-shape contract and its remediation, got %q", st.Transport.Cause)
	}
}

// TestInPlaceWriteRefusalNamesTheShapeThatWorks: the message must say what to do
// instead — a refusal whose only content is "not supported" is the defect.
func TestInPlaceWriteRefusalNamesTheShapeThatWorks(t *testing.T) {
	m, _ := testMount(t, boundTestShort)
	h := openThroughMount(t, m, "target.txt", syscall.O_RDWR)
	if _, errno := (&node{m: m, p: "target.txt"}).Write(context.Background(), h, []byte("XX"), 0); errno != syscall.EOPNOTSUPP {
		t.Fatalf("errno=%v, want EOPNOTSUPP", errno)
	}
	got := m.Status().Transport.Cause
	for _, want := range []string{"whole-file", "cp, tee, editor save", "git"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal the owner can read does not mention %q; it must name the contract and the remediation, got %q", want, got)
		}
	}
}

// TestModeChangeRefusalKeepsTheBareErrnoAndTheWriteShapeCount is the deliberate
// boundary of this row: `chmod` (the syscall git dies on) is NOT a write shape,
// so it keeps its errno and does not inflate the write_shape counters — what it
// gets is the explanation in the mount log, because nothing else can carry text
// back through FUSE.
func TestModeChangeRefusalKeepsTheBareErrnoAndTheWriteShapeCount(t *testing.T) {
	m, _ := testMount(t, boundTestShort)

	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_MODE
	in.Mode = 0o755
	var out fuse.AttrOut
	if errno := (&node{m: m, p: "target.txt"}).Setattr(context.Background(), nil, in, &out); errno != syscall.EOPNOTSUPP {
		t.Fatalf("a mode change must be refused EOPNOTSUPP (this is the syscall git fails on), got errno=%v", errno)
	}
	if got := m.Status().WriteShape.RefusalsTotal; got != 0 {
		t.Errorf("a metadata refusal is not a write-shape refusal; the write_shape count must stay 0, got %d", got)
	}
}

// TestWriteShapeRefusalRecordedCauseMatchesTheClientContract keeps the mount and
// the client from drifting: the sentence the mount records is the one the
// client's own refusal text carries, so the two surfaces cannot say different
// things about the same refusal.
func TestWriteShapeRefusalRecordedCauseMatchesTheClientContract(t *testing.T) {
	m, _ := testMount(t, boundTestShort)
	h := openThroughMount(t, m, "target.txt", syscall.O_RDWR)
	if _, errno := (&node{m: m, p: "target.txt"}).Write(context.Background(), h, []byte("XX"), 0); errno != syscall.EOPNOTSUPP {
		t.Fatalf("errno=%v, want EOPNOTSUPP", errno)
	}
	if got := m.Status().Transport.Cause; !strings.HasSuffix(got, "["+fsclient.WriteShapeNotice+"]") {
		t.Errorf("the recorded cause must end with the client contract's compact form, got %q", got)
	}
}
