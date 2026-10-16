package fsclient

// The write-shape contract of the bunker-fs mount, spelled in ONE place
// (DF-BUNKER-100).
//
// Why it lives here and not in the CLI: the three surfaces that have to say the
// same thing all read from this file — the `bunker fs mount` help text, the
// banner the mount prints at the moment of mount, and the Detail/Error() text of
// a refusal — and the one thing that must not happen is the docs saying "whole
// files only" while the refusal the caller just hit says only "operation not
// supported". `CauseWriteShapeUnsupported` is defined in this package, so the
// explanation of that cause belongs beside it.
//
// The measured facts behind the text are dogfood run 27 (2026-10-16,
// docs/dogfood/2026-10-16b-bunker-fs-surface.md): inside a bunker-fs mount,
// `git init` fails with "chmod on .git/config.lock failed: Operation not
// supported", shell redirection onto an existing path is refused with
// "Operation not supported", and cp/tee-style whole-file writes land with a
// byte-identical read-back. The README's mount pitch could not be exercised as
// written because the repo half of git lives on the mount — which is what this
// contract exists to stop costing people an afternoon.

// WriteShapeNotice is the COMPACT form of the contract: one sentence, for the
// places that are already printing a block of output — an error's tail, the
// mount banner, the mount log. It names the shape that works, the two that are
// refused, git, and the remediation, because a notice that names only the
// refusal is the "operation not supported" this row is about.
const WriteShapeNotice = "write shape: this surface publishes WHOLE FILES only — " +
	"`>`/`>>` onto an existing path and in-place edits are refused EOPNOTSUPP, " +
	"and git cannot run on the mount (it chmods and renames lockfiles into place); " +
	"publish whole files (cp, tee, editor save) or run the tool in the agent and copy the result back"

// WriteShapeContract is the FULL contract, printed by `bunker fs mount --help`
// (and copied verbatim into docs/mount-drivers.md, the BFS-005 spec and the
// README's mount note). It is prose rather than a table because every consumer
// prints it to a human.
const WriteShapeContract = `WRITE SHAPE CONTRACT — the mount publishes WHOLE FILES only.

  * A file is created or replaced by ONE whole-file PUT ("publish"): cp FILE
    <mount>/path, tee <mount>/path and an editor's save all work, and the bytes
    read back are byte-identical to the bytes written.

  * REDIRECTION AND IN-PLACE EDITS ARE NOT SUPPORTED. ` + "`>`" + ` and ` + "`>>`" + ` onto a path that
    ALREADY EXISTS are refused with EOPNOTSUPP — the shell prints "Operation
    not supported" — and the file that was there is left byte-identical. There
    is no in-place write, and an append is at best a bounded whole-file
    re-publication (BFS-021), not an ordinary append. Publish whole files.

  * GIT DOES NOT WORK ON THE MOUNT. git writes through lockfiles it chmods and
    renames into place, and this surface carries neither mode changes nor those
    rename shapes, so ` + "`git init`" + ` fails with
    "chmod on .git/config.lock failed: Operation not supported".
    Run git IN the agent (bunker exec), clone in the agent, or ask the mount's
    delegated verbs (bunker fs op) a git question — then publish the results
    onto the mount as whole files.

  * REMEDIATION: generate content locally (or in the agent) and publish it as a
    WHOLE file with cp/tee/editor save, or run the tool in the agent and read
    its output back through the mount.
`

// ErrWriteShapeUnsupported matches every refusal of a write shape this surface
// does not serve, so a caller can write
// `errors.Is(err, fsclient.ErrWriteShapeUnsupported)` instead of testing the
// errno (the errno alone cannot tell "this shape is not served" from "the
// server said no").
var ErrWriteShapeUnsupported = causeError(CauseWriteShapeUnsupported)

// WriteShapeRefusal builds the mount's refusal of a write shape: EOPNOTSUPP and
// the named cause, with the caller-specific reason in Detail. It exists so the
// refusal sites on the write path (`node.Write`, `truncate`) carry the SAME
// class and the same explanation instead of each spelling its own — and so a
// new refusal site cannot invent a bare errno. OpError.Error() appends
// WriteShapeNotice to every refusal built here, which is what makes the message
// name the contract and the remediation rather than only the errno the kernel
// hands back. (The append path and the metadata refusal are NOT this class:
// their recoveries differ — an append is bounded and a `chmod` cannot be
// fixed by writing — so they keep CauseLocalCapability and are explained in the
// contract text instead.)
func WriteShapeRefusal(op, path, detail string) *OpError {
	return &OpError{
		Op:     op,
		Path:   path,
		Errno:  ErrnoEOPNOTSUPP,
		Cause:  CauseWriteShapeUnsupported,
		Detail: detail,
	}
}
