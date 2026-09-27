//go:build linux

package fsmount

import (
	"context"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// The BFS-030 fix adds exactly one thing to the mount's work: the predicate
// `writeIntentOn`, consulted on a size-carrying Setattr. These benchmarks put a
// NUMBER on that new work, so the cost of the rule is not an adjective.
//
// What is NOT measured here is the successful path — because the rule does not
// touch it: no Setattr is issued for a create, and the write/publish path is
// unchanged. The live cost measurement for the successful shape is
// docs/evidence/BFS-030-cost-*.txt (200 files per arm, before/after).

func BenchmarkWriteIntentOn(b *testing.B) {
	m, _ := testMount(b, boundTestShort)
	nd := &node{m: m, p: "target.txt"}
	if _, _, errno := nd.Open(context.Background(), syscall.O_WRONLY); errno != 0 {
		b.Fatalf("open: errno=%v", errno)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !m.writeIntentOn("target.txt") {
			b.Fatal("the handle must be seen")
		}
	}
}

// BenchmarkRefusedResize is the expensive half of the predicate: a Setattr that
// carries a size, arrives while a write handle is open, and is refused without
// touching the network.
func BenchmarkRefusedResize(b *testing.B) {
	m, _ := testMount(b, boundTestShort)
	nd := &node{m: m, p: "target.txt"}
	if _, _, errno := nd.Open(context.Background(), syscall.O_RDWR); errno != 0 {
		b.Fatalf("open: errno=%v", errno)
	}
	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_SIZE
	in.Size = 0
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out fuse.AttrOut
		if errno := nd.Setattr(context.Background(), nil, in, &out); errno != syscall.EOPNOTSUPP {
			b.Fatalf("want EOPNOTSUPP, got %v", errno)
		}
	}
}
