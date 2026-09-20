package cli

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The tests below pin the SURF-006 contract at the unit level: what the
// builders emit, what the parsers accept and refuse, and the named-failure
// mapping. The live-agent proof is the foreman's merge gate; these tests make
// the mechanism reviewable without one.

// TestSurfaceUnitContentIsTheProvenShape guards the exact unit text proven live
// by tools/surf-mechanism-proof.sh: the specifiers (%t/%h), the socket mode,
// Accept=yes, the socket-activated stdio, and the enable target.
func TestSurfaceUnitContentIsTheProvenShape(t *testing.T) {
	for _, want := range []string{
		"ListenStream=%t/toolsd.sock",
		"SocketMode=0600",
		"Accept=yes",
		"WantedBy=sockets.target",
	} {
		if !strings.Contains(surfaceSocketUnitContent, want) {
			t.Errorf("surfaceSocketUnitContent lost %q:\n%s", want, surfaceSocketUnitContent)
		}
	}
	for _, want := range []string{
		"ExecStart=%h/bin/toolsd mcp",
		"StandardInput=socket",
		"StandardOutput=socket",
	} {
		if !strings.Contains(surfaceServiceUnitContent, want) {
			t.Errorf("surfaceServiceUnitContent lost %q:\n%s", want, surfaceServiceUnitContent)
		}
	}
}

// TestBuildSurfaceInstallScript walks the generated install script: both unit
// files written under the agent's own per-user unit dir, the exact unit text
// carried verbatim, and the daemon-reload + enable + start steps present in a
// sane order.
func TestBuildSurfaceInstallScript(t *testing.T) {
	script := buildSurfaceInstallScript()
	for _, want := range []string{
		`"$HOME/.config/systemd/user"`,
		"toolsd.socket",
		"toolsd@.service",
		quoteShell(surfaceSocketUnitContent),
		quoteShell(surfaceServiceUnitContent),
		"systemctl --user daemon-reload",
		"systemctl --user enable toolsd.socket",
		"systemctl --user start toolsd.socket",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script lost %q:\n%s", want, script)
		}
	}
	reload := strings.Index(script, "systemctl --user daemon-reload")
	write := strings.LastIndex(script, "write_unit ")
	if write == -1 || reload == -1 || reload < write {
		t.Errorf("install script must write units BEFORE daemon-reload:\n%s", script)
	}
}

// TestBuildSurfaceInstallScriptIdempotentWrite is the idempotency contract at
// the script level: write_unit must COMPARE before writing and skip an
// identical file (the "unchanged" path a re-run exercises), and a differing
// file must be replaced atomically (temp file in the same dir + rename).
func TestBuildSurfaceInstallScriptIdempotentWrite(t *testing.T) {
	script := buildSurfaceInstallScript()
	for _, want := range []string{
		`have=$(cat "$2" 2>/dev/null || true)`,     // read current content
		`if [ "$have" = "$want" ]; then`,           // compare BEFORE writing
		"unchanged",                                // the skip path is reported
		`tmp=$(mktemp "$dir/.toolsd-unit.XXXXXX")`, // atomic: temp in same dir
		"mv -f \"$tmp\" \"$2\"",                    // atomic: rename over target
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script lost idempotency step %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, "set -eu") {
		t.Errorf("install script must abort on the first failed step (set -eu):\n%s", script)
	}
}

// TestBuildSurfaceRemoveScript walks the generated remove script: stop,
// disable, remove BOTH files, reload — in that order — with an absence
// verification at the end.
func TestBuildSurfaceRemoveScript(t *testing.T) {
	script := buildSurfaceRemoveScript()
	for _, want := range []string{
		"systemctl --user stop toolsd.socket",
		"systemctl --user disable toolsd.socket",
		`rm -f "$s" "$v"`,
		"systemctl --user daemon-reload",
		"/run/user/$(id -u)/toolsd.sock",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("remove script lost %q:\n%s", want, script)
		}
	}
	stop := strings.Index(script, "systemctl --user stop")
	disable := strings.Index(script, "systemctl --user disable")
	rm := strings.Index(script, `rm -f "$s" "$v"`)
	reload := strings.LastIndex(script, "systemctl --user daemon-reload")
	if !(stop < disable && disable < rm && rm < reload) {
		t.Errorf("remove script steps out of order (stop < disable < rm < reload):\n%s", script)
	}
	// Both unit names must be bound to the variables the rm deletes.
	for _, want := range []string{surfaceSocketUnitName, surfaceServiceUnitName} {
		if !strings.Contains(script, `"$dir/`+want+`"`) {
			t.Errorf("remove script must bind %s to a deleted path:\n%s", want, script)
		}
	}
}

// TestQuoteShell: unit text must survive the shell as one literal word.
func TestQuoteShell(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "'plain'"},
		{"it's", `'it'\''s'`},
		{"", "''"},
	}
	for _, c := range cases {
		if got := quoteShell(c.in); got != c.want {
			t.Errorf("quoteShell(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The real unit content contains no quotes but must round-trip inside the
	// install script without breaking its quoting.
	if !strings.Contains(buildSurfaceInstallScript(), quoteShell(surfaceSocketUnitContent)) {
		t.Error("socket unit content must be carried verbatim inside the install script")
	}
}

// TestParseSurfaceResultLine: the RESULT line is the verify contract. Missing
// fields, malformed fields and non-RESULT lines must never parse as success.
func TestParseSurfaceResultLine(t *testing.T) {
	st, ok := parseSurfaceResultLine("RESULT\tstate=active\tsocket=/run/user/1001/toolsd.sock\towner=bunker-abc\tmode=600")
	if !ok {
		t.Fatal("a well-formed RESULT line must parse")
	}
	if st.State != "active" || st.SocketPath != "/run/user/1001/toolsd.sock" ||
		st.Owner != "bunker-abc" || st.Mode != "600" {
		t.Errorf("parsed state wrong: %+v", st)
	}

	for _, bad := range []string{
		"",
		"RESULT",                            // no fields at all
		"RESULT\tsocket=/run/user/1/x.sock", // no state=
		"RESULT\tstate",                     // malformed field
		"some other output line",            // not a RESULT line
	} {
		if _, ok := parseSurfaceResultLine(bad); ok {
			t.Errorf("parseSurfaceResultLine(%q) must not parse", bad)
		}
	}
}

// TestParseSurfaceVerifyOutput: the RESULT line is findable among the noise a
// real agent prints, and output without one reports not-found.
func TestParseSurfaceVerifyOutput(t *testing.T) {
	wire := "Created symlink /home/bunker-a/.config/systemd/user/sockets.target.wants/toolsd.socket \u2192 /home/bunker-a/.config/systemd/user/toolsd.socket.\n" +
		"RESULT\tstate=active\tsocket=/run/user/1001/toolsd.sock\towner=bunker-a\tmode=600\n"
	st, ok := parseSurfaceVerifyOutput(wire)
	if !ok {
		t.Fatal("the RESULT line must be found among other output")
	}
	if st.State != "active" || st.Owner != "bunker-a" {
		t.Errorf("wrong state parsed: %+v", st)
	}
	if _, ok := parseSurfaceVerifyOutput("no result here\n"); ok {
		t.Error("output without a RESULT line must report not-found")
	}
}

// TestSurfaceInstallWrote: a re-run over an identical surface must read as
// unchanged (the idempotency evidence); any written unit reads as changed.
func TestSurfaceInstallWrote(t *testing.T) {
	if surfaceInstallWrote("UNIT\t/x/toolsd.socket\tunchanged\nUNIT\t/x/toolsd@.service\tunchanged\n") {
		t.Error("all-unchanged install must not report changed")
	}
	if !surfaceInstallWrote("UNIT\t/x/toolsd.socket\twritten\n") {
		t.Error("a written unit must report changed")
	}
	if !surfaceInstallWrote("UNIT\t/x/toolsd.socket\twritten") {
		t.Error("a written unit without trailing newline must still report changed")
	}
	if surfaceInstallWrote("") {
		t.Error("empty install output must not report changed")
	}
}

// TestParseSurfaceRemoveOutput: removal verdicts parse from the RESULT line.
func TestParseSurfaceRemoveOutput(t *testing.T) {
	path, state, ok := parseSurfaceRemoveOutput("RESULT\tsocket=/run/user/1001/toolsd.sock\tstate=removed\tunits=deleted\n")
	if !ok || state != "removed" || path != "/run/user/1001/toolsd.sock" {
		t.Errorf("removed verdict misparsed: (%q, %q, %v)", path, state, ok)
	}
	_, state, ok = parseSurfaceRemoveOutput("NOTE\tstop: not loaded.\nRESULT\tsocket=/run/user/1001/toolsd.sock\tstate=still-present\tunits=deleted\n")
	if !ok || state != "still-present" {
		t.Errorf("still-present verdict misparsed: (%q, %v)", state, ok)
	}
	if _, _, ok := parseSurfaceRemoveOutput("garbage"); ok {
		t.Error("output without a RESULT line must not parse")
	}
}

// TestParseSurfaceIsRunning picks the single status word out of preflight
// output, ignoring empty and CR-carrying lines.
func TestParseSurfaceIsRunning(t *testing.T) {
	cases := []struct{ in, want string }{
		{"running\n", "running"},
		{"degraded\n", "degraded"},
		{"\n\noffline\n", "offline"},
		{"running\r\n", "running"},
		{"", ""},
		{"\n", ""},
	}
	for _, c := range cases {
		if got := parseSurfaceIsRunning(c.in); got != c.want {
			t.Errorf("parseSurfaceIsRunning(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsUserManagerUnavailable draws the line the acceptance criterion names: a
// non-running user manager is detected (named), while running and degraded are
// a live manager and must proceed.
func TestIsUserManagerUnavailable(t *testing.T) {
	for _, line := range []string{
		"offline",
		"Failed to connect to bus: No such file or directory",
		"System has not been booted with systemd",
		"connection refused",
		"Failed to start manager: lock is held",
	} {
		if !isUserManagerUnavailable(line) {
			t.Errorf("%q must classify as USER MANAGER UNAVAILABLE", line)
		}
	}
	for _, line := range []string{"running", "degraded", ""} {
		if isUserManagerUnavailable(line) {
			t.Errorf("%q is a live manager; must not classify as unavailable", line)
		}
	}
}

// TestClassifySurfaceManagerFailure: the named-failure criterion. Every dead
// manager output maps to an error that NAMES the cause and the check, never a
// generic exit.
func TestClassifySurfaceManagerFailure(t *testing.T) {
	for _, out := range []string{
		"offline\n",
		"Failed to connect to bus: No such file or directory\n",
		"",
	} {
		err := classifySurfaceManagerFailure("agent7", out)
		if err == nil {
			t.Fatalf("classifySurfaceManagerFailure(%q) must fail", out)
		}
		msg := err.Error()
		for _, want := range []string{"agent7", "user manager", "Linger"} {
			if !strings.Contains(msg, want) {
				t.Errorf("failure for %q must name %q, got: %v", out, want, msg)
			}
		}
	}
	if err := classifySurfaceManagerFailure("agent7", "weirdstate\n"); err == nil ||
		!strings.Contains(err.Error(), "weirdstate") {
		t.Errorf("an unmodelled state must still fail WITH the state named, got: %v", err)
	}
}

// TestSurfaceToolsdProbeParsing: the dependency probe reports present/absent,
// and anything unreadable is unknown — never silently present.
func TestSurfaceToolsdProbeParsing(t *testing.T) {
	if got := parseSurfaceToolsdProbe("present\n"); got != "present" {
		t.Errorf("present misread as %q", got)
	}
	if got := parseSurfaceToolsdProbe("absent\r\n"); got != "absent" {
		t.Errorf("absent misread as %q", got)
	}
	if got := parseSurfaceToolsdProbe("connection lost"); got != "unknown" {
		t.Errorf("garbage must read unknown, got %q", got)
	}
}

// TestSurfaceToolsdProbeScript pins the probe to the executable check the
// warning names: $HOME/bin/toolsd.
func TestSurfaceToolsdProbeScript(t *testing.T) {
	if !strings.Contains(surfaceToolsdProbeScript, `"$HOME/bin/toolsd"`) {
		t.Errorf("probe must check $HOME/bin/toolsd:\n%s", surfaceToolsdProbeScript)
	}
}

// TestTailSurfaceOutput: errors carry the agent's own words, bounded.
func TestTailSurfaceOutput(t *testing.T) {
	if got := tailSurfaceOutput("short", 100); got != "short" {
		t.Errorf("short output must pass through, got %q", got)
	}
	long := strings.Repeat("x", 600)
	got := tailSurfaceOutput(long, 500)
	// "…" is 3 bytes in UTF-8; count runes so the bound is in characters.
	if utf8.RuneCountInString(got) != 501 || !strings.HasPrefix(got, "…") {
		t.Errorf("long output must be tail-bounded with an ellipsis marker, got %d runes prefix %q", utf8.RuneCountInString(got), got[:1])
	}
}
