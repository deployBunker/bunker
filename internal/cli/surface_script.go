package cli

import (
	"fmt"
	"strings"
)

// SURF-006: the agent tool surface as systemd units. These builders produce ONE
// shell program per operation, run through the same audited exec path as
// `bunker exec` (execOnAgentScript). Everything the operator must be able to
// trust about the mechanism — the exact unit text, the idempotent write, the
// steps and their order — is pinned by tests against these builders, so the
// live proof (docs/prd/evidence-surfaces-mechanism.md) only has to happen once.

const (
	// surfaceUnitSubdir is the per-user unit directory, relative to $HOME. The
	// agent's own user manager watches exactly this path.
	surfaceUnitSubdir = ".config/systemd/user"

	// surfaceSocketUnitName / surfaceServiceUnitName are the two units the
	// surface consists of. The service is a TEMPLATE unit: systemd spawns one
	// `toolsd mcp` per accepted connection (Accept=yes), named toolsd@N.service.
	surfaceSocketUnitName  = "toolsd.socket"
	surfaceServiceUnitName = "toolsd@.service"
)

// surfaceSocketUnitContent is the socket unit, verbatim as proven live by
// tools/surf-mechanism-proof.sh. %t resolves to /run/user/<uid> PER USER, which
// is why one unit text is correct for every agent: the units are installed
// through the agent's own exec context, so the identity that owns %t and %h is
// the agent's, never the client's or the daemon's.
const surfaceSocketUnitContent = `[Unit]
Description=toolsd socket (per-connection activation)
[Socket]
ListenStream=%t/toolsd.sock
SocketMode=0600
Accept=yes
[Install]
WantedBy=sockets.target`

// surfaceServiceUnitContent is the per-connection service template, verbatim as
// proven live. StandardInput/Output=socket is what hands the accepted
// connection to the spawned process; the binary itself is a SEPARATE concern
// (`bunker agent-tools --install`), which is why a missing one is a loud
// warning on the install path, not an install failure.
const surfaceServiceUnitContent = `[Unit]
Description=toolsd MCP (one process per connection, as the agent user)
[Service]
ExecStart=%h/bin/toolsd mcp
StandardInput=socket
StandardOutput=socket`

// quoteShell renders s as ONE literal shell word (single-quoted, embedded
// quotes broken out), so the unit text cannot be reinterpreted by the agent's
// shell. The unit content is a constant, but the helper keeps that property
// true by construction rather than by review.
func quoteShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildSurfaceInstallScript writes both unit files under the agent's own
// $HOME/.config/systemd/user, then daemon-reload, enable and start the socket.
//
// Idempotency is structural: write_unit compares the wanted content with the
// file on disk and SKIPS the write when they are already identical, so
// re-running an install that is current changes nothing (no mtime churn, no
// rewrite). A differing or missing file is written atomically (temp file in the
// same directory + rename), so a half-written unit can never be what systemd
// loads. `exec 2>&1` merges the agent's stderr into the captured stdout so a
// failure surfaces with its actual systemctl message, not just an exit code.
func buildSurfaceInstallScript() string {
	return `exec 2>&1
set -eu
dir="$HOME/` + surfaceUnitSubdir + `"
mkdir -p "$dir"
write_unit() {
  want=$(printf '%s\n' "$1")
  have=$(cat "$2" 2>/dev/null || true)
  if [ "$have" = "$want" ]; then
    printf 'UNIT\t%s\tunchanged\n' "$2"
    return 0
  fi
  tmp=$(mktemp "$dir/.toolsd-unit.XXXXXX")
  printf '%s\n' "$1" > "$tmp"
  mv -f "$tmp" "$2"
  printf 'UNIT\t%s\twritten\n' "$2"
}
write_unit ` + quoteShell(surfaceSocketUnitContent) + ` "$dir/` + surfaceSocketUnitName + `"
write_unit ` + quoteShell(surfaceServiceUnitContent) + ` "$dir/` + surfaceServiceUnitName + `"
systemctl --user daemon-reload
systemctl --user enable ` + surfaceSocketUnitName + ` >/dev/null
systemctl --user start ` + surfaceSocketUnitName
}

// buildSurfacePreflightScript answers one question — is the agent's USER
// manager alive? — and always exits 0 so the verdict is read from the output,
// never from an exit code that conflates "degraded" with "could not ask".
// is-system-running deliberately returns non-zero for several states
// (offline, degraded, maintenance, unknown); the caller classifies them.
const surfacePreflightScript = `systemctl --user is-system-running 2>/dev/null || true
`

// buildSurfaceVerifyScript reports the post-install state as ONE RESULT line:
// both unit files present on disk, the socket unit's is-active state, and the
// socket path with its owner and mode. Exit 4 means a unit file is missing
// after an install — an install that reports success while a unit is absent is
// exactly the silent partial state SURF-006 forbids.
const surfaceVerifyScript = `set -eu
s="$HOME/` + surfaceUnitSubdir + `/` + surfaceSocketUnitName + `"
v="$HOME/` + surfaceUnitSubdir + `/` + surfaceServiceUnitName + `"
if [ ! -f "$s" ] || [ ! -f "$v" ]; then
  printf 'RESULT\tstate=missing-unit\tsocket=none\towner=unknown\tmode=unknown\n'
  exit 4
fi
state=$(systemctl --user is-active ` + surfaceSocketUnitName + ` 2>/dev/null || true)
u=$(id -u)
p="/run/user/$u/toolsd.sock"
if [ -S "$p" ]; then
  own=$(stat -c '%U' "$p" 2>/dev/null || echo unknown)
  mode=$(stat -c '%a' "$p" 2>/dev/null || echo unknown)
else
  own=absent
  mode=absent
fi
printf 'RESULT\tstate=%s\tsocket=%s\towner=%s\tmode=%s\n' "$state" "$p" "$own" "$mode"
`

// buildSurfaceRemoveScript stops and disables the socket, removes BOTH unit
// files, and reloads the manager so nothing dangling stays loaded. It is
// deliberately tolerant of a down user manager (`set -u`, not `set -e`; stop /
// disable / reload failures become NOTE lines, because removal must still work
// when the manager the units belonged to is gone) but STRICT about the result:
// exit 5 = the socket path still exists, exit 6 = a unit file survived the rm —
// either way the CLI reports a failed removal instead of a partial one.
const surfaceRemoveScript = `exec 2>&1
set -u
dir="$HOME/` + surfaceUnitSubdir + `"
s="$dir/` + surfaceSocketUnitName + `"
v="$dir/` + surfaceServiceUnitName + `"
if ! out=$(systemctl --user stop ` + surfaceSocketUnitName + ` 2>&1); then
  printf 'NOTE\tstop: %s\n' "$out"
fi
if ! out=$(systemctl --user disable ` + surfaceSocketUnitName + ` 2>&1); then
  printf 'NOTE\tdisable: %s\n' "$out"
fi
rm -f "$s" "$v"
if [ -e "$s" ] || [ -e "$v" ]; then
  printf 'RESULT\tsocket=none\tstate=remove-incomplete\tunits=remaining\n'
  exit 6
fi
if ! out=$(systemctl --user daemon-reload 2>&1); then
  printf 'NOTE\tdaemon-reload: %s\n' "$out"
fi
p="/run/user/$(id -u)/toolsd.sock"
if [ -e "$p" ] || [ -S "$p" ]; then
  printf 'RESULT\tsocket=%s\tstate=still-present\tunits=deleted\n' "$p"
  exit 5
fi
printf 'RESULT\tsocket=%s\tstate=removed\tunits=deleted\n' "$p"
`

// buildSurfaceRemoveScript returns the remove program. A function like its
// install sibling so callers and tests address both through one shape.
func buildSurfaceRemoveScript() string {
	return surfaceRemoveScript
}

// surfaceResultLinePrefix marks the one line of a verify script's output that
// carries the parsed state. Everything else on the wire is prose/notes.
const surfaceResultLinePrefix = "RESULT\t"

// surfaceState is the verified post-install state of one agent's tool surface,
// and the --json wire shape of `bunker surface install`.
type surfaceState struct {
	Agent      string `json:"agent"`
	State      string `json:"socket_state"`
	SocketPath string `json:"socket_path"`
	Owner      string `json:"owner"`
	Mode       string `json:"mode"`
	// Changed is true when this install WROTE at least one unit file; a
	// re-run over an identical surface reports changed=false (the
	// idempotency evidence).
	Changed bool `json:"changed"`
	// UnixReady reports the dependency probe: is the binary the socket
	// activates present at the agent's $HOME/bin/toolsd?
	UnixReady string `json:"toolsd"`
}

// surfaceUnitAbsentErr is returned when the verify script reports a unit file
// missing after an install; runSurfaceInstall maps it to a hard error.
var surfaceUnitAbsentErr = fmt.Errorf("unit file missing after install")

// parseSurfaceResultLine extracts the state from a RESULT line. It returns ok=false
// when the line is not a RESULT line or does not carry the expected fields —
// an unparseable verify output is surfaced as UNKNOWN, never as success.
func parseSurfaceResultLine(line string) (surfaceState, bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, surfaceResultLinePrefix) {
		return surfaceState{}, false
	}
	st := surfaceState{}
	fields := strings.Split(strings.TrimPrefix(line, surfaceResultLinePrefix), "\t")
	for _, f := range fields {
		key, val, found := strings.Cut(f, "=")
		if !found {
			return surfaceState{}, false
		}
		switch key {
		case "state":
			st.State = val
		case "socket":
			st.SocketPath = val
		case "owner":
			st.Owner = val
		case "mode":
			st.Mode = val
		}
	}
	if st.State == "" {
		return surfaceState{}, false
	}
	return st, true
}

// parseSurfaceVerifyOutput finds the RESULT line in a verify script's stdout.
func parseSurfaceVerifyOutput(out string) (surfaceState, bool) {
	for _, line := range strings.Split(out, "\n") {
		if st, ok := parseSurfaceResultLine(line); ok {
			return st, true
		}
	}
	return surfaceState{}, false
}

// surfaceInstallWrote reports whether the install script actually wrote a unit
// file (as opposed to finding both unchanged).
func surfaceInstallWrote(installOut string) bool {
	return strings.Contains(installOut, "\twritten\n") ||
		strings.HasSuffix(installOut, "\twritten") ||
		strings.Contains(installOut, "\twritten\r\n")
}

// parseSurfaceRemoveResultLine extracts (socketPath, state, ok) from a remove
// RESULT line (fields socket=, state=, units=).
func parseSurfaceRemoveResultLine(line string) (string, string, bool) {
	st, ok := parseSurfaceResultLine(line)
	if !ok || st.SocketPath == "" {
		return "", "", false
	}
	return st.SocketPath, st.State, true
}

// parseSurfaceRemoveOutput finds the RESULT line in a remove script's stdout.
func parseSurfaceRemoveOutput(out string) (string, string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if path, state, ok := parseSurfaceRemoveResultLine(line); ok {
			return path, state, true
		}
	}
	return "", "", false
}

// userManagerUnavailablePhrases are the outputs `systemctl --user
// is-system-running` produces when there is NO usable user manager behind the
// call (bus not up, manager not reachable). A running-but-degraded manager
// (another unit failed) is NOT in this set: that is a health finding, not a
// reason to refuse the install.
var userManagerUnavailablePhrases = []string{
	"offline",
	"not found",                // systemctl itself missing / no systemd at all
	"no such file",             // the per-user bus socket absent (manager never started)
	"connection refused",       // manager socket exists, nothing behind it
	"failed to connect to bus", // the canonical dead-manager message
	"not been booted",          // no systemd on the host at all (containers, WSL1)
	"lock is held",             // shutdown in progress: the manager is going away
}

// parseSurfaceIsRunning returns the first non-empty, non-blank line of the
// preflight output: is-system-running prints exactly one status word.
func parseSurfaceIsRunning(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line != "" {
			return line
		}
	}
	return ""
}

// isUserManagerUnavailable classifies a preflight output line. "running" and
// "degraded" both mean a live manager (degraded = live but some other unit
// failed, which does not block our units); anything matching an unavailable
// phrase does not.
func isUserManagerUnavailable(line string) bool {
	if line == "" || line == "running" || line == "degraded" {
		return false
	}
	lower := strings.ToLower(line)
	for _, phrase := range userManagerUnavailablePhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// classifySurfaceManagerFailure maps a failed preflight to a NAMED error: which
// agent, what systemctl said, why the install refuses to proceed, and what to
// check. It exists so a down user manager can never surface as a generic exit
// code or — worse — as a silent half-install.
func classifySurfaceManagerFailure(agentID, output string) error {
	line := parseSurfaceIsRunning(output)
	if line == "" {
		return fmt.Errorf("systemd user manager not reachable on agent %q (systemctl --user is-system-running produced no output) — "+
			"refusing to install the toolsd units: writing them now would leave a silent partial state. "+
			"Every spawn enables linger for the agent user; check `loginctl show-user <agent-user> --property=Linger` on the daemon host, then re-run",
			agentID)
	}
	if isUserManagerUnavailable(line) {
		return fmt.Errorf("systemd user manager not running on agent %q (systemctl --user is-system-running: %q) — "+
			"refusing to install the toolsd units: without a live user manager there is nothing to load, start or own the socket, "+
			"so installing now would leave a silent partial state. The user manager starts on login or when linger is enabled "+
			"(every spawn enables it; check `loginctl show-user <agent-user> --property=Linger` on the daemon host), then re-run",
			agentID, line)
	}
	// A live manager reported a state we do not model: pass it through as the
	// named condition rather than inventing a verdict.
	return fmt.Errorf("systemctl --user on agent %q reported %q — refusing to install the toolsd units (expected running/degraded)", agentID, line)
}

// surfaceToolsdProbeScript stats the binary the socket activates, through the
// agent, and prints one word: present or absent.
const surfaceToolsdProbeScript = `if [ -x "$HOME/bin/toolsd" ]; then echo present; else echo absent; fi
`

// parseSurfaceToolsdProbe reads present/absent from the probe output.
func parseSurfaceToolsdProbe(out string) string {
	switch strings.TrimSpace(strings.TrimRight(out, "\r")) {
	case "present":
		return "present"
	case "absent":
		return "absent"
	default:
		return "unknown"
	}
}

// tailSurfaceOutput keeps the tail of agent output for an error message so the
// operator sees the failing command's own words, not just an exit code.
func tailSurfaceOutput(out string, max int) string {
	out = strings.TrimRight(out, "\n")
	if max <= 0 || len(out) <= max {
		return out
	}
	return "…" + out[len(out)-max:]
}
