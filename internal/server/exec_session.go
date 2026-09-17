// Session-denial diagnostic for `bunker exec`.
//
// An agent whose ssh session is rejected by PAM before the command runs
// produces a signature that is easy to mistake for a generic ssh failure:
// the ssh child exits 254, the login banner is the only stdout, and ssh
// writes NOTHING to stderr (sshd logs `error: PAM: pam_open_session():
// System error` to the SERVER journal, not to the client). The operator
// otherwise sees only the banner plus a bare exit 254, with no cause.
//
// The recognition of that signature and the diagnostic text itself live in
// the internal/sshsig leaf package (INT-DEMO-002): internal/server imports
// internal/agent, so the agent-side spawn probe cannot import this package
// without a cycle — the classifier moved to a leaf both sides share. This
// file keeps the package-local names and delegates, so every existing call
// site and test is unchanged.
package server

import "github.com/deployBunker/bunker/internal/sshsig"

// sessionDenialDiagnostic is the one-line operator-facing message streamed
// as a stderr frame when the session-denied signature is recognized. It is
// owned by internal/sshsig so the spawn-path probe reports the same line.
const sessionDenialDiagnostic = sshsig.SessionDenialDiagnostic

// classifyExecSessionDenial reports whether an ssh child's result is the
// session-denied-before-the-command-ran signature, and returns the
// operator-facing diagnostic to stream.
//
// It fires ONLY on the conjunction of all three facts:
//
//	exitCode == 254   — ssh's own "remote command failed to start" status
//	stderrBytes == 0  — ssh reported no error of its own to the client
//	stdoutBytes > 0   — something WAS streamed (the login banner), so the
//	                    transport reached the host and the session got far
//	                    enough to print before being torn down
//
// Everything else is untouched and gets no diagnostic:
//
//	exit 0                    — success
//	exit 1/127 with output    — the remote command ran and reported its own
//	                            failure; that output is the cause
//	254 WITH stderr bytes     — ssh (or the remote command) reported a real
//	                            error; surface that instead, never guess over it
//	254 with no output at all — ssh never got a session (DNS/route/auth/key
//	                            failure): a reachability problem, not a
//	                            session denial
//
// Pure: it reads no state and writes none, so callers keep full control of
// frame ordering and exit codes. Behaviour is unchanged — the conjunction
// and the diagnostic text are byte-identical to the pre-INT-DEMO-002
// implementation, which now lives in internal/sshsig.
func classifyExecSessionDenial(exitCode int, stderrBytes, stdoutBytes int) (string, bool) {
	return sshsig.ClassifySessionDenial(exitCode, stderrBytes, stdoutBytes)
}
