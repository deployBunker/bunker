// Package fsclient is the client half of the bunker-fs filesystem: it talks to
// the `bunkerd` WebDAV surface (docs/spec/BFS-004-webdav-surface.md) and gives
// its callers the four mechanisms docs/spec/BFS-005-client-cache-and-diff.md
// specifies — a bounded content-addressed cache (§3), the invalidation channel
// with its declared poll fallback (§4), the content-hash write precondition
// (§5), and the delegated whole-tree operations (§6) that serve this client's
// own verbs rather than the mount's syscalls.
//
// The package is deliberately transport-shaped and has no FUSE in it: the
// binding lives in internal/fsmount, so the client can be exercised (and
// measured) with no mount at all — which is exactly how BFS-005 §6.4's
// "second surface" is supposed to work.
package fsclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"

	"time"
)

// Cause names the recovery class of a failure — the one errno per recovery
// class of BFS-005 §7.1, with the cause named one level up where a human and a
// test can both read it.
type Cause string

const (
	// CauseNone is the zero cause of a successful operation.
	CauseNone Cause = ""
	// CauseUnreachableConnect: connection refused / TLS handshake failure.
	CauseUnreachableConnect Cause = "unreachable_connect"
	// CauseUnreachableDeadline: an in-flight request exceeded its deadline.
	CauseUnreachableDeadline Cause = "unreachable_deadline"
	// CauseUnreachableReset: the stream was reset / GOAWAY mid-operation.
	CauseUnreachableReset Cause = "unreachable_reset"
	// CauseStaleIdentity: the served tree identity changed (re-bound tree).
	CauseStaleIdentity Cause = "stale_identity"
	// CauseConflict: a write precondition failed. Recoverable per file.
	CauseConflict Cause = "conflict"
	// CauseServerError: malformed or 5xx response, or a failed body hash.
	CauseServerError Cause = "server_error"
	// CauseUnverified: the operation completed without a server ack. Never
	// reported as success (SPEC-sshfs-mount-durability rule 3).
	CauseUnverified Cause = "unverified"
	// CauseLocalCapability: the caller's local configuration refused, e.g. a
	// mountpoint that is not private. No server was contacted.
	CauseLocalCapability Cause = "local_capability"
)

// The server's machine codes this client branches on, quoted verbatim from the
// surface's own vocabulary (BFS-004 §5.1). Nothing here is a second spelling.
const (
	VerdictOK                    = "ok"
	VerdictHashMismatch          = "hash_mismatch"
	VerdictPreconditionFailed    = "precondition_failed"
	VerdictIdenticalContent      = "identical_content"
	VerdictBodyHashMismatch      = "body_hash_mismatch"
	VerdictStaleTree             = "stale_tree"
	VerdictCapabilityUnavailable = "capability_unavailable"
	VerdictOpUnknown             = "op_unknown"
	VerdictExtensionOpMissing    = "extension_op_missing"
	VerdictWorkspaceInvalid      = "workspace_invalid"
	VerdictNotFound              = "not_found"
	VerdictNoop                  = "noop"
)

// OpError is one failed client operation. It carries three things a caller
// needs and which a bare error string cannot: the errno the filesystem will
// report, the named cause a human reads in `bunker fs status`, and the server's
// own machine code where the server named one.
type OpError struct {
	// Op is the client operation ("GET", "PUT", "PROPFIND", "snapshot", …).
	Op string
	// Path is the in-tree path the operation addressed, when it had one.
	Path string
	// Errno is the FUSE-visible error for this recovery class (§7.1).
	Errno syscall.Errno
	// Cause is the named cause for the same class.
	Cause Cause
	// Status is the HTTP status, 0 when no response arrived.
	Status int
	// Verdict is the server's machine code, verbatim (BFS-004 §5.1).
	Verdict string
	// CurrentHash / ExpectedHash are the two hashes a hash_mismatch names.
	// Both are empty for a precondition_failed, which names no hash — the two
	// codes exist precisely so absence is *named* rather than reported as a
	// null (BFS-004 §3 E-2).
	CurrentHash  string
	ExpectedHash string
	// Capability/Scope/Phase/Mode are the structured refusal fields of
	// BFS-004 §5.2, present on a capability_unavailable.
	Capability string
	Scope      string
	Phase      string
	Mode       string
	// Detail is the server's own detail text, when it sent one.
	Detail string
	// Err is the underlying error, when there was one.
	Err error
}

func (e *OpError) Error() string {
	msg := fmt.Sprintf("%s %s: errno=%s cause=%s", e.Op, e.Path, ErrnoName(e.Errno), e.Cause)
	if e.Status != 0 {
		msg += fmt.Sprintf(" status=%d", e.Status)
	}
	if e.Verdict != "" {
		msg += " verdict=" + e.Verdict
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *OpError) Unwrap() error { return e.Err }

// Is lets callers match on the named cause without unwrapping.
func (e *OpError) Is(target error) bool {
	other, ok := target.(*OpError)
	if !ok {
		return false
	}
	return other.Cause != CauseNone && other.Cause == e.Cause
}

// errCause is a sentinel wrapper so errors.Is(err, &OpError{Cause: X}) works.
func causeError(c Cause) error { return &OpError{Cause: c} }

// ErrDeletedTree is the sentinel for a tree-identity mismatch: the served tree
// was re-created, and BFS-005 §7.1's rule is that a fresh tree is NEVER
// auto-adopted.
var ErrDeletedTree = causeError(CauseStaleIdentity)

// ErrConflictRefused matches every refused write precondition.
var ErrConflictRefused = causeError(CauseConflict)

// ErrUnreachable matches all three transport causes (one recovery class).
var ErrUnreachable = causeError(CauseUnreachableConnect)

// ErrnoName renders an errno the way the status document and the mount log
// report it, so one vocabulary is used everywhere.
func ErrnoName(errno syscall.Errno) string {
	switch errno {
	case 0:
		return "OK"
	case syscall.ENOTCONN:
		return "ENOTCONN"
	case syscall.ESTALE:
		return "ESTALE"
	case syscall.EREMOTEIO:
		return "EREMOTEIO"
	case syscall.EIO:
		return "EIO"
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.EACCES:
		return "EACCES"
	case syscall.EPERM:
		return "EPERM"
	case syscall.EEXIST:
		return "EEXIST"
	case syscall.ENOTEMPTY:
		return "ENOTEMPTY"
	case syscall.EINVAL:
		return "EINVAL"
	case syscall.EFBIG:
		return "EFBIG"
	case syscall.EOPNOTSUPP:
		return "EOPNOTSUPP"
	default:
		return fmt.Sprintf("errno(%d)", int(errno))
	}
}

// classifyTransport maps a transport-level error onto the §7.1 table. The
// three transport causes share ENOTCONN because they recover identically; the
// cause keeps them distinguishable.
func classifyTransport(err error) *OpError {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &OpError{Errno: syscall.ENOTCONN, Cause: CauseUnreachableDeadline, Err: err}
	case errors.Is(err, context.Canceled):
		return &OpError{Errno: syscall.ENOTCONN, Cause: CauseUnreachableReset, Err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &OpError{Errno: syscall.ENOTCONN, Cause: CauseUnreachableDeadline, Err: err}
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return &OpError{Errno: syscall.ENOTCONN, Cause: CauseUnreachableDeadline, Err: err}
		}
		// A refused connection or a TLS handshake failure: the far end is not
		// there to answer. Distinguish "refused" (we could not connect at all,
		// no request was ever sent) from a reset mid-flight.
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			if opErr.Op == "dial" {
				return &OpError{Errno: syscall.ENOTCONN, Cause: CauseUnreachableConnect, Err: err}
			}
			if opErr.Op == "write" || opErr.Op == "read" {
				return &OpError{Errno: syscall.ENOTCONN, Cause: CauseUnreachableReset, Err: err}
			}
		}
		return &OpError{Errno: syscall.ENOTCONN, Cause: CauseUnreachableConnect, Err: err}
	}
	return &OpError{Errno: syscall.ENOTCONN, Cause: CauseUnreachableConnect, Err: err}
}

// The deadlines of BFS-005 §7.2. Both are bound at construction and are
// overridable for tests only.
const (
	// DefaultBindTimeout is the mount-time probe deadline. Failure refuses the
	// mount: nothing appears at the mountpoint.
	DefaultBindTimeout = 5 * time.Second
	// DefaultOpTimeout bounds any in-flight operation (AC-6's number).
	DefaultOpTimeout = 30 * time.Second
)
