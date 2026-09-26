package fsclient

import "syscall"

// The errno values BFS-005 §7.1 fixes, named once and carried explicitly rather
// than read from the platform's syscall package.
//
// Why not `syscall.ESTALE` directly: this package must keep compiling for the
// non-Linux targets BFS-010 (WinFsp, via cgofuse) will need, and the Windows
// syscall package does not define ESTALE, EREMOTEIO or ENOTCONN at all. The
// Linux numbers are the canonical ones here because Linux is the platform with
// a binding today (BFS-003's decision); the Windows driver maps these named
// values onto Win32 status codes at its own seam rather than re-deriving them.
//
// One errno per *recovery* class: ENOTCONN covers all three transport causes,
// because they recover identically, while the cause is named one level up where
// a human and a test can both read it. EREMOTEIO (tree identity) is kept
// distinct from ESTALE (per-file conflict) because a tool that retries an
// ESTALE in a loop must not do the same with a re-bound tree.
const (
	ErrnoNone       syscall.Errno = 0   // success
	ErrnoEPERM      syscall.Errno = 1   // a refusal by policy (write outside the mount root)
	ErrnoENOENT     syscall.Errno = 2   // the path is not there
	ErrnoEIO        syscall.Errno = 5   // malformed/5xx response, body hash mismatch
	ErrnoEACCES     syscall.Errno = 13  // credentials refused
	ErrnoEEXIST     syscall.Errno = 17  // target exists
	ErrnoEINVAL     syscall.Errno = 22  // the caller (or the client) asked for something invalid
	ErrnoEFBIG      syscall.Errno = 27  // the write exceeds a stated local bound
	ErrnoENOTEMPTY  syscall.Errno = 39  // collection not empty
	ErrnoEOPNOTSUPP syscall.Errno = 95  // this surface/build does not serve that
	ErrnoENOTCONN   syscall.Errno = 107 // transport: connect, deadline or reset
	ErrnoESTALE     syscall.Errno = 116 // write precondition refused: re-read and retry
	ErrnoEREMOTEIO  syscall.Errno = 121 // the served tree identity changed
)

// portableErrno is the single seam where a named errno becomes the value the
// running platform reports. On Linux it is the identity; a future non-Linux
// binding overrides it (that is BFS-010's job, and this is where it plugs in).
func portableErrno(n syscall.Errno) syscall.Errno { return n }
