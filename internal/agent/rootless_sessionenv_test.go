package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── INT-SPAWN-004: the session bus environment must be delivered IN-BAND ───
//
// The daemon runs commands inside the agent user's session with
// `su - <user> -c <script>`. A LOGIN shell started by `su -` RESETS the
// environment, so setting XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS on the su
// PROCESS (userSessionEnv) provably does NOT reach the command the session
// runs. Measured on the demo host, as root, both ways:
//
//	XDG_RUNTIME_DIR=/run/user/1002 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1002/bus \
//	    su - kara -c 'echo XDG=[$XDG_RUNTIME_DIR] DBUS=[$DBUS_SESSION_BUS_ADDRESS]'
//	→ XDG=[] DBUS=[]                                  (the environment was stripped)
//
//	systemd-run --quiet --wait --pipe --unit=probe su - kara -c \
//	    'echo XDG=[$XDG_RUNTIME_DIR] DBUS=[$DBUS_SESSION_BUS_ADDRESS]'
//	→ XDG=[/run/user/1001] DBUS=[unix:path=/run/user/1001/bus]  (only because
//	  pam_systemd supplied it: the caller was NOT inside a logind session)
//
// The agent session therefore had NEITHER variable and systemctl answered
// "Failed to connect to bus: No medium found", which killed every spawn of a
// daemon that was not launched by systemd at stage rootless-install.
//
// Every test in this file asserts the COMMAND STRING recorded at the seam
// (userSessionRunner / rootlessInstallerRunner) — the artifact `su -` actually
// executes. That distinction is the point of the whole ticket: a test that
// asserts cmd.Env on the su process is a PHANTOM PASS, because production
// strips exactly that environment before the command runs.
//
// The mechanism is additionally EXECUTED, not just inspected: the in-band
// prefix of a recorded command is run by /bin/sh with a wiped environment
// (`env -i`, the local model of the login shell's reset) and the values it
// carries are read back out of the shell.

// sessionScriptTail returns the caller's own script from a session command
// string: it skips the LEADING in-band `export …;` assignment statement
// (INT-SPAWN-004) and returns everything after it. The scan is quote-aware, so
// a value that itself contains a space, a semicolon or a quote cannot be
// mistaken for the separator, and it handles the `'\”` idiom the single-quote
// spelling uses. A command with no in-band prefix is returned unchanged — that
// is exactly the pre-fix command string, which keeps the call labels in the
// harnesses meaningful either way.
func sessionScriptTail(script string) string {
	_, tail, ok := splitSessionHead(script)
	if !ok {
		return script
	}
	return tail
}

// splitSessionHead splits a session command into its in-band assignment words
// and the caller's script. ok is false when the command carries no in-band
// assignment block at all (the pre-fix shape, and the phantom-pass shape).
func splitSessionHead(cmd string) (assignments []string, script string, ok bool) {
	const prefix = "export "
	if !strings.HasPrefix(cmd, prefix) {
		return nil, cmd, false
	}
	rest := cmd[len(prefix):]
	inQuote := false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '\\':
			if !inQuote {
				i++ // an escaped literal, outside quotes
			}
		case '\'':
			inQuote = !inQuote
		case ';':
			if !inQuote && i+1 < len(rest) && rest[i+1] == ' ' {
				return shellFields(rest[:i]), rest[i+2:], true
			}
		}
	}
	return nil, cmd, false
}

// shellFields splits a shell statement head into words, honoring single quotes
// so a quoted value containing spaces stays ONE word. The characters are
// preserved byte-for-byte (including the `'\”` escaping), so the result can be
// compared against the expected wire form character by character.
func shellFields(head string) []string {
	var words []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(head); i++ {
		c := head[i]
		switch {
		case c == '\\' && !inQuote:
			// An escaped literal outside quotes: kept verbatim, and it does
			// NOT toggle the quote state (that is the `'\''` idiom).
			cur.WriteByte(c)
			i++
			if i < len(head) {
				cur.WriteByte(head[i])
			}
		case c == '\'':
			inQuote = !inQuote
			cur.WriteByte(c)
		case (c == ' ' || c == '	') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return words
}

// sq is the single-quote spelling of a shell value, written out HERE on
// purpose: the expected command strings are built by the test itself, so the
// assertions pin the wire form instead of echoing whatever the production
// helper happens to produce.
func sq(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

// sessionBusWords is the expected in-band assignment block for a runtime
// directory: the systemd runtime dir and the user manager's bus socket
// address, both derived from the runtime directory and single-quoted.
func sessionBusWords(runtimeDir string) []string {
	return []string{
		"XDG_RUNTIME_DIR=" + sq(runtimeDir),
		"DBUS_SESSION_BUS_ADDRESS=" + sq("unix:path="+filepath.Join(runtimeDir, "bus")),
	}
}

// assertInBandSessionCommand asserts that cmd is exactly one in-band
// assignment block carrying wantAssignments IN ORDER, followed by wantScript
// BYTE-IDENTICAL. A command whose assignments are missing fails here — which
// is the pre-fix failure mode this file exists to catch.
func assertInBandSessionCommand(t *testing.T, cmd string, wantAssignments []string, wantScript string) {
	t.Helper()
	got, script, ok := splitSessionHead(cmd)
	if !ok {
		t.Fatalf("the session command carries NO in-band assignment block, so `su -` would strip the bus environment and the session would answer \"Failed to connect to bus: No medium found\"; command: %q", cmd)
	}
	if len(got) != len(wantAssignments) {
		t.Errorf("the in-band block must carry exactly %d assignment(s) (%s), got %d: %q",
			len(wantAssignments), strings.Join(wantAssignments, " "), len(got), cmd)
	}
	for i, want := range wantAssignments {
		if i >= len(got) {
			break
		}
		if got[i] != want {
			t.Errorf("in-band assignment %d must be %q, got %q (command: %q)", i, want, got[i], cmd)
		}
	}
	// In-band means INSIDE the command: the assignments come first, then the
	// caller's script.
	if i := strings.Index(cmd, wantScript); i < 0 {
		t.Errorf("the caller's script is missing from the command: %q", cmd)
	}
	if script != wantScript {
		t.Errorf("the caller's script must pass through byte-identically: got %q, want %q (command: %q)", script, wantScript, cmd)
	}
}

// sessionPrefixOf returns the in-band prefix of a recorded session command
// (everything up to and including the separator) so a test can run the REAL
// prefix against its own witness script.
func sessionPrefixOf(t *testing.T, cmd, script string) string {
	t.Helper()
	if !strings.HasSuffix(cmd, script) {
		t.Fatalf("premise broken: the command must end with the caller's script %q, got %q", script, cmd)
	}
	return strings.TrimSuffix(cmd, script)
}

// shellRun executes command through /bin/sh with an EMPTY environment
// (`env -i`): nothing the caller put on the parent process survives into it,
// which is the local model of the login shell's environment reset. Note that
// `su -` cannot be exercised here (a non-root test host cannot su without a
// password), so this proves the MECHANISM the command relies on rather than
// the su invocation itself.
func shellRun(t *testing.T, command string) string {
	t.Helper()
	out, err := exec.Command("env", "-i", "/bin/sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("running the session command under a wiped environment failed: %v (command: %q, output: %s)",
			err, command, out)
	}
	return strings.TrimRight(string(out), "\n")
}

// readBusEnvWitness is a witness script that prints the two bus variables, so
// the shell that runs it reports what it actually received. The brackets make
// an EMPTY environment visible as `[][]` instead of vanishing into whitespace.
const readBusEnvWitness = `printf '[%s][%s]' "$XDG_RUNTIME_DIR" "$DBUS_SESSION_BUS_ADDRESS"`

// TestSessionProbeCommand_CarriesBusEnvInBand is the probe path: the command
// string the agent session receives for the reachability probe must assign
// BOTH bus variables IN-BAND, before the reload script, with the script passed
// through byte-identically.
func TestSessionProbeCommand_CarriesBusEnvInBand(t *testing.T) {
	h := newRecycledUidHost(t)
	h.waitBudget = 500 * time.Millisecond
	h.install(t, t.TempDir())

	if err := waitForUserManagerBus(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger()); err != nil {
		t.Fatalf("healthy bus must return nil, got: %v%s", err, h.callLog())
	}
	if len(h.sessionScripts) != 1 {
		t.Fatalf("expected exactly 1 session command, got %d:%s", len(h.sessionScripts), h.callLog())
	}
	assertInBandSessionCommand(t, h.sessionScripts[0], sessionBusWords(h.runtimeDir), userManagerReloadCmd)
}

// TestSessionProbeCommand_EveryProbeCarriesTheInBandEnv covers the readiness
// gate's retries and both readiness waits of the one-shot recovery: EVERY
// session command must carry the in-band block, because a probe that relied on
// the su process environment would fail exactly the way the tick-453 spawns
// did — no matter how many times it retried.
func TestSessionProbeCommand_EveryProbeCarriesTheInBandEnv(t *testing.T) {
	h := newRecycledUidHost(t)
	h.foreignManagerRunning(t)
	h.waitBudget = 500 * time.Millisecond
	// Refused twice, then the bus answers: the gate issues three probes and
	// its retry log line proves the retries really happened.
	h.sessionBusAnswers = func(idx int) bool { return idx >= 2 }
	h.install(t, lingerDirWithEntries(t, 2))

	if err := proveUserManagerReachableWithRecovery(context.Background(), h.username, h.uid, h.runtimeDir, ctlLogger()); err != nil {
		t.Fatalf("a bus that answers within the readiness budget must let the spawn proceed, got: %v%s", err, h.callLog())
	}
	if len(h.sessionScripts) < 3 {
		t.Fatalf("premise broken: expected the gate to retry (>=3 probes), got %d:%s", len(h.sessionScripts), h.callLog())
	}
	for i, cmd := range h.sessionScripts {
		assertInBandSessionCommand(t, cmd, sessionBusWords(h.runtimeDir), userManagerReloadCmd)
		if i > 0 && cmd != h.sessionScripts[0] {
			t.Errorf("every probe must be the identical command string (single construction), probe 0 %q vs probe %d %q",
				h.sessionScripts[0], i, cmd)
		}
	}
}

// TestInstallRootlessDocker_InstallerCarriesBusEnvAndTogglesInBand is the
// installer path: the command string handed to the installer runner must carry
// BOTH bus variables AND the installer's own toggles in-band, with the
// installer path passed through byte-identically.
func TestInstallRootlessDocker_InstallerCarriesBusEnvAndTogglesInBand(t *testing.T) {
	h := newUserUnitHost(t)
	home := h.userHome(t)
	h.install(t, t.TempDir())

	if err := installRootlessDocker(context.Background(), h.username, home, uuLogger()); err != nil {
		t.Fatalf("installRootlessDocker() error = %v%s", err, h.callLog())
	}
	if len(h.installerScripts) != 1 {
		t.Fatalf("expected exactly 1 installer command, got %d:%s", len(h.installerScripts), h.callLog())
	}
	want := append(sessionBusWords(h.runtimeDir), "FORCE_ROOTLESS_INSTALL=1", "SKIP_IPTABLES=1")
	assertInBandSessionCommand(t, h.installerScripts[0], want, filepath.Join(home, "rootless-install.sh"))
}

// TestInstallRootlessDocker_SingleConstructionAcrossProbeAndRetry pins the
// single-construction property (INT-CI-009) on the wire: the daemon-reload
// issued before the installer retry must be the IDENTICAL command string as the
// reachability probe, and both installer calls must be identical too — one
// construction, so no consumer can drift back to a bare script.
func TestInstallRootlessDocker_SingleConstructionAcrossProbeAndRetry(t *testing.T) {
	h := newUserUnitHost(t)
	// Installer call #0 fails with the unit-not-found signature, #1 succeeds:
	// the pre-retry daemon-reload is therefore issued.
	h.installerRuns = []bool{false, true}
	home := h.userHome(t)
	h.install(t, t.TempDir())

	if err := installRootlessDocker(context.Background(), h.username, home, uuLogger()); err != nil {
		t.Fatalf("expected the daemon-reload retry to succeed, got: %v%s", err, h.callLog())
	}
	if len(h.sessionScripts) != 2 {
		t.Fatalf("expected 2 session commands (probe + pre-retry reload), got %d:%s", len(h.sessionScripts), h.callLog())
	}
	if h.sessionScripts[0] != h.sessionScripts[1] {
		t.Errorf("the pre-retry daemon-reload must be the identical command string as the reachability probe:\n probe: %q\n retry: %q",
			h.sessionScripts[0], h.sessionScripts[1])
	}
	for _, cmd := range h.sessionScripts {
		assertInBandSessionCommand(t, cmd, sessionBusWords(h.runtimeDir), userManagerReloadCmd)
	}
	if len(h.installerScripts) != 2 || h.installerScripts[0] != h.installerScripts[1] {
		t.Errorf("both installer calls must receive the identical command string, got: %q",
			h.installerScripts)
	}
}

// TestSessionCommand_MechanismSurvivesAnEmptiedEnvironment executes the REAL
// in-band prefix of a recorded command with a wiped environment (`env -i`) and
// reads the two variables back out of the shell. That is the mechanism itself:
// the variables are carried by the command the login shell runs, so the reset
// that `su -` performs cannot take them away. The negative control in the same
// test is the phantom pass this ticket removes — the same witness script
// without the in-band block reads nothing back once the environment is gone.
func TestSessionCommand_MechanismSurvivesAnEmptiedEnvironment(t *testing.T) {
	h := newRecycledUidHost(t)
	h.waitBudget = 500 * time.Millisecond
	h.install(t, t.TempDir())

	if _, err := probeUserManagerReachable(context.Background(), h.username, h.runtimeDir); err != nil {
		t.Fatalf("healthy probe must succeed, got: %v%s", err, h.callLog())
	}
	if len(h.sessionScripts) != 1 {
		t.Fatalf("expected exactly 1 session command, got %d:%s", len(h.sessionScripts), h.callLog())
	}
	cmd := h.sessionScripts[0]
	prefix := sessionPrefixOf(t, cmd, userManagerReloadCmd)

	want := "[" + h.runtimeDir + "][unix:path=" + filepath.Join(h.runtimeDir, "bus") + "]"
	if got := shellRun(t, prefix+readBusEnvWitness); got != want {
		t.Errorf("the in-band environment must reach the command the session runs; got %q, want %q (command: %q)",
			got, want, prefix+readBusEnvWitness)
	}

	// Negative control — the pre-fix shape: the same witness script with NO
	// in-band assignment, run with the environment wiped exactly as `su -`
	// wipes it. It reads nothing back, which is why a cmd.Env assertion would
	// have been a phantom pass.
	if got := shellRun(t, readBusEnvWitness); got != "[][]" {
		t.Errorf("premise broken: a script with no in-band assignment must see an empty bus environment under a wiped environment, got %q", got)
	}
}

// TestSessionCommand_QuotesValuesSoAHostilePathCannotBreakOut drives the probe
// path with a runtime directory full of shell metacharacters (spaces, single
// quotes, command substitutions, backticks and a semicolon) and asserts that
// the value is carried EXACTLY, that a shell running the real prefix reads it
// back verbatim, and that nothing in the path EXECUTED.
func TestSessionCommand_QuotesValuesSoAHostilePathCannotBreakOut(t *testing.T) {
	scratch := t.TempDir()
	injected := filepath.Join(scratch, "injected-by-the-runtime-dir")
	hostileBase := filepath.Join(scratch,
		"run usr $(touch "+injected+") 'q' `touch "+injected+".2` ; echo BROKEN")

	h := newRecycledUidHost(t)
	h.runtimeDir = filepath.Join(hostileBase, strconv.Itoa(h.uid))
	h.dirOwner = uint32(h.uid)
	h.waitBudget = 500 * time.Millisecond
	h.install(t, t.TempDir())

	if _, err := probeUserManagerReachable(context.Background(), h.username, h.runtimeDir); err != nil {
		t.Fatalf("healthy probe must succeed, got: %v%s", err, h.callLog())
	}
	if len(h.sessionScripts) != 1 {
		t.Fatalf("expected exactly 1 session command, got %d:%s", len(h.sessionScripts), h.callLog())
	}
	cmd := h.sessionScripts[0]
	assertInBandSessionCommand(t, cmd, sessionBusWords(h.runtimeDir), userManagerReloadCmd)

	// The recorded command must still round-trip: a shell running the real
	// prefix hands the vulnerable directory through untouched.
	prefix := sessionPrefixOf(t, cmd, userManagerReloadCmd)
	want := "[" + h.runtimeDir + "][unix:path=" + filepath.Join(h.runtimeDir, "bus") + "]"
	if got := shellRun(t, prefix+readBusEnvWitness); got != want {
		t.Errorf("a runtime dir with special characters must survive the quoting; got %q, want %q (command: %q)",
			got, want, prefix+readBusEnvWitness)
	}
	for _, p := range []string{injected, injected + ".2"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("the hostile runtime-dir value EXECUTED inside the session command: %s was created (command: %q)", p, cmd)
		}
	}
}
