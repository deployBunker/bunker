package fsclient

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// DF-BUNKER-100: the write-shape contract has ONE spelling, and a refusal of a
// write shape carries it.
//
// The defect these cells pin is a MESSAGE defect with a real cost: inside a
// bunker-fs mount a shell write was refused with the kernel's bare "Operation
// not supported", nothing in the CLI or the docs said the mount publishes whole
// files only, and a dogfood session lost an afternoon to it
// (docs/dogfood/2026-10-16b-bunker-fs-surface.md). The contract is therefore
// data in this package — printed by the CLI's help, by the mount's banner, and
// appended to every write-shape refusal — and these tests fail if any of those
// phrasings is dropped, so the two ways this rots (a doc that stops saying it,
// a refusal that goes back to naming only the errno) are both caught here.

// writeShapePhrases are the four facts a reader must be able to find, in every
// surface that states the contract: what works, what is refused, that git
// cannot run on the mount, and what to do instead.
var writeShapePhrases = []string{
	"WHOLE FILES only",
	"EOPNOTSUPP",
	"git",
}

// TestWriteShapeContractNamesTheShapeTheRefusalAndTheRemediation pins the
// contract text itself. It is deliberately phrase-level (not a golden blob) so
// the text can be edited, but the facts cannot be quietly dropped.
func TestWriteShapeContractNamesTheShapeTheRefusalAndTheRemediation(t *testing.T) {
	for _, want := range append([]string{
		"REDIRECTION AND IN-PLACE EDITS ARE NOT SUPPORTED",
		"git init",
		"chmod on .git/config.lock failed: Operation not supported",
		"REMEDIATION",
		"cp/tee/editor save",
	}, writeShapePhrases...) {
		if !strings.Contains(WriteShapeContract, want) {
			t.Errorf("the write-shape contract does not name %q; a contract that omits it is the gap this row exists for", want)
		}
	}
}

// TestWriteShapeNoticeCarriesTheRemediation is the SHORT form's cell: it is what
// gets appended to a refusal message and printed in the mount banner, so it
// must still say what works and what to do instead of only what failed.
func TestWriteShapeNoticeCarriesTheRemediation(t *testing.T) {
	for _, want := range append([]string{
		"`>`/`>>`",
		"cp, tee, editor save",
	}, writeShapePhrases...) {
		if !strings.Contains(WriteShapeNotice, want) {
			t.Errorf("the compact write-shape notice does not name %q; it is the text a caller actually reads, so it cannot be a bare refusal", want)
		}
	}
}

// TestWriteShapeRefusalIsEOPNOTSUPPWithTheNamedCauseAndTheContract: the helper
// every refusal site builds through must produce the errno the kernel will be
// given (EOPNOTSUPP — the wire behaviour is unchanged), the named cause the
// status document reports, a matchable sentinel, and a message that ends with
// the contract's compact form.
func TestWriteShapeRefusalIsEOPNOTSUPPWithTheNamedCauseAndTheContract(t *testing.T) {
	err := WriteShapeRefusal("WRITE", "/tree/src/main.go", "refusing an in-place write of 2 byte(s) at offset 0")
	if err.Errno != syscall.EOPNOTSUPP {
		t.Errorf("errno = %v, want EOPNOTSUPP: the kernel-visible behaviour must not change with the message", err.Errno)
	}
	if err.Cause != CauseWriteShapeUnsupported {
		t.Errorf("cause = %q, want %q (the cause BFS-030's resize refusal already uses)", err.Cause, CauseWriteShapeUnsupported)
	}
	if !errors.Is(err, ErrWriteShapeUnsupported) {
		t.Error("a caller must be able to match the refusal with errors.Is(err, fsclient.ErrWriteShapeUnsupported)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "write_shape_unsupported") {
		t.Errorf("the message must name the cause, got %q", msg)
	}
	if !strings.HasSuffix(msg, "["+WriteShapeNotice+"]") {
		t.Errorf("every write-shape refusal must end with the contract's compact form, got %q", msg)
	}
}

// TestOtherCausesDoNotGrowTheWriteShapeNotice is the NEGATIVE control: the
// notice is appended for the write-shape cause alone, so an unrelated failure's
// message stays exactly as it was (a transport cause that grew a write-shape
// explanation would send the reader after the wrong problem).
func TestOtherCausesDoNotGrowTheWriteShapeNotice(t *testing.T) {
	err := &OpError{Op: "PUT", Path: "/tree/f", Errno: syscall.ESTALE, Cause: CauseConflict, Detail: "the base hash moved"}
	if strings.Contains(err.Error(), "WHOLE FILES only") {
		t.Errorf("a %s refusal must not carry the write-shape notice, got %q", CauseConflict, err.Error())
	}
}
