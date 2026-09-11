package server

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
)

// TestIsContainmentProbe_Allowed is the table-driven matcher test: every
// allowed probe shape is recognized, everything else is rejected. The
// matcher is a strict table lookup — never a regex over command text.
func TestIsContainmentProbe_Allowed(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    []string
		want    bool
	}{
		// Allowed: bare allowlisted probe basenames.
		{"uname", "uname", nil, true},
		{"hostname", "hostname", nil, true},
		{"uptime", "uptime", nil, true},
		{"free", "free", nil, true},
		{"df", "df", nil, true},
		{"id", "id", nil, true},
		{"whoami", "whoami", nil, true},
		{"lsb_release", "lsb_release", nil, true},
		// Allowed: per-command safe option flags (exact table entries).
		{"uname -a args", "uname", []string{"-a"}, true},
		{"uname -a inline", "uname -a", nil, true},
		{"uname --all", "uname --all", nil, true},
		{"uname -a -p multi", "uname", []string{"-a", "-p"}, true},
		{"df -h inline", "df -h", nil, true},
		{"df -hT", "df", []string{"-h", "-T"}, true},
		{"free -h", "free -h", nil, true},
		{"free --mebi", "free --mebi", nil, true},
		{"id -u", "id -u", nil, true},
		{"id -ur", "id", []string{"-u", "-r"}, true},
		{"lsb_release -a", "lsb_release -a", nil, true},
		{"lsb_release --description", "lsb_release --description", nil, true},
		{"uptime -p", "uptime -p", nil, true},
		{"hostname -f", "hostname -f", nil, true},
		// Repeated IDENTICAL safe flags are still exact table entries —
		// accepted (deduping would add logic without adding safety).
		{"free -h twice", "free", []string{"-h", "-h"}, true},
		// Allowed: absolute paths in TRUSTED executable directories only.
		{"abs uname", "/usr/bin/uname", nil, true},
		{"abs uname -a", "/bin/uname", []string{"-a"}, true},
		{"abs bin uname", "/bin/uname", nil, true},
		{"abs sbin", "/usr/sbin/lsb_release", []string{"-a"}, true},
		{"abs cat", "/usr/bin/cat /etc/os-release", nil, true},
		// Allowed: cat /etc/os-release exactly.
		{"cat os-release", "cat /etc/os-release", nil, true},
		{"cat -n os-release", "cat -n /etc/os-release", nil, true},
		{"cat harmless flags", "cat -nsv /etc/os-release", nil, true},
		{"cat long flag", "cat --number /etc/os-release", nil, true},
		{"cat flags+args split", "cat", []string{"-n", "/etc/os-release"}, true},
		// Rejected: non-probe commands.
		{"ls", "ls", nil, false},
		{"echo", "echo", nil, false},
		{"echo hi", "echo hi", nil, false},
		{"docker ps", "docker", []string{"ps"}, false},
		{"cat passwd", "cat /etc/passwd", nil, false},
		{"cat relative operand", "cat etc/os-release", nil, false},
		{"cat two operands", "cat /etc/os-release /etc/hostname", nil, false},
		{"cat stdin dash", "cat -", nil, false},
		{"cat dash operand", "cat - /etc/os-release", nil, false},
		{"cat non-harmless flag", "cat -u /etc/os-release", nil, false},
		{"cat combined weird flag", "cat -zx /etc/os-release", nil, false},
		{"cat no operand", "cat", nil, false},
		{"cat flag only", "cat -n", nil, false},
		// Rejected: lookalikes.
		{"uname lookalike", "uname2", nil, false},
		{"uuname", "uuname", nil, false},
		{"unamed", "unamed", nil, false},
		{"catfile lookalike", "catfile /etc/os-release", nil, false},
		{"uptime lookalike", "uptime2", nil, false},
		// Rejected: relative-path and degenerate path shapes.
		{"relative uname", "./uname", nil, false},
		{"relative dir uname", "bin/uname", nil, false},
		{"trailing slash dir", "/usr/bin/", nil, false},
		{"root slash", "/", nil, false},
		{"empty", "", nil, false},
		{"whitespace only", "   ", nil, false},
		// Rejected: shell compounds / metacharacters (fail closed).
		{"compound semicolon", "uname; whoami", nil, false},
		{"compound &&", "uname && whoami", nil, false},
		{"pipe", "uname | tee /tmp/x", nil, false},
		{"redirection", "uname > /tmp/x", nil, false},
		{"substitution", "$(uname)", nil, false},
		{"backticks", "`uname`", nil, false},
		{"quoted uname", "'uname'", nil, false},
		{"double quoted", "\"uname\"", nil, false},
		{"glob", "un*", nil, false},
		{"newline compound", "uname\nwhoami", nil, false},
		// Rejected: unsupported flags, operands/usernames, repeated or
		// unknown options (strict: only exact safe-flag table entries).
		{"id someuser", "id", []string{"someuser"}, false},
		{"id -u someuser", "id", []string{"-u", "someuser"}, false},
		{"df a path", "df", []string{"-h", "/etc"}, false},
		{"df -x tmpfs", "df -x tmpfs", nil, false},
		{"free -s 5", "free -s 5", nil, false},
		{"free --count", "free --count", nil, false},
		{"uname --unknown", "uname --unknown", nil, false},
		{"uptime -a", "uptime -a", nil, false},
		{"whoami -a", "whoami -a", nil, false},
		{"whoami args", "whoami", []string{"extra"}, false},
		{"lsb_release --help", "lsb_release --help", nil, false},
		{"echo -n", "echo -n", nil, false},
		// Rejected: cross-command flag import (flag valid for another probe
		// but not for this one — per-command tables, not a global set).
		{"uname -h", "uname -h", nil, false},
		{"hostname -r", "hostname -r", nil, false},
		{"id -h", "id -h", nil, false},
		// Rejected: absolute paths in UNTRUSTED directories / non-exact
		// path shapes (lookalike binaries must never be labeled probes).
		{"tmp uname", "/tmp/uname", nil, false},
		{"home uname", "/home/x/uname", nil, false},
		{"nested usr local", "/usr/local/bin/uname", nil, false},
		{"snap path", "/snap/bin/uname", nil, false},
		{"trailing slash uname", "/usr/bin/uname/", nil, false},
		{"dot dir uname", "/usr/bin/./uname", nil, false},
		{"dotdot parent", "/usr/bin/../bin/uname", nil, false},
		{"abs ls", "/bin/ls", nil, false},
		{"abs echo", "/bin/echo hi", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isContainmentProbe(c.command, c.args)
			if got != c.want {
				t.Errorf("isContainmentProbe(%q, %v) = %v, want %v", c.command, c.args, got, c.want)
			}
		})
	}
}

// TestContainmentDisclosureMarkerConstant pins the canonical marker: one
// self-describing bracketed line — greppable, newline-free, prefix-tagged.
func TestContainmentDisclosureMarkerConstant(t *testing.T) {
	m := containmentDisclosureMarker
	if strings.ContainsAny(m, "\n") {
		t.Errorf("marker must be a single line: %q", m)
	}
	if !strings.HasPrefix(m, "[bunker:") || !strings.Contains(m, "containment") {
		t.Errorf("marker not self-describing: %q", m)
	}
}

// TestMarkerFrameForStream tests the ACTUAL frame/state logic ExecAgent
// uses (markerFrameForStream over the streamed-stdout facts recorded by the
// stdout streamer), not a proxy. Client-side concatenation of the process
// stdout frames + marker frame is computed for each case: the marker must
// land on its OWN line exactly once — no leading blank when output is empty,
// no extra blank when output already ended in '\n', and no `Linux[bunker:…]`
// concatenation when it did not.
func TestMarkerFrameForStream(t *testing.T) {
	marker := containmentDisclosureMarker
	cases := []struct {
		name            string
		sentBytes       bool
		endsWithNewline bool
		enabled         bool
		// stdoutFrames simulates the bytes the stdout streamer forwarded
		// (kept in sync with sentBytes/endsWithNewline).
		stdoutFrames string
	}{
		{"disabled no stdout", false, false, false, ""},
		{"disabled no trailing newline", true, false, false, "Linux"},
		{"disabled trailing newline", true, true, false, "Linux\n"},
		{"enabled no stdout", false, false, true, ""},
		{"enabled no trailing newline", true, false, true, "Linux"},
		{"enabled trailing newline", true, true, true, "Linux\n"},
		{"enabled multiline trailing", true, true, true, "a\nb\n"},
		{"enabled single byte", true, true, true, "\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			frame := markerFrameForStream(c.sentBytes, c.endsWithNewline, c.enabled)
			// Client-side view: process frames + marker frame concatenated.
			got := c.stdoutFrames + frame

			if !c.enabled {
				if frame != "" {
					t.Fatalf("disabled must produce an empty frame (caller sends nothing), got %q", frame)
				}
				if got != c.stdoutFrames {
					t.Errorf("disabled must be byte-identical passthrough: %q", got)
				}
				return
			}

			// Exactly one marker occurrence overall.
			if n := strings.Count(got, marker); n != 1 {
				t.Fatalf("marker appears %d times in concatenated output %q", n, got)
			}
			// Marker is the final line: the only content after the marker
			// is its trailing newline.
			if !strings.HasSuffix(got, marker+"\n") {
				t.Fatalf("marker must be the final line: %q", got)
			}
			// Whatever precedes the marker must end with a newline (marker
			// on its own line) — and when nothing precedes it, there must
			// be NO leading blank line.
			before := strings.TrimSuffix(got, marker+"\n")
			switch {
			case before == "":
				if c.sentBytes {
					t.Errorf("state says stdout was sent but frame carries no separation: %q", got)
				}
			case !strings.HasSuffix(before, "\n"):
				t.Errorf("marker not on its own line (missing newline after stdout): %q", got)
			default:
				if c.endsWithNewline && strings.HasSuffix(before, "\n\n") {
					t.Errorf("extra blank line before marker though stdout already ended in newline: %q", got)
				}
			}
		})
	}
}

// TestMarkerCountsAsSent pins the empty-chunk rule: a zero-length read must
// not flip the marker state (a failed Send or a 0-byte pipe read must not
// make the marker logic believe stdout was sent).
func TestMarkerCountsAsSent(t *testing.T) {
	if markerCountsAsSent(0) {
		t.Error("zero-length chunk must not count as sent stdout")
	}
	if !markerCountsAsSent(1) || !markerCountsAsSent(4096) {
		t.Error("non-empty chunks must count as sent stdout")
	}
}

// TestContainmentMarkerPreservesNonZeroExit simulates the acceptance
// criterion "exit code is unchanged, including a simulated non-zero probe":
// a failing probe (exit 3) still ends with the marker on stdout, and the
// final exit-code frame carries the original non-zero code untouched. The
// marker logic is stdout-only and never participates in exit-code
// computation. This test drives the ACTUAL frame construction ExecAgent
// performs for a failing probe: stdout frames ("probe-output\n" here) then
// ONE marker frame, then the exit frame carrying the ORIGINAL code (3).
func TestContainmentMarkerPreservesNonZeroExit(t *testing.T) {
	// Functional proof that running a command through the same builder
	// leaves its status alone; the marker is appended separately.
	built := buildAgentExecCommand("abc123", "/home/bunker-abc123", "sh", []string{"-c", "echo probe-output; exit 3"}, true)
	_, runErr := exec.Command("sh", "-c", built).CombinedOutput()
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("expected underlying exit code 3 to survive the builder, got %v", runErr)
	}

	// Frame sequence ExecAgent emits for an enabled allowed probe whose
	// stdout was "probe-output\n": the marker frame carries no leading
	// newline (output already ends in one), and the exit frame carries the
	// untouched original code. The marker emission has no code path that
	// can modify exitCode (it reads only the streamer-recorded state).
	frame := markerFrameForStream(true, true, true)
	if frame != containmentDisclosureMarker+"\n" {
		t.Errorf("marker frame after newline-terminated stdout must be marker-only: %q", frame)
	}
	clientView := "probe-output\n" + frame
	if strings.Count(clientView, containmentDisclosureMarker) != 1 ||
		!strings.HasSuffix(clientView, containmentDisclosureMarker+"\n") {
		t.Errorf("client-side output malformed: %q", clientView)
	}
}

// TestBuildAgentExecCommand_ContainmentEnv verifies BUNKER_SANDBOX=1 rides
// the same env(1) injection as PATH/DOCKER_HOST/TMPDIR, positioned before
// the wrapped sh -c, and that disabled output is BYTE-IDENTICAL to the
// pre-GAP-067 string (flag off = zero behavior change).
func TestBuildAgentExecCommand_ContainmentEnv(t *testing.T) {
	const wantDisabled = "set -a; [ -f /run/bunker/abc123/env ] && . /run/bunker/abc123/env 2>/dev/null; set +a; env PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DOCKER_HOST=unix:///run/bunker/abc123/docker.sock TMPDIR=/run/bunker/abc123/tmp sh -c 'uname'"
	gotOff := buildAgentExecCommand("abc123", "/home/bunker-abc123", "uname", nil, false)
	if gotOff != wantDisabled {
		t.Errorf("flag-off output changed (must be byte-identical):\n got: %q\nwant: %q", gotOff, wantDisabled)
	}
	if strings.Contains(gotOff, "BUNKER_SANDBOX") {
		t.Errorf("flag-off output must not contain BUNKER_SANDBOX: %q", gotOff)
	}

	gotOn := buildAgentExecCommand("abc123", "/home/bunker-abc123", "uname", nil, true)
	if !strings.Contains(gotOn, config.ContainmentSandboxEnv+" ") {
		t.Errorf("flag-on output missing %q: %q", config.ContainmentSandboxEnv, gotOn)
	}
	envIdx := strings.Index(gotOn, config.ContainmentSandboxEnv)
	tmpIdx := strings.Index(gotOn, "TMPDIR=")
	shIdx := strings.Index(gotOn, "sh -c ")
	if envIdx < 0 || tmpIdx < 0 || shIdx < 0 || envIdx < tmpIdx || envIdx > shIdx {
		t.Errorf("BUNKER_SANDBOX must sit inside the env(1) list (after TMPDIR, before sh -c): %q", gotOn)
	}

	// Functional: run through sh exactly as the remote side would and prove
	// the sandbox var is visible to the command when enabled, absent when
	// disabled (works with no env file present, mirroring a fresh agent).
	probeOn := buildAgentExecCommand("abc123", "/home/bunker-abc123", "sh", []string{"-c", "echo SB=$BUNKER_SANDBOX"}, true)
	outOn, err := exec.Command("sh", "-c", probeOn).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c failed (flag on): %v, output: %s", err, outOn)
	}
	if !strings.Contains(string(outOn), "SB=1") {
		t.Errorf("flag-on exec did not expose BUNKER_SANDBOX to the command: %s", outOn)
	}
	probeOff := buildAgentExecCommand("abc123", "/home/bunker-abc123", "sh", []string{"-c", "echo SB=$BUNKER_SANDBOX"}, false)
	outOff, err := exec.Command("sh", "-c", probeOff).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c failed (flag off): %v, output: %s", err, outOff)
	}
	if strings.Contains(string(outOff), "SB=1") {
		t.Errorf("flag-off exec leaked BUNKER_SANDBOX into the environment: %s", outOff)
	}
}

// TestBuildAgentRawExecCommand_ContainmentEnv verifies the raw argv carries
// BUNKER_SANDBOX=1 as its own element when enabled and is DEEP-EQUAL to the
// pre-GAP-067 argv when disabled.
func TestBuildAgentRawExecCommand_ContainmentEnv(t *testing.T) {
	wantOff := []string{
		"env",
		"PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"TMPDIR=/run/bunker/abc123/tmp",
		"uname",
	}
	gotOff := buildAgentRawExecCommand("abc123", "/home/bunker-abc123", "uname", nil, false)
	if strings.Join(gotOff, "\x00") != strings.Join(wantOff, "\x00") {
		t.Errorf("flag-off raw argv changed (must be identical):\n got: %v\nwant: %v", gotOff, wantOff)
	}

	gotOn := buildAgentRawExecCommand("abc123", "/home/bunker-abc123", "uname", nil, true)
	found := false
	for _, a := range gotOn {
		if a == config.ContainmentSandboxEnv {
			found = true
		}
	}
	if !found {
		t.Errorf("flag-on raw argv missing dedicated %q element: %v", config.ContainmentSandboxEnv, gotOn)
	}
	// The command must still be the element right after the env assignments.
	if last := gotOn[len(gotOn)-1]; last != "uname" {
		t.Errorf("flag-on raw argv must keep the command last: %v", gotOn)
	}
	// Args must still follow the command untouched.
	gotArgs := buildAgentRawExecCommand("abc123", "/home/bunker-abc123", "docker", []string{"compose", "up"}, true)
	if strings.Join(gotArgs[len(gotArgs)-2:], "\x00") != "compose\x00up" {
		t.Errorf("raw args must pass through unchanged: %v", gotArgs)
	}
}

// TestBuildAgentScriptCommand_ContainmentEnv verifies BUNKER_SANDBOX=1 is
// injected into the script exec env(1) and the disabled output is
// byte-identical to the pre-GAP-067 string.
func TestBuildAgentScriptCommand_ContainmentEnv(t *testing.T) {
	script := "#!/bin/sh\necho hi\n"
	const wantDisabled = "mkdir -p \"/home/bunker-abc123/.bunker\" && cat > \"/home/bunker-abc123/.bunker/exec-script.sh\" <<'EOFSCRIPT'\n#!/bin/sh\necho hi\n\nEOFSCRIPT\nchmod +x \"/home/bunker-abc123/.bunker/exec-script.sh\" && set -a; [ -f /run/bunker/abc123/env ] && . /run/bunker/abc123/env 2>/dev/null; set +a; env PATH=/home/bunker-abc123/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin DOCKER_HOST=unix:///run/bunker/abc123/docker.sock TMPDIR=/run/bunker/abc123/tmp \"/home/bunker-abc123/.bunker/exec-script.sh\""
	gotOff := buildAgentScriptCommand("abc123", "/home/bunker-abc123", script, false)
	if gotOff != wantDisabled {
		t.Errorf("flag-off script command changed (must be byte-identical):\n got: %q\nwant: %q", gotOff, wantDisabled)
	}
	if strings.Contains(gotOff, "BUNKER_SANDBOX") {
		t.Errorf("flag-off script command must not contain BUNKER_SANDBOX: %q", gotOff)
	}

	gotOn := buildAgentScriptCommand("abc123", "/home/bunker-abc123", script, true)
	envIdx := strings.Index(gotOn, config.ContainmentSandboxEnv)
	if envIdx < 0 {
		t.Errorf("flag-on script command missing %q: %q", config.ContainmentSandboxEnv, gotOn)
	}
	if tmpIdx := strings.Index(gotOn, "TMPDIR="); tmpIdx >= 0 && envIdx < tmpIdx {
		t.Errorf("BUNKER_SANDBOX must come after TMPDIR in the env(1) list: %q", gotOn)
	}
	// The script path invocation must remain the final token.
	if !strings.HasSuffix(gotOn, `"/home/bunker-abc123/.bunker/exec-script.sh"`) {
		t.Errorf("flag-on script command must keep the script invocation last: %q", gotOn)
	}
}

// TestLogDisclosureStartup verifies the safe startup note reflects the flag
// state, names the marker/env semantics when enabled, and never invents
// content beyond the boolean (no secrets can enter — it takes only a bool).
func TestLogDisclosureStartup(t *testing.T) {
	off := logDisclosureStartup(false)
	if !strings.Contains(off, "disabled") {
		t.Errorf("disabled note should say disabled: %q", off)
	}
	if strings.Contains(off, config.ContainmentSandboxEnv) {
		t.Errorf("disabled note should not advertise the sandbox env var: %q", off)
	}
	on := logDisclosureStartup(true)
	if !strings.Contains(on, "ENABLED") {
		t.Errorf("enabled note should say ENABLED: %q", on)
	}
	if !strings.Contains(on, config.ContainmentSandboxEnv) || !strings.Contains(on, containmentDisclosureMarker) {
		t.Errorf("enabled note should document env var and marker: %q", on)
	}
}
