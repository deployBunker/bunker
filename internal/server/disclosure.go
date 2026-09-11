// GAP-067 containment disclosure: an admin-controlled, hidden-by-default
// feature. When containment.disclosure is enabled, managed agents honestly
// disclose that they run in a managed sandbox:
//
//  1. BUNKER_SANDBOX=1 is injected into every agent session (shell exec,
//     raw exec, script exec, detached RunAgent) via the same explicit env
//     injection path as PATH/DOCKER_HOST/TMPDIR.
//  2. Successful OR failed output of an allowed system-info probe command
//     (strict basename allowlist below) gets ONE self-describing marker
//     line appended to its stdout stream. The command's exit code is
//     preserved exactly.
//
// When disabled, NOTHING changes: no env var, no marker, no extra bytes.
// See specs/containment-disclosure.md.
package server

import (
	"fmt"
	"strings"

	"github.com/deployBunker/bunker/internal/config"
)

// containmentDisclosureMarker is the single canonical marker line appended
// to allowed probe output when containment disclosure is enabled. It is
// greppable and self-describing by design — an operator (or harness) that
// sees it in command output knows the environment is a managed sandbox.
// Do NOT duplicate this literal anywhere else; reference the constant.
const containmentDisclosureMarker = "[bunker: managed sandbox environment — containment active]"

// containmentSandboxEnv aliases config.ContainmentSandboxEnv for the
// server-package builders; the canonical definition lives next to the
// containment config (single literal, no duplication).
const containmentSandboxEnv = config.ContainmentSandboxEnv

// probeSafeFlags is the strict, per-command allowlist of system-info probe
// commands and the ONLY option tokens each may carry. Table-driven: an
// invocation is an allowed probe iff its basename is a key of this table
// (bare form = no extra tokens at all) and every remaining token is an
// exact entry in that command's set. Flags that take their own argument
// (free -s N, df --output=..., id user) are deliberately absent — an
// unsupported flag or any operand fails the allowlist, so an unusual probe
// shape stays undisclosed rather than being creatively accepted.
var probeSafeFlags = map[string]map[string]bool{
	"uname": {
		"-a": true, "--all": true,
		"-s": true, "--kernel-name": true,
		"-n": true, "--nodename": true,
		"-r": true, "--kernel-release": true,
		"-v": true, "--kernel-version": true,
		"-m": true, "--machine": true,
		"-p": true, "--processor": true,
		"-i": true, "--hardware-platform": true,
		"-o": true, "--operating-system": true,
	},
	"hostname": {
		"-s": true, "--short": true,
		"-f": true, "--fqdn": true,
		"-a": true, "--alias": true,
		"-i": true, "--ip-address": true,
		"-d": true, "--domain": true,
	},
	"uptime": {
		"-p": true, "--pretty": true,
		"-s": true, "--since": true,
	},
	"free": {
		"-h": true, "--human": true,
		"-b": true, "--bytes": true,
		"-k": true, "--kibi": true,
		"-m": true, "--mebi": true,
		"-g": true, "--gibi": true,
		"--tebi": true, "--pebi": true,
		"-t": true, "--total": true,
		"-l": true, "--lohi": true,
		"-v": true, "--committed": true,
		"-w": true, "--wide": true,
	},
	"df": {
		"-h": true, "--human-readable": true,
		"-H": true, "--si": true,
		"-i": true, "--inodes": true,
		"-l": true, "--local": true,
		"-T": true, "--print-type": true,
		"-t": true, "--total": true,
	},
	"id": {
		"-u": true, "--user": true,
		"-g": true, "--group": true,
		"-G": true, "--groups": true,
		"-n": true, "--name": true,
		"-r": true, "--real": true,
		"-Z": true, "--context": true,
		"-a": true,
	},
	"whoami": {}, // bare only: whoami takes no options of value
	"lsb_release": {
		"-a": true, "--all": true,
		"-i": true, "--id": true,
		"-d": true, "--description": true,
		"-r": true, "--release": true,
		"-c": true, "--codename": true,
		"-s": true, "--short": true,
		"-v": true,
	},
}

// osReleasePath is the only file `cat` may read to still count as a probe.
const osReleasePath = "/etc/os-release"

// harmlessCatShortChars are the cat short flags that carry no target
// semantics (display-only: numbering, blank squeezing, non-printing
// revelation). A combined cluster like -nsv is harmless iff every letter
// is in this set.
const harmlessCatShortChars = "nsvEbTA"

// harmlessCatFlags are cat long/short flags that carry no target semantics.
// Anything else — including flags that add operands or read stdin — makes
// the invocation fail the strict allowlist. Strictness is the design goal:
// an unusual probe shape should stay undisclosed rather than be creatively
// accepted.
var harmlessCatFlags = map[string]bool{
	"-A": true, "--show-all": true,
	"-b": true, "--number-nonblank": true,
	"-e": true,
	"-E": true, "--show-ends": true,
	"-n": true, "--number": true,
	"-s": true, "--squeeze-blank": true,
	"-T": true, "--show-tabs": true,
	"-v": true, "--show-nonprinting": true,
}

// isHarmlessCatFlag accepts a table flag or a combined short-flag cluster
// (-nsv) whose every letter is display-only.
func isHarmlessCatFlag(flag string) bool {
	if harmlessCatFlags[flag] {
		return true
	}
	if len(flag) > 2 && flag[0] == '-' && flag[1] != '-' {
		for _, ch := range flag[1:] {
			if !strings.ContainsRune(harmlessCatShortChars, ch) {
				return false
			}
		}
		return true
	}
	return false
}

// isContainmentProbe reports whether the given command+args is an allowed
// system-info probe under the STRICT allowlist:
//
//   - shell and script mode: the first token's basename is a key of the
//     probeSafeFlags table and every remaining token is an exact entry in
//     that command's safe-flag set (bare form = no extra tokens); or `cat`
//     with only harmless flags and the single operand /etc/os-release.
//     Anything else (unsupported flags, operands/usernames, compounds,
//     pipes, redirections, command substitution, lookalikes) is rejected.
//   - raw mode: identical rules applied to argv[0] + the args slice.
//
// The matcher is a table lookup, never a regex over arbitrary command text.
// It never alters how the command runs — it only decides marker emission.
func isContainmentProbe(command string, args []string) bool {
	tokens := probeTokens(command, args)
	if len(tokens) == 0 {
		return false
	}
	base, ok := probeBasename(tokens[0])
	if !ok {
		return false
	}
	if base == "cat" {
		// Only `cat [harmless-flags] /etc/os-release` is a probe. The
		// operand must match the literal path exactly — any other target,
		// multiple operands, or stdin-only cat fails the allowlist.
		if len(tokens) < 2 || tokens[len(tokens)-1] != osReleasePath {
			return false
		}
		for _, t := range tokens[1 : len(tokens)-1] {
			if !isHarmlessCatFlag(t) {
				return false
			}
		}
		return true
	}
	safeFlags, ok := probeSafeFlags[base]
	if !ok {
		return false
	}
	for _, t := range tokens[1:] {
		if !safeFlags[t] {
			// Not an exact table entry: an unsupported flag, an operand
			// (e.g. `id someuser`), or anything else — fail closed.
			return false
		}
	}
	return true
}

// probeSafeDirs is the exact set of trusted directories a probe may be
// invoked from. An absolute path is honored ONLY when its parent is one of
// these directories; every other path shape (anything under /tmp or /home,
// relative paths, nested paths, trailing slashes) is rejected so an
// arbitrary lookalike binary can never be labeled a system probe.
var probeSafeDirs = map[string]bool{
	"/bin":      true,
	"/usr/bin":  true,
	"/usr/sbin": true,
}

// probeBasename extracts the basename of a probe token. Only BARE names
// (uname) and absolute paths whose PARENT is exactly one of probeSafeDirs
// (/usr/bin/uname) are honored. Everything else fails: absolute paths in
// untrusted directories (/tmp/uname, /home/x/uname), nested paths
// (/usr/local/bin/uname), relative paths (./uname, bin/uname), and
// trailing-slash shapes (/usr/bin/, /usr/bin/uname/). ok is false for any
// rejected shape.
func probeBasename(token string) (base string, ok bool) {
	if i := strings.LastIndex(token, "/"); i >= 0 {
		// Any slash present: require EXACTLY dir/<base> with a trusted,
		// non-empty parent directory. This single structural check rejects
		// everything untrusted — including trailing slashes (parent ends
		// in "/", base empty or nested) and traversal (".." is just a
		// rejected base under a trusted parent; "../usr/bin/uname" has an
		// untrusted ".." parent).
		if !probeSafeDirs[token[:i]] || token[i+1:] == "" {
			return "", false
		}
		return token[i+1:], true
	}
	return token, true
}

// probeTokens combines the command token with its args into one argv. If
// the command string contains shell metacharacters (quotes, operators,
// substitution, globs, vars) it fails closed: nil tokens means the caller
// treats the invocation as not-a-probe. This is what keeps `uname; whoami`
// and `uname | tee /x` out of the allowlist.
func probeTokens(command string, args []string) []string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return nil
	}
	tokens := splitSimple(trimmed)
	if tokens == nil {
		return nil
	}
	return append(tokens, args...)
}

// splitSimple splits a command string on whitespace WITHOUT any shell
// interpretation. Any shell metacharacter anywhere in the string makes it
// return nil (fail closed).
func splitSimple(command string) []string {
	if strings.ContainsAny(command, "'\";|&<>`$()*?[]{}#\\\n") {
		return nil
	}
	return strings.Fields(command)
}

// markerFrameForStream builds the marker stdout frame from the ACTUAL
// streamed-stdout state: sentBytes (was any process stdout forwarded?) and
// endsWithNewline (was the last streamed stdout byte a '\n'?). Both facts
// are recorded by the stdout streamer and synchronized via wg.Wait before
// this runs.
//
// ExecAgent streams process stdout as raw frames and then sends ONE
// separate marker frame; clients concatenate frames verbatim, so the frame
// boundary is NOT a line boundary. The marker must therefore carry its own
// leading newline exactly when the streamed output does not end with one —
// otherwise a probe printing `Linux` with no trailing newline would yield
// `Linux[bunker: ...]` client-side.
//
//   - no stdout at all           → just the marker line (no leading blank).
//   - stdout ends in '\n'        → just the marker line (no extra blank).
//   - stdout lacks final '\n'    → newline + marker line.
//
// When enabled is false the frame is empty — callers send nothing (zero
// extra bytes). Pure so it stays table-driven-testable.
func markerFrameForStream(sentBytes, endsWithNewline, enabled bool) string {
	if !enabled {
		return ""
	}
	if sentBytes && !endsWithNewline {
		return "\n" + containmentDisclosureMarker + "\n"
	}
	return containmentDisclosureMarker + "\n"
}

// markerCountsAsSent reports whether a chunk forwarded to the client counts
// as process stdout for marker-separation purposes. Empty chunks (n == 0)
// must not flip the sent state.
func markerCountsAsSent(n int) bool { return n > 0 }

// logDisclosureStartup emits the safe startup note about whether
// containment disclosure is enabled. It takes only the boolean — there is
// deliberately no path for secrets or host paths to reach it.
func logDisclosureStartup(enabled bool) string {
	if enabled {
		return fmt.Sprintf("containment disclosure ENABLED: agents disclose managed sandbox (%s env, %s marker on system-info probes)",
			containmentSandboxEnv, containmentDisclosureMarker)
	}
	return "containment disclosure disabled (default): agent sessions and exec output are unmodified"
}
