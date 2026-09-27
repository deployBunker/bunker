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
	// CauseStaleBound: the mount's own published size for a path is stale, so a
	// read bound by it would be silently truncated (BFS-025). Recoverable per
	// file — re-open the path and retry — which is why it shares ESTALE with the
	// per-file conflict class and is named separately from it: this is not a
	// write conflict, and a cause string that says "conflict" would tell the next
	// person the wrong thing happened.
	CauseStaleBound Cause = "stale_bound"
	// CauseWriteShapeUnsupported: a write SHAPE this surface cannot complete was
	// refused BEFORE it published anything, so the target kept its original
	// content (BFS-030). The measured case: a resize (`O_TRUNC`'s half, or
	// ftruncate) arriving while a handle opened for writing is live on the path
	// — the destructive half of an in-place rewrite whose write half cannot land
	// (Open hands back a read handle whatever the open flags say). Named
	// separately from CauseLocalCapability because nothing about the local
	// environment is wrong: the operation itself is one this surface does not
	// serve, and the recovery is to use a whole-file write instead.
	CauseWriteShapeUnsupported Cause = "write_shape_unsupported"
	// CauseStreamEnded: the pushed channel's stream ended on a LIVE context —
	// the server closed it (a designed event: BFS-040 §4.6 ends the stream on
	// `watch_lost`) rather than this client cancelling it. Named separately from
	// the transport causes because "it closed on us" and "we closed it" are
	// different facts and the landed code could not tell them apart: a clean EOF
	// was read as a success and ENDED invalidation for the life of the mount
	// (SPEC-push-channel §8.2 R-6, hole H-1). Recovery is the reconnect the
	// transport causes take: the same channel, with the cursor.
	CauseStreamEnded Cause = "stream_ended"
	// CauseStreamStalled: no line arrived for the client's idle rule — three
	// missed heartbeats of the period the surface declares (BFS-005 §4.4,
	// SPEC-push-channel §8.1/§8.3). This is the one channel fault that is a POLL
	// trigger rather than a reconnect: by silence alone the client cannot tell a
	// stalled channel from a quiet tree, so the declared rule says take the
	// mechanism that works and REPORT the degradation — silence is never a quiet
	// answer (BFS-040 §5.1, §5.4 O-2). Hole H-3: the option and its default
	// existed and nothing read them.
	CauseStreamStalled Cause = "stream_stalled"
	// CauseEventFrameOverLimit: an event frame on the pushed channel was longer
	// than this client's per-line reader, or the server declared a per-frame
	// bound this client cannot hold (BFS-062, SPEC-push-channel §7.2's
	// `max_event_bytes` relation). It is named as its own cause and NOT as a
	// transport fault for one reason: the input is DETERMINISTIC. The same bytes
	// arrive on every attempt, so a transport classification puts a legal server
	// frame into the reconnect loop — an outage manufactured out of a size
	// mismatch, which is the defect this cause removes. Recovery is the declared
	// poll (a mechanism that does not read one frame per line) once, with the
	// condition counted and both numbers named; never a retry of the same read.
	CauseEventFrameOverLimit Cause = "event_frame_over_limit"
	// CauseCancelled: the CALLER cancelled the operation — a FUSE interrupt
	// after a kill, a timeout at the caller's own level, a Ctrl-C — and the
	// exchange was abandoned before the server gave any verdict. It is a NAMED
	// cause and not one of the transport causes on purpose, and that naming is
	// this row's whole subject (PRD-bunker-invalidation.md R10 / §2.8): a
	// cancellation is not a fault, the recovery is a RETRY (EINTR), and the
	// alternative — what the tree did before this cause existed — reported the
	// caller's own interrupt as `unreachable_reset`/ENOTCONN, indistinguishable
	// from a connection that died under us. A caller that treats a cancel as a
	// transport fault backs off instead of retrying; a caller that treats it as
	// a verdict stops instead of retrying. Both lose the operation.
	CauseCancelled Cause = "cancelled"
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

// ErrCancelled matches a cancellation — the caller's own interrupt, which is
// retryable and is NOT any of the failure classes. It exists so a caller can
// write `errors.Is(err, fsclient.ErrCancelled)` where it would otherwise have
// to test an errno, and so the distinction this row lands is available to the
// mount's own retry logic without re-deriving it.
var ErrCancelled = causeError(CauseCancelled)

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
	case syscall.EINTR:
		return "EINTR"
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
		// Reachable only when the CALLER's context is NOT cancelled: a caller
		// cancel is classified as a cancellation (EINTR) by classifyRequest
		// BEFORE this table is consulted (BFS-039), so what arrives here is our
		// own teardown — the response-body cancel, or a request written off
		// while its context was being rewound. It is a transport fault, not a
		// cancellation, and it keeps its class.
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

// classifyRequest answers the one question classifyTransport cannot: did the
// CALLER cancel this operation?
//
// `context.Canceled` reaches a failed request from two different places and
// only one of them is the caller, so the two must not be given the same answer
// (PRD-bunker-invalidation.md R10 / §2.8). parent is the context the caller
// passed in, BEFORE `do` wraps it with the operation deadline — reading the
// wrapped one would report our own deadline as a caller cancel, which is the
// same defect pointing the other way.
//
//	the parent is done with Canceled → the CALLER cancelled: EINTR, cause
//	                                     "cancelled", nothing refused, retry it
//	anything else                    → classifyTransport, unchanged: a real
//	                                     transport fault keeps its §7.1 class
//
// Everything else deliberately keeps its existing classification, including the
// internal teardown the response-body cancel (cancelOnCloseBody) performs: that
// request really did end without an answer, and the caller cancelled nothing.
func classifyRequest(parent context.Context, err error, op, path string) *OpError {
	if parent != nil && errors.Is(parent.Err(), context.Canceled) {
		return &OpError{
			Op: op, Path: path,
			Errno:  portableErrno(ErrnoEINTR),
			Cause:  CauseCancelled,
			Detail: "the operation was cancelled by the caller (EINTR): no verdict was given, so retrying it is safe",
			Err:    err,
		}
	}
	e := classifyTransport(err)
	e.Op, e.Path = op, path
	return e
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
