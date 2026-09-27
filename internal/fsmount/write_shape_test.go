//go:build linux

package fsmount

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/deployBunker/bunker/internal/fsclient"
)

// BFS-030: a FAILED write must leave the ORIGINAL content intact.
//
// The defect these arms pin, measured on the unfixed tree
// (docs/evidence/BFS-030-red.txt): `open(path,'wb')` on an EXISTING file made
// the kernel send SETATTR(size=0), the mount published that resize as ONE
// conditional PUT of zero bytes, the file was EMPTY on the server and through
// the mount, and the write that followed failed with EOPNOTSUPP. The caller got
// an error AND lost their content — the worst pairing, and the write-side twin
// of BFS-025's read-side silent truncation.
//
// The rule these tests pin is the fix's whole content: a resize that arrives
// while a handle opened for WRITING is live on the path is refused BEFORE
// anything is read or published, because such a resize is the destructive half
// of an in-place rewrite this surface cannot complete (Open hands back a read
// handle whatever the open flags say, which is why the write half can never
// land). These are handler-level arms, driven through the same objects a live
// mount wires together; the live arm that goes RED on the unfixed tree is
// docs/evidence/BFS-030-probes/bfs030-arms.py.

// openThroughMount performs what the kernel does before a write: Lookup is
// answered from the fixture, then Open with the caller's flags. It returns the
// handle the mount handed back.
func openThroughMount(t *testing.T, m *Mount, p string, flags uint32) fs.FileHandle {
	t.Helper()
	nd := &node{m: m, p: p}
	fh, _, errno := nd.Open(context.Background(), flags)
	if errno != 0 {
		t.Fatalf("Open(%s, %#x): errno=%v", p, flags, errno)
	}
	return fh
}

// setattrSize is what the kernel sends for an O_TRUNC open or an ftruncate: a
// size-carrying Setattr. fh is the handle the request named, exactly as the
// kernel sends it — measured as 0 (no handle) for the O_TRUNC open, which is why
// the mount's rule cannot rest on it. See writeIntentOn.
func setattrSize(t *testing.T, m *Mount, p string, fh fs.FileHandle, size uint64) syscall.Errno {
	t.Helper()
	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_SIZE
	in.Size = size
	var out fuse.AttrOut
	return (&node{m: m, p: p}).Setattr(context.Background(), fh, in, &out)
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestResizeThroughAWriteHandleIsRefusedBeforeItPublishes is THE arm: the shape
// that destroyed data, refused instead, with the original byte-identical and
// nothing at all sent to the server.
func TestResizeThroughAWriteHandleIsRefusedBeforeItPublishes(t *testing.T) {
	m, target := testMount(t, boundTestShort)
	before := fileBytes(t, target)

	// `open(path,'wb')` on an existing file: the write-intent open the kernel
	// makes, with O_TRUNC in the flags (the kernel turns that half into the
	// SETATTR below).
	h := openThroughMount(t, m, "target.txt", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC)

	// The premise the whole rule rests on, pinned as a fact rather than a
	// comment: the handle this surface hands back cannot accept a write, so any
	// resize published here would be half of an operation that can never
	// complete.
	if _, isWriter := h.(fs.FileWriter); isWriter {
		t.Fatal("an existing path must not be openable for writing (no write handle exists for it): the refusal below is only correct because the write half cannot land")
	}

	// THE ARM. The kernel's O_TRUNC half, with the handle it named (measured:
	// none — Fh 0, which is exactly why the predicate is the live handle table).
	requestsBefore := m.client.Requests()
	errno := setattrSize(t, m, "target.txt", h, 0)

	// A failed write must leave the original intact. Anything other than a loud
	// refusal here is data loss.
	if errno != fsclient.ErrnoEOPNOTSUPP {
		t.Fatalf("want a loud EOPNOTSUPP refusal, got errno=%v", errno)
	}
	if got := fileBytes(t, target); string(got) != string(before) {
		t.Fatalf("THE ORIGINAL MUST SURVIVE: server content %q, want %q (a failed write destroyed the target)", got, before)
	}
	// Refused BEFORE it published: not one request left the client — no GET to
	// read the content it would have republished, no PUT to land the empty body.
	if got := m.client.Requests(); got != requestsBefore {
		t.Fatalf("the refusal must publish nothing and read nothing, but the client made %d request(s)", got-requestsBefore)
	}
	// And it is reported, not silent.
	st := m.Status()
	if st.WriteShape.RefusalsTotal != 1 {
		t.Fatalf("the refusal must be counted on the owner-facing surface, got %d", st.WriteShape.RefusalsTotal)
	}
	if !strings.Contains(st.WriteShape.Last, "target.txt") {
		t.Fatalf("the status document must name the path that was refused, got %q", st.WriteShape.Last)
	}
}

// TestFtruncateWithAWriteHandleIsRefusedToo is the second measured hole of the
// same family (docs/evidence/BFS-030-trace.txt, shape b): an in-place
// handle truncating the file to zero EMPTIED it on the unfixed tree, and the
// write that follows fails for the same reason. The rule covers it because the
// predicate is the live write handle, not the O_TRUNC flag.
func TestFtruncateWithAWriteHandleIsRefusedToo(t *testing.T) {
	m, target := testMount(t, boundTestShort)
	before := fileBytes(t, target)

	h := openThroughMount(t, m, "target.txt", syscall.O_RDWR) // no O_TRUNC: `r+b`
	if errno := setattrSize(t, m, "target.txt", h, 0); errno != fsclient.ErrnoEOPNOTSUPP {
		t.Fatalf("want a loud EOPNOTSUPP refusal, got errno=%v", errno)
	}
	if got := fileBytes(t, target); string(got) != string(before) {
		t.Fatalf("an in-place ftruncate must not destroy the target: got %q, want %q", got, before)
	}
	if got := m.Status().WriteShape.RefusalsTotal; got != 1 {
		t.Fatalf("the refusal must be counted, got %d", got)
	}
}

// TestDeliberateResizeWithNoWriteHandleStillPublishes is the control that keeps
// the fix off the one supported Setattr: `truncate -s N file` (a path-based
// resize, no handle open) reads the content, publishes exactly N bytes and
// succeeds — the behaviour BFS-012's truncate measurements depend on, unchanged.
func TestDeliberateResizeWithNoWriteHandleStillPublishes(t *testing.T) {
	m, target := testMount(t, boundTestLong)

	if errno := setattrSize(t, m, "target.txt", nil, 32); errno != 0 {
		t.Fatalf("a deliberate resize must still succeed, got errno=%v", errno)
	}
	got := fileBytes(t, target)
	if len(got) != 32 {
		t.Fatalf("the server must hold 32 bytes, got %d", len(got))
	}
	if string(got) != boundTestLong[:32] {
		t.Fatalf("the resize must keep the leading bytes: got %q", got)
	}
	if n := m.Status().WriteShape.RefusalsTotal; n != 0 {
		t.Fatalf("a resize with no write handle open must not be refused, got %d refusals", n)
	}
}

// TestReadsThroughAWriteHandleAreUnaffected: the shape that still works — an
// in-place handle can be READ — must keep working, or the refusal would be
// buying safety with a capability nobody asked to lose.
func TestReadsThroughAWriteHandleAreUnaffected(t *testing.T) {
	m, _ := testMount(t, boundTestShort)
	h := openThroughMount(t, m, "target.txt", syscall.O_RDWR)
	rh, ok := h.(*readHandle)
	if !ok {
		t.Fatalf("want the read handle for an in-place open, got %T", h)
	}
	res, errno := rh.Read(context.Background(), make([]byte, 4096), 0)
	if errno != 0 || res == nil {
		t.Fatalf("a read through a writable handle must be served: errno=%v", errno)
	}
	if res.Size() != len(boundTestShort) {
		t.Fatalf("served %d bytes, want %d", res.Size(), len(boundTestShort))
	}
}

// TestResizeIsAllowedAgainOnceTheWriteHandleIsReleased: the predicate is handle
// LIVENESS, not a per-path mark that could stick. A handle the kernel closed
// must not keep blocking deliberate resizes, or an abandoned open would wedge the
// path until a remount.
func TestResizeIsAllowedAgainOnceTheWriteHandleIsReleased(t *testing.T) {
	m, target := testMount(t, boundTestLong)

	h := openThroughMount(t, m, "target.txt", syscall.O_WRONLY)
	if errno := setattrSize(t, m, "target.txt", h, 0); errno != fsclient.ErrnoEOPNOTSUPP {
		t.Fatalf("while the handle is live the resize must be refused, got errno=%v", errno)
	}
	rh, ok := h.(*readHandle)
	if !ok {
		t.Fatalf("want the read handle, got %T", h)
	}
	if errno := rh.Release(context.Background()); errno != 0 {
		t.Fatalf("release: errno=%v", errno)
	}
	if errno := setattrSize(t, m, "target.txt", nil, 32); errno != 0 {
		t.Fatalf("after the handle is released a deliberate resize must succeed, got errno=%v", errno)
	}
	if got := fileBytes(t, target); len(got) != 32 {
		t.Fatalf("the server must hold 32 bytes after the released-handle resize, got %d", len(got))
	}
}

// TestWriteIntentFollowsTheOpenFlags is the predicate's table: the mount's own
// handle table says "this path is open for writing" exactly for the opens the
// kernel will follow with a write or a resize, and never for a pure read.
func TestWriteIntentFollowsTheOpenFlags(t *testing.T) {
	cases := []struct {
		name  string
		flags uint32
		want  bool
	}{
		{"read", syscall.O_RDONLY, false},
		{"write", syscall.O_WRONLY, true},
		{"read-write", syscall.O_RDWR, true},
		{"truncating write", syscall.O_WRONLY | syscall.O_TRUNC, true},
		{"append", syscall.O_WRONLY | syscall.O_APPEND, true},
		{"create-read", syscall.O_RDONLY | syscall.O_CREAT, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := testMount(t, boundTestShort)
			h := openThroughMount(t, m, "target.txt", tc.flags)
			if got := m.writeIntentOn("target.txt"); got != tc.want {
				t.Fatalf("writeIntentOn after Open(%#x) = %v, want %v", tc.flags, got, tc.want)
			}
			if got := m.writeIntentOn("some/other.txt"); got {
				t.Fatal("a write handle on one path must not mark another path")
			}
			if rh, ok := h.(*readHandle); ok {
				if errno := rh.Release(context.Background()); errno != 0 {
					t.Fatalf("release: errno=%v", errno)
				}
			}
			if m.writeIntentOn("target.txt") {
				t.Fatal("a released handle must not keep the path marked as open for writing")
			}
		})
	}
}

// TestRefusedResizeDoesNotPoisonTheTransportVerdict: the refusal is per file and
// local — the tree and the transport are fine — so it must not make the mount
// report itself unreachable. (The same distinction BFS-025's read refusal makes.)
func TestRefusedResizeDoesNotPoisonTheTransportVerdict(t *testing.T) {
	m, _ := testMount(t, boundTestShort)
	h := openThroughMount(t, m, "target.txt", syscall.O_WRONLY)
	if errno := setattrSize(t, m, "target.txt", h, 0); errno != fsclient.ErrnoEOPNOTSUPP {
		t.Fatalf("want the refusal, got errno=%v", errno)
	}
	if got := m.Status().Transport.Verdict; got != "healthy" {
		t.Fatalf("a local write-shape refusal must leave the transport verdict alone, got %q", got)
	}
}
