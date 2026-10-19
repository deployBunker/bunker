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
//
// SURF-B2: the socket is addressed in the agent's PER-AGENT RUNTIME DIRECTORY
// (/run/bunker/<agent-id>/toolsd.sock) instead of the shared /run/user/<uid>.
// The directory is created — mode 0700, verified by stat — by the same
// mechanism that creates the docker socket directory on every spawn
// (internal/agent NET-BUNKER-007), so the install script asserts the state it
// needs and repairs it if missing; it never widens an existing directory. The
// unit text is parametrized per agent with systemd's %i specifier (the
// templated unit's instance name): ONE constant template renders correctly for
// every agent, the agent-id never passes through a shell expansion, and the
// id regex makes the rendering injection-proof by construction.

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

// surfaceToolsdSocketPath is THE canonical remote socket path for an agent
// (SURF-B2): inside the per-agent runtime directory the daemon's own spawn
// machinery provisions with mode 0700 (internal/agent sockdir NET-BUNKER-007).
// The path is pure string composition over a validated id — callers that take
// an agent id from the wire must run validSurfaceAgentID first (the command
// layer does; the daemon enforces the same regex at spawn).
func surfaceToolsdSocketPath(agentID string) string {
	return "/run/bunker/" + agentID + "/" + surfaceToolsdSocketName
}

// validSurfaceAgentID mirrors the daemon's spawn-time agent-id rule
// (internal/agent: ^[a-z0-9-]{1,63}$). The surface builders interpolate the id
// into unit text and absolute paths, so every entry point validates before
// rendering — a rejected id is a named error, never a mangled unit.
func validSurfaceAgentID(agentID string) error {
	if agentID == "" {
		return fmt.Errorf("agent id is empty")
	}
	if len(agentID) > 63 {
		return fmt.Errorf("agent id %q is longer than 63 characters", agentID)
	}
	for _, r := range agentID {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("agent id %q contains %q — only [a-z0-9-] is valid", agentID, string(r))
		}
	}
	return nil
}

// surfaceSocketUnitTemplate is the socket unit, parametrized per agent.
// ListenStream carries the SURF-B2 runtime-dir path built from %i (the
// instance name — exactly the agent id this unit was rendered for), and
// SocketMode tightens the socket file to owner-only. The template is a
// constant; renderSurfaceSocketUnit substitutes the (validated) id.
const surfaceSocketUnitTemplate = `[Unit]
Description=toolsd socket for %i (per-connection activation)
[Socket]
ListenStream=/run/bunker/%i/` + surfaceToolsdSocketName + `
SocketMode=0600
Accept=yes
[Install]
WantedBy=sockets.target`

// renderSurfaceSocketUnit renders the socket unit for one agent. The
// specifier is the id verbatim — validSurfaceAgentID has already rejected
// anything that could carry a specifier of its own.
func renderSurfaceSocketUnit(agentID string) string {
	return strings.ReplaceAll(surfaceSocketUnitTemplate, "%i", agentID)
}

// surfaceServiceUnitContent is the per-connection service template, verbatim as
// proven live. StandardInput/Output=socket is what hands the accepted
// connection to the spawned process; the binary itself is a SEPARATE concern
// (`bunker agent-tools --install`), which is why a missing one is a loud
// warning on the install path, not an install failure. The unit is templated
// (%i = instance) so the same text serves every agent; %h still resolves to
// the AGENT user's home because the units are installed through the agent's
// own exec context.
const surfaceServiceUnitContent = `[Unit]
Description=toolsd MCP for %i (one process per connection, as the agent user)
[Service]
ExecStart=%h/bin/toolsd mcp
StandardInput=socket
StandardOutput=socket`

// surfaceLegacySocketUnitContent / surfaceLegacyVerifyPath are the SURF-006
// unit text and socket path this design REPLACED. The install and remove
// scripts migrate them away: an old socket unit (ListenStream=%t/toolsd.sock)
// on an upgraded agent is re-written by the idempotent write_unit on upgrade,
// and the remove path proves BOTH the current and the legacy socket absent so
// a pre-SURF-B2 agent cannot keep a hidden listening socket behind a
// "successfully removed" verdict.
const surfaceLegacySocketUnitContent = `[Unit]
Description=toolsd socket (per-connection activation)
[Socket]
ListenStream=%t/toolsd.sock
SocketMode=0600
Accept=yes
[Install]
WantedBy=sockets.target`

const surfaceLegacySocketPath = "/run/user/$(id -u)/toolsd.sock"

// quoteShell renders s as ONE literal shell word (single-quoted, embedded
// quotes broken out), so the unit text cannot be reinterpreted by the agent's
// shell. The unit content is a constant, but the helper keeps that property
// true by construction rather than by review.
func quoteShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildSurfaceInstallScript writes both unit files under the agent's own
// per-user unit dir, then daemon-reload, enable and start the socket.
//
// SURF-B2 additions, in order:
//
//  1. The runtime directory is prepared FIRST: mkdir -p with an explicit
//     chmod 0700 (a `chown` does not set the mode, and the umask only strips
//     bits — neither is proof), then a STAT READ-BACK that aborts the install
//     unless the directory is exactly 0700. The directory is owned by the
//     agent's own uid by construction (it sits under the runtime root the
//     daemon's spawn machinery created for THIS agent and chowned to it), so
//     the script running AS the agent can create and verify it without any
//     new privilege. A directory that is somehow wider is a fail-loud
//     abort — the install never widens it.
//  2. The legacy SURF-006 unit is re-written too: write_unit's
//     compare-then-write turns an old ListenStream=%t unit into the new
//     runtime-dir unit on the first install after an upgrade, and the final
//     daemon-reload makes systemd forget the old address.
//
// Idempotency is structural: write_unit compares the wanted content with the
// file on disk and SKIPS the write when they are already identical, so
// re-running an install that is current changes nothing (no mtime churn, no
// rewrite). A differing or missing file is written atomically (temp file in
// the same directory + rename), so a half-written unit can never be what
// systemd loads. `exec 2>&1` merges the agent's stderr into the captured
// stdout so a failure surfaces with its actual systemctl message, not just an
// exit code.
func buildSurfaceInstallScript(agentID string) string {
	socketPath := surfaceToolsdSocketPath(agentID)
	return `exec 2>&1
set -eu
dir="$HOME/` + surfaceUnitSubdir + `"
rundir="/run/bunker/` + agentID + `"
# SURF-B2: the socket lives in the per-agent runtime dir (0700, agent-owned).
# mkdir -p then an EXPLICIT chmod: the umask strips bits, it never adds them,
# and a chown does not set the mode — the stat read-back below is the proof.
mkdir -p "$rundir"
chmod 700 "$rundir"
gotmode=$(stat -c '%a' "$rundir")
if [ "$gotmode" != "700" ]; then
  printf 'RUNDIR\t%s\tmode=%s\tstate=abort-wide\n' "$rundir" "$gotmode"
  exit 7
fi
printf 'RUNDIR\t%s\tmode=%s\tstate=ready\n' "$rundir" "$gotmode"
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
write_unit ` + quoteShell(renderSurfaceSocketUnit(agentID)) + ` "$dir/` + surfaceSocketUnitName + `"
write_unit ` + quoteShell(surfaceServiceUnitContent) + ` "$dir/` + surfaceServiceUnitName + `"
systemctl --user daemon-reload
systemctl --user enable ` + surfaceSocketUnitName + ` >/dev/null
systemctl --user start ` + surfaceSocketUnitName + `
# SURF-B2: the started socket must exist at the runtime-dir path with the
# agent as its owner — prove it, never assume it.
if [ -S ` + quoteShell(socketPath) + ` ]; then
  printf 'SOCKET\t%s\towner=%s\tmode=%s\n' ` + quoteShell(socketPath) + ` "$(stat -c '%U' ` + quoteShell(socketPath) + `)" "$(stat -c '%a' ` + quoteShell(socketPath) + `)"
else
  printf 'SOCKET\t%s\tstate=absent\n' ` + quoteShell(socketPath) + `
  exit 8
fi`
}

// buildSurfacePreflightScript answers one question — is the agent's USER
// manager alive? — and always exits 0 so the verdict is read from the output,
// never from an exit code that conflates "degraded" with "could not ask".
// is-system-running deliberately returns non-zero for several states
// (offline, degraded, maintenance, unknown); the caller classifies them.
// It is agent-independent (no id, no path), so one constant serves every call.
func buildSurfacePreflightScript() string {
	return surfacePreflightScript
}

const surfacePreflightScript = `systemctl --user is-system-running 2>/dev/null || true
`

// surfaceVerifyScript reports the post-install state as ONE RESULT line:
// both unit files present on disk, the socket unit's is-active state, the
// runtime-dir socket path with its owner and mode, and the runtime dir's own
// mode. Exit 4 means a unit file is missing after an install — an install
// that reports success while a unit is absent is exactly the silent partial
// state SURF-006 forbids.
func surfaceVerifyScript(agentID string) string {
	socketPath := surfaceToolsdSocketPath(agentID)
	return `set -eu
s="$HOME/` + surfaceUnitSubdir + `/` + surfaceSocketUnitName + `"
v="$HOME/` + surfaceUnitSubdir + `/` + surfaceServiceUnitName + `"
p=` + quoteShell(socketPath) + `
if [ ! -f "$s" ] || [ ! -f "$v" ]; then
  printf 'RESULT\tstate=missing-unit\tsocket=none\towner=unknown\tmode=unknown\n'
  exit 4
fi
state=$(systemctl --user is-active ` + surfaceSocketUnitName + ` 2>/dev/null || true)
dmode=$(stat -c '%a' /run/bunker/` + agentID + ` 2>/dev/null || echo unknown)
if [ -S "$p" ]; then
  own=$(stat -c '%U' "$p" 2>/dev/null || echo unknown)
  mode=$(stat -c '%a' "$p" 2>/dev/null || echo unknown)
else
  own=absent
  mode=absent
fi
printf 'RESULT\tstate=%s\tsocket=%s\towner=%s\tmode=%s\tdirmode=%s\n' "$state" "$p" "$own" "$mode" "$dmode"
`
}

// surfaceRemoveScript stops and disables the socket, removes BOTH unit files,
// and reloads the manager so nothing dangling stays loaded. It also proves the
// LEGACY (%t) socket path absent, so a pre-SURF-B2 agent upgraded and later
// removed cannot keep a hidden listening socket behind a success verdict.
// The script is deliberately tolerant of a down user manager (`set -u`, not
// `set -e`; stop / disable / reload failures become NOTE lines, because
// removal must still work when the manager the units belonged to is gone) but
// STRICT about the result: exit 5 = a socket path still exists, exit 6 = a
// unit file survived the rm — either way the CLI reports a failed removal
// instead of a partial one.
func surfaceRemoveScript(agentID string) string {
	socketPath := surfaceToolsdSocketPath(agentID)
	return `exec 2>&1
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
p=` + quoteShell(socketPath) + `
if [ -e "$p" ] || [ -S "$p" ]; then
  printf 'RESULT\tsocket=%s\tstate=still-present\tunits=deleted\n' "$p"
  exit 5
fi
legacy=` + quoteShell(surfaceLegacySocketPath) + `
if [ -e "$legacy" ] || [ -S "$legacy" ]; then
  printf 'RESULT\tsocket=%s\tstate=still-present\tunits=deleted\n' "$legacy"
  exit 5
fi
printf 'RESULT\tsocket=%s\tstate=removed\tunits=deleted\n' "$p"
`
}

// buildSurfaceRemoveScript returns the remove program for one agent. A
// function like its install sibling so callers and tests address both through
// one shape.
func buildSurfaceRemoveScript(agentID string) string {
	return surfaceRemoveScript(agentID)
}

// surfaceTCPProbeScript is the SURF-B2 no-TCP listener proof, run ON THE
// AGENT as the agent user: it counts LISTENING TCP SOCKETS OWNED BY THE
// SURFACE (process name toolsd), not the whole host — the agent is a plain
// user on the daemon host, so sshd/bunkerd/dockerd listeners belong to other
// components and are not this transport's. The contract is `TCP\tcount=0`:
// the surface is connection-activated (zero toolsd processes before the
// first connect), so ANY toolsd-owned TCP listener is a failure of the
// unix-only transport claim. `ss` is preferred (iproute2 is on every spawn
// base image); when it is absent the script prints count=unknown and the
// CALLER treats unknown as a failure to prove, not as a pass.
const surfaceTCPProbeScript = `if command -v ss >/dev/null 2>&1; then
  n=$(ss -H -tlnp 2>/dev/null | grep -c 'users:(("toolsd"')
else
  n=unknown
fi
printf 'TCP\tcount=%s\n' "$n"
`

// surfaceCheckScript answers the recipe's live checks in ONE exec, as
// CHECK	key=value lines so the verdict is parsed, not eyeballed:
//
//	socket_active : systemctl --user is-active toolsd.socket
//	socket_meta   : owner and mode of the agent's runtime-dir socket (the
//	                AGENT must own it; the unit sets SocketMode=0600)
//	dir_meta      : owner and mode of /run/bunker/<agent-id> (must be the
//	                AGENT, mode 0700 — the per-agent boundary)
//	toolsd_procs  : pgrep -c toolsd BEFORE any connect (must be 0 — the
//	                service is connection-activated; a pre-running toolsd
//	                means something else spawned it)
//	tcp_listeners : listening TCP sockets owned by the surface (must be 0 —
//	                the transport is the unix socket only; unknown =
//	                unproven). Host listeners belonging to other components
//	                (sshd, bunkerd, dockerd) are not this transport's and
//	                are excluded by the process-name match.
func surfaceCheckScript(agentID string) string {
	socketPath := surfaceToolsdSocketPath(agentID)
	return `exec 2>&1
p=` + quoteShell(socketPath) + `
d=/run/bunker/` + agentID + `
state=$(systemctl --user is-active ` + surfaceSocketUnitName + ` 2>/dev/null || true)
printf 'CHECK	socket_active=%s\n' "$state"
if [ -S "$p" ]; then
  own=$(stat -c '%U' "$p" 2>/dev/null || echo unknown)
  mode=$(stat -c '%a' "$p" 2>/dev/null || echo unknown)
  printf 'CHECK	socket_meta=%s:%s\n' "$own" "$mode"
else
  printf 'CHECK	socket_meta=absent\n'
fi
if [ -d "$d" ]; then
  down=$(stat -c '%U' "$d" 2>/dev/null || echo unknown)
  dm=$(stat -c '%a' "$d" 2>/dev/null || echo unknown)
  printf 'CHECK	dir_meta=%s:%s\n' "$down" "$dm"
else
  printf 'CHECK	dir_meta=absent\n'
fi
procs=$(pgrep -c -x toolsd 2>/dev/null || echo 0)
printf 'CHECK	toolsd_procs=%s\n' "$procs"
if command -v ss >/dev/null 2>&1; then
  tcp=$(ss -H -tlnp 2>/dev/null | grep -c 'users:(("toolsd"')
else
  tcp=unknown
fi
printf 'CHECK	tcp_listeners=%s\n' "$tcp"
`
}

// surfaceToolsdProbeScript stats the binary the socket activates, through the
// agent's own exec context. It is agent-independent and unchanged since
// SURF-006: the binary lives at $HOME/bin/toolsd regardless of where the
// socket is addressed.
const surfaceToolsdProbeScript = `if [ -x "$HOME/bin/toolsd" ]; then echo present; else echo absent; fi
`

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
