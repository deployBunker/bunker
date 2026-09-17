// Package sshsig owns the recognition of the SSH session-denial signature
// shared by the exec path and the spawn path.
//
// An agent whose ssh session is rejected by PAM before the command runs
// produces a signature that is easy to mistake for a generic ssh failure:
// the ssh child exits 254, the login banner is the only stdout, and ssh
// writes NOTHING to stderr (sshd logs `error: PAM: pam_open_session():
// System error` to the SERVER journal, not to the client). The operator
// otherwise sees only the banner plus a bare exit 254, with no cause.
//
// This package owns the recognition of exactly that signature and the single
// operator-facing line reported in its place. It is deliberately a pure
// function of the child's observable result so it can be table-tested and so
// it can never alter an exit code or an existing frame.
//
// It is a LEAF package (stdlib only) so both importers can reach it:
//
//   - internal/server (the `bunker exec` streaming path) delegates its
//     classifier here — internal/server imports internal/agent, so the
//     agent-side spawn probe (INT-DEMO-002) cannot import internal/server
//     without a cycle;
//   - internal/agent (the spawn-time session probe) calls it directly to
//     classify a failed probe before the agent is ever reported ready.
package sshsig

// SessionDenialDiagnostic is the one-line operator-facing message reported
// when the session-denied signature is recognized. It carries the observed
// evidence (exit 254, no SSH error on stderr), the most likely cause, and the
// concrete checks/remedy. There is deliberately no second cause: nothing
// else about this signature is substantiated.
const SessionDenialDiagnostic = "bunker: exec session was denied before the command ran (ssh exit 254, no SSH error on stderr): the agent user's session was rejected by PAM. Most likely cause: the agent is not a member of the isolation group, which happens when the running daemon predates the spawn-side isolation grant. Check on the server: 'getent group bunker-agents', 'bunker host-provision --status', and rebuild/redeploy the daemon so spawn grants membership."

// ClassifySessionDenial reports whether an ssh child's result is the
// session-denied-before-the-command-ran signature, and returns the
// operator-facing diagnostic to surface.
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
// frame ordering, exit codes, and spawn outcomes.
func ClassifySessionDenial(exitCode int, stderrBytes, stdoutBytes int) (string, bool) {
	if exitCode != 254 {
		return "", false
	}
	if stderrBytes != 0 {
		return "", false
	}
	if stdoutBytes <= 0 {
		return "", false
	}
	return SessionDenialDiagnostic, true
}
