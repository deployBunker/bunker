package fsclient

import (
	"fmt"
	"testing"
	"time"
)

// The BFS-033 enforcement adds exactly one thing to a publish: `checkHold`,
// consulted before the request. These benchmarks put a NUMBER on it, so the cost
// of the rule is not an adjective — the same discipline BFS-030's write-shape
// benchmarks follow.
//
// What is NOT measured here is the end-to-end successful write: the rule adds no
// request to it (the live cost arms measure that, and their request counts are
// byte-for-byte identical before and after).

// BenchmarkCheckHoldMiss is the SUCCESSFUL path's cost: a publish on a path with
// no refusal standing — one map lookup under the write path's own mutex.
func BenchmarkCheckHoldMiss(b *testing.B) {
	wp := NewWritePath(nil, nil, "", OnConflictRefuse)
	base := WriteBase{IfMatch: "sha256:aaaa", Source: BaseFromServed}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := wp.checkHold("src/no-refusal-here.txt", base); err != nil {
			b.Fatalf("a path with no refusal must publish: %v", err)
		}
	}
}

// BenchmarkCheckHoldMissContended is the same lookup with a refusal standing on
// ANOTHER path and a full hold map — the shape a busy mount actually has.
func BenchmarkCheckHoldMissContended(b *testing.B) {
	wp := NewWritePath(nil, nil, "", OnConflictRefuse)
	for i := 0; i < refusalHoldMax; i++ {
		wp.armHold(Conflict{Path: holdPath(i), Code: VerdictHashMismatch, TS: time.Now()})
	}
	base := WriteBase{IfMatch: "sha256:aaaa", Source: BaseFromServed}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := wp.checkHold("src/other-path.txt", base); err != nil {
			b.Fatalf("a path with no refusal must publish: %v", err)
		}
	}
}

// BenchmarkCheckHoldHit is the cost of the REFUSAL: a publish on a held path is
// refused without touching the network at all.
func BenchmarkCheckHoldHit(b *testing.B) {
	wp := NewWritePath(nil, nil, "", OnConflictRefuse)
	wp.armHold(Conflict{Path: "src/held.txt", Code: VerdictHashMismatch, TS: time.Now()})
	base := WriteBase{IfMatch: "sha256:aaaa", Source: BaseFromServed}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := wp.checkHold("src/held.txt", base); err == nil {
			b.Fatal("the held path must be refused")
		}
	}
}

// BenchmarkNoteRead is the recovery's cost: a caller-facing read clears the hold
// (and records what was served).
func BenchmarkNoteRead(b *testing.B) {
	wp := NewWritePath(nil, nil, "", OnConflictRefuse)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wp.NoteRead("src/served.txt", "sha256:bbbb")
	}
}

func holdPath(i int) string {
	return fmt.Sprintf("p/%d.txt", i)
}
