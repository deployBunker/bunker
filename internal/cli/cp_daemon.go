package cli

// ── PERF-020: in-process (daemon-mediated) file transfer for `bunker cp` ─────
//
// `bunker cp` used to spawn an `scp` child for the transfer and an `ssh` child
// for the chown. Measured on the PERF-020 dogfood link (2026-11-22,
// bunker-las-03, 60-byte file): 4.36–4.49s per run, while `bunker exec` over
// the same link — the daemon's ExecAgent RPC — ran 0.58s. The cost was never
// the WAN: each child pays its own SSH connection setup (~2.2–2.6s fresh,
// ~1.3s per mux-reused scp child here) and exec proves the RPC path needs
// none. Building an in-process SSH client of our own (x/crypto/ssh) was
// measured too and is NOT the fix the acceptance gate needs: dial
// 0.17–0.21s + handshake 1.14–1.33s + write/fixup session 0.57–0.59s ≈
// 1.9–2.1s even with the GetAgent round trip overlapped — over the ≤1.5s
// warm gate. This file is the fix that meets the gate: the file streams
// through the daemon's ExecAgent stream with the bytes as the command's
// stdin (GAP-094), and the chown rides the same remote shell, so a default
// `bunker cp` pays exactly one GetAgent RPC plus one ExecAgent stream.
//
// Behavior kept from the scp path:
//   - the agent identity is resolved from the sshfs mount exactly as before;
//   - the destination's ownership hint (DF-BUNKER-17) and GAP-144
//     partial-transfer reporting still run through their own bounded ssh
//     probes on the failure path;
//   - the file still lands as the agent user with the source's mode
//     (scp's C-mode contract) and gets an explicit chown;
//   - the scp/vocabulary of the failure modes is preserved by construction
//     (see cpStreamViaDaemon): a missing destination directory prints the
//     same "scp: dest open …" lines the scp child printed.
//
// scp stays the transport whenever the user points the transfer at an SSH
// identity of their own — --ssh-host/--ssh-port/--ssh-key name a connection
// the daemon could never honor — and for files over maxInprocFileBytes.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// maxInprocFileBytes caps what a transfer may ride the daemon's ExecAgent
// stream at. The payload travels inside ONE request message, so it is fully
// materialized in memory on both ends; scp streams from disk and never holds
// the whole file. Past the cap the CLI keeps the scp child: the
// memory-for-speed trade is only worth it below this line. 64 MiB is ~1000x
// the 60-byte dogfood payload and well past every config/script copy the
// command is used for.
const maxInprocFileBytes = 64 << 20

// cpShouldStreamViaDaemon decides between the in-process daemon transfer and
// the scp child. Any explicitly-set SSH identity flag routes to scp verbatim:
// the daemon always writes through the agent's OWN host, so --ssh-host and
// --ssh-port cannot be honored through it, and --ssh-key would change which
// secret authenticates the operation.
func cpShouldStreamViaDaemon(sshHost string, sshPort uint32, sshKey string, fileSize int64) bool {
	return sshHost == "" && sshPort == 0 && sshKey == "" && fileSize > 0 && fileSize <= maxInprocFileBytes
}

// cpRemoteWriteScript builds the remote shell script that plays the scp
// child's role: write the stdin to the destination, then set the mode and the
// owner the way scp and the old chown child did.
//
// Exit contract (visible as the exec stream's exit code):
//
//	2 — the write failed (destination missing / not writable);
//	3 — chmod failed after the write landed;
//	4 — chown failed after the write landed;
//	0 — all three landed.
//
// The chown target is the CURRENT uid/gid ("$(id -u):$(id -g)") — the script
// already runs as the agent user both in the host context and inside an
// image-backed agent's container, where the bunker-<id> name itself may not
// resolve (the host passwd entry is what a name-based chown would need).
func cpRemoteWriteScript(remotePath string, mode fs.FileMode) string {
	return fmt.Sprintf("cat > %s || exit 2; chmod %04o %s || exit 3; chown \"$(id -u):$(id -g)\" %s || exit 4",
		quotePOSIX(remotePath), mode.Perm(), quotePOSIX(remotePath), quotePOSIX(remotePath))
}

// errnoPhrase extracts the errno message the remote shell reports for a failed
// write, as the scp child relayed it: the text after the final ": " of the
// remote's stderr line. A stderr that names no errno yields "" so the
// caller falls back to the raw remote text instead of inventing a phrase.
func errnoPhrase(remoteStderr string) string {
	line := strings.TrimSpace(remoteStderr)
	if line == "" {
		return ""
	}
	if i := strings.LastIndex(line, ": "); i >= 0 {
		phrase := line[i+2:]
		if phrase != "" {
			return phrase
		}
	}
	return ""
}

// scpErrnoVocabulary renders the remote errno phrase in the vocabulary the
// scp child printed. The daemon-side exec shell reports the SAME errno in a
// different dialect than scp's sftp transport did — measured on
// bunker-las-03: a missing destination directory came back as
// "Directory nonexistent" through the daemon's shell where scp printed
// "No such file or directory" — and the CLI's failure transcripts must not
// change shape. Known synonyms map onto scp's exact words; anything
// unfamiliar stays verbatim (never a made-up message).
func scpErrnoVocabulary(phrase string) string {
	switch strings.ToLower(strings.TrimSpace(phrase)) {
	case "no such file or directory", "directory nonexistent", "no such directory", "no such file":
		return "No such file or directory"
	case "permission denied", "access denied", "permission denied (os error 13)":
		return "Permission denied"
	}
	return phrase
}

// cpStreamViaDaemon transfers one local file into the agent through the
// daemon's ExecAgent stream and returns nil on success.
//
// Failure vocabulary: for the failed-write exit (2) the two lines the scp
// child used to print on stderr are re-emitted verbatim from the same errno
// fact the remote reported, and the returned error renders "exit status 1" —
// scp's own exit code for exactly that failure — so the CLI's error text
// ("scp: exit status 1") and the caller's GAP-144 ownership hint pipeline see
// the same words they always saw. The chown failure exit (4) renders "exit
// status 1" for the same reason: the old chown child surfaced the remote
// chown's own (normally 1) status. Nothing here invents a success: every
// non-zero exit still fails the command non-zero.
func cpStreamViaDaemon(
	ctx context.Context,
	client bunkerv1connect.BunkerdClient,
	token string,
	agentID, localPath, remotePath string,
	localMode fs.FileMode,
	msgLog io.Writer,
) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	req := connect.NewRequest(&v1.ExecAgentRequest{
		AgentId: agentID,
		Command: "sh",
		Args:    []string{"-c", cpRemoteWriteScript(remotePath, localMode)},
		// Raw stays false: the write must pass through the same
		// agent-context shell wrapper (`env DOCKER_HOST=… sh -c …`) the
		// interactive exec path uses.
		StdinPayload: data,
	})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}

	stream, err := client.ExecAgent(ctx, req)
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	var exitCode int32
	for stream.Receive() {
		msg := stream.Msg()
		stdout.Write(msg.GetStdout())
		stderr.Write(msg.GetStderr())
		if msg.ExitCode != 0 {
			exitCode = msg.ExitCode
		}
	}
	if serr := stream.Err(); serr != nil {
		return serr
	}

	switch exitCode {
	case 0:
		return nil
	case 2:
		if phrase := errnoPhrase(stderr.String()); phrase != "" {
			// The trailing \r is the scp child's own line terminator on
			// this output (its relayed sftp lines arrived as \r\n) — kept
			// so the failure transcript is byte-identical to what scp
			// printed before PERF-020.
			_, _ = fmt.Fprintf(msgLog, "scp: dest open %q: %s\r\n", remotePath, scpErrnoVocabulary(phrase))
			_, _ = fmt.Fprintf(msgLog, "scp: failed to upload file %s to %s\r\n", localPath, remotePath)
			return fmt.Errorf("exit status 1")
		}
		// No errno phrase to quote: keep the raw remote text instead of a
		// made-up shape.
		_, _ = fmt.Fprint(msgLog, stderr.String())
		return fmt.Errorf("scp: remote write to %s: %s", remotePath, strings.TrimSpace(stderr.String()))
	case 3:
		return cpFixupError{fixup: "chmod"}
	case 4:
		return cpChownError{}
	default:
		return fmt.Errorf("exit status %d", exitCode)
	}
}

// cpChownError marks the failed chown exit of the remote write script so the
// caller keeps today's chown wrap ("chown after scp: …") instead of the
// transfer's.
type cpChownError struct{}

func (cpChownError) Error() string { return "exit status 1" }

// cpFixupError marks the failed chmod exit the same way.
type cpFixupError struct {
	fixup string
}

func (e cpFixupError) Error() string { return e.fixup + " after scp: exit status 1" }
