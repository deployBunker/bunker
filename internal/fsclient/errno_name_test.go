package fsclient

import (
	"syscall"
	"testing"
)

// TestErrnoNameEREMOTEIOPortable pins the BFS-029 portability contract: the
// tree-identity errno reaches ErrnoName through the package constant
// (errno.go), not through `syscall.EREMOTEIO`, which the darwin and freebsd
// syscall packages do not define — so a value-based lookup must keep working
// on every platform ErrnoName compiles for.
func TestErrnoNameEREMOTEIOPortable(t *testing.T) {
	if ErrnoEREMOTEIO != syscall.Errno(121) {
		t.Fatalf("ErrnoEREMOTEIO = %d, want 121", int(ErrnoEREMOTEIO))
	}
	if got := ErrnoName(ErrnoEREMOTEIO); got != "EREMOTEIO" {
		t.Fatalf("ErrnoName(ErrnoEREMOTEIO) = %q, want \"EREMOTEIO\"", got)
	}
	if got := ErrnoName(syscall.Errno(121)); got != "EREMOTEIO" {
		t.Fatalf("ErrnoName(121) = %q, want \"EREMOTEIO\"", got)
	}
}
