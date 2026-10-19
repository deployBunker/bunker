package cli

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The tests below pin the SURF-006 + SURF-B2 contract at the unit level: what
// the builders emit, what the parsers accept and refuse, and the named-failure
// mapping. The live-agent proof is the foreman's merge gate; these tests make
// the mechanism reviewable without one.

// TestSurfaceUnitContentIsTheProvenShape guards the exact unit text the
// mechanism relies on: the runtime-dir ListenStream built from %i (SURF-B2),
// the socket mode, Accept=yes, the socket-activated stdio, and the enable
// target. The service template keeps %h (installed through the agent's own
// exec context, so %h IS the agent's home) and %i for per-agent descriptions.
func TestSurfaceUnitContentIsTheProvenShape(t *testing.T) {
	socketUnit := renderSurfaceSocketUnit("abc123")
	for _, want := range []string{
		"ListenStream=/run/bunker/abc123/toolsd.sock",
		"SocketMode=0600",
		"Accept=yes",
		"WantedBy=sockets.target",
	} {
		if !strings.Contains(socketUnit, want) {
			t.Errorf("socket unit lost %q:\n%s", want, socketUnit)
		}
	}
	// %i substitution must be complete — a literal %i left in rendered text
	// would address a directory literally named %i.
	if strings.Contains(socketUnit, "%i") {
		t.Errorf("rendered socket unit still carries a literal %%i:\n%s", socketUnit)
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

// TestRenderSurfaceSocketUnitPerAgent: each agent gets its OWN runtime-dir
// path, and a different id never renders another agent's path.
func TestRenderSurfaceSocketUnitPerAgent(t *testing.T) {
	a := renderSurfaceSocketUnit("abc123")
	b := renderSurfaceSocketUnit("deadbeef")
	if !strings.Contains(a, "ListenStream=/run/bunker/abc123/toolsd.sock") {
		t.Errorf("unit for abc123 wrong:\n%s", a)
	}
	if !strings.Contains(b, "ListenStream=/run/bunker/deadbeef/toolsd.sock") {
		t.Errorf("unit for deadbeef wrong:\n%s", b)
	}
	if strings.Contains(b, "abc123") {
		t.Errorf("unit for deadbeef leaked abc123:\n%s", b)
	}
}

// TestValidSurfaceAgentID: the id rule mirrors the daemon's spawn-time regex
// (^$ → named error; [a-z0-9-] only; ≤63 chars) because builders interpolate
// the id into unit text and absolute paths.
func TestValidSurfaceAgentID(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"abc123", false},
		{"dead-beef-01", false},
		{"a", false},
		{"", true},
		{"UPPER", true},
		{"with.dot", true},
		{"with/slash", true},
		{"with space", true},
		{"with%percent", true},
		{"with$i", true},
		{"with'quote", true},
		{"with;semicolon", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 63), false},
	}
	for _, c := range cases {
		err := validSurfaceAgentID(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("validSurfaceAgentID(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
		}
	}
}

// TestSurfaceToolsdSocketPath: the canonical remote path composes from the
// validated id only.
func TestSurfaceToolsdSocketPath(t *testing.T) {
	if got := surfaceToolsdSocketPath("abc123"); got != "/run/bunker/abc123/toolsd.sock" {
		t.Errorf("surfaceToolsdSocketPath = %q", got)
	}
}

// TestBuildSurfaceInstallScript walks the generated install script: the
// runtime dir prepared 0700 with a stat read-back BEFORE the units, both unit
// files written under the agent's own per-user unit dir, the exact unit text
// carried verbatim, and the daemon-reload + enable + start steps present in a
// sane order.
func TestBuildSurfaceInstallScript(t *testing.T) {
	script := buildSurfaceInstallScript("abc123")
	for _, want := range []string{
		`rundir="/run/bunker/abc123"`,
		`mkdir -p "$rundir"`,
		`chmod 700 "$rundir"`,
		`gotmode=$(stat -c '%a' "$rundir")`,
		`if [ "$gotmode" != "700" ]; then`, // the read-back is the ABORT, not a note
		`exit 7`,                           // named failure: the dir is wider than 0700
		`"$HOME/.config/systemd/user"`,
		"toolsd.socket",
		"toolsd@.service",
		quoteShell(renderSurfaceSocketUnit("abc123")),
		quoteShell(surfaceServiceUnitContent),
		"systemctl --user daemon-reload",
		"systemctl --user enable toolsd.socket",
		"systemctl --user start toolsd.socket",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script lost %q:\n%s", want, script)
		}
	}
	// The per-agent runtime dir must never be widened: no chmod that grants
	// group/world bits may appear for it.
	if strings.Contains(script, "chmod 770") || strings.Contains(script, "chmod 777") ||
		strings.Contains(script, "chmod 755") {
		t.Errorf("install script widens the runtime dir:\n%s", script)
	}
	// The socket's existence is PROVEN after start, at the runtime-dir path.
	if !strings.Contains(script, `if [ -S '/run/bunker/abc123/toolsd.sock' ]; then`) {
		t.Errorf("install script must prove the runtime-dir socket after start:\n%s", script)
	}
	if !strings.Contains(script, "exit 8") {
		t.Errorf("install script must fail loudly (exit 8) when the socket did not appear:\n%s", script)
	}
	reload := strings.Index(script, "systemctl --user daemon-reload")
	write := strings.LastIndex(script, "write_unit ")
	if write == -1 || reload == -1 || reload < write {
		t.Errorf("install script must write units BEFORE daemon-reload:\n%s", script)
	}
	// Runtime-dir prep must precede the unit writes (the units reference it).
	mkdir := strings.Index(script, `mkdir -p "$rundir"`)
	if mkdir == -1 || mkdir > write {
		t.Errorf("install script must prepare the runtime dir BEFORE writing units:\n%s", script)
	}
}

// TestBuildSurfaceInstallScriptNoLoginHook is the SURF-B2 load-bearing
// constraint at the text level: NOTHING in the install path may touch the
// SSH login path (no .ssh/authorized_keys edit, no .profile hook, no
// ForceCommand, no PAM artifact). bunker exec/cp/deploy WORK by running
// commands over SSH — a login hook would break the most-used path in the
// product. The script may only: mkdir/chmod the runtime dir, write unit
// files, and drive systemctl --user.
func TestBuildSurfaceInstallScriptNoLoginHook(t *testing.T) {
	for _, id := range []string{"abc123", "x1"} {
		script := buildSurfaceInstallScript(id)
		for _, forbidden := range []string{
			"authorized_keys", ".profile", "ForceCommand", "sshd_config",
			"pam_", "Match User", " PermitUserEnvironment",
		} {
			if strings.Contains(script, forbidden) {
				t.Errorf("install script (%s) touches the login path with %q:\n%s", id, forbidden, script)
			}
		}
	}
}

// TestBuildSurfaceInstallScriptIdempotentWrite is the idempotency contract at
// the script level: write_unit must COMPARE before writing and skip an
// identical file (the "unchanged" path a re-run exercises), and a differing
// file must be replaced atomically (temp file in the same dir + rename).
func TestBuildSurfaceInstallScriptIdempotentWrite(t *testing.T) {
	script := buildSurfaceInstallScript("abc123")
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

// TestBuildSurfaceInstallScriptMigratesLegacyUnit: a pre-SURF-B2 agent carries
// the old ListenStream=%%t unit; the idempotent write must REPLACE it. That
// works precisely because the rendered NEW unit differs from the legacy text —
// byte-equality would make write_unit keep the old address forever.
func TestBuildSurfaceInstallScriptMigratesLegacyUnit(t *testing.T) {
	// The rendered NEW unit is not the legacy one — the compare-then-write in
	// write_unit rewrites the file precisely because the contents differ.
	if renderSurfaceSocketUnit("abc123") == surfaceLegacySocketUnitContent {
		t.Fatal("the new unit is byte-identical to the legacy unit — migration would never rewrite the old address")
	}
	// And the new unit carries the runtime-dir address, the legacy one the %t
	// address — the property that makes an old install migrate on re-install.
	if !strings.Contains(renderSurfaceSocketUnit("abc123"), "ListenStream=/run/bunker/abc123/toolsd.sock") {
		t.Fatal("the new unit lost the runtime-dir ListenStream")
	}
	if !strings.Contains(surfaceLegacySocketUnitContent, "ListenStream=%t/toolsd.sock") {
		t.Fatal("the legacy constant must keep the percent-t runtime address it represents")
	}
}

// TestBuildSurfaceRemoveScript walks the generated remove script: stop,
// disable, remove BOTH files, reload — in that order — with absence
// verification at the end for BOTH the runtime-dir and the legacy socket
// paths (a pre-SURF-B2 socket must not survive a removal).
func TestBuildSurfaceRemoveScript(t *testing.T) {
	script := surfaceRemoveScript("abc123")
	for _, want := range []string{
		"systemctl --user stop toolsd.socket",
		"systemctl --user disable toolsd.socket",
		`rm -f "$s" "$v"`,
		"systemctl --user daemon-reload",
		`p='/run/bunker/abc123/toolsd.sock'`,
		`legacy='/run/user/$(id -u)/toolsd.sock'`,
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
	// Absence proof covers BOTH paths before the success verdict.
	current := strings.Index(script, `p='/run/bunker/abc123/toolsd.sock'`)
	legacy := strings.Index(script, `if [ -e "$legacy" ] || [ -S "$legacy" ]; then`)
	success := strings.LastIndex(script, "state=removed")
	if current == -1 || legacy == -1 || success == -1 || current >= legacy || legacy >= success {
		t.Errorf("remove script must prove both socket paths absent before success:\n%s", script)
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
	if !strings.Contains(buildSurfaceInstallScript("abc123"), quoteShell(renderSurfaceSocketUnit("abc123"))) {
		t.Error("socket unit content must be carried verbatim inside the install script")
	}
}

// TestSurfaceTCPProbeScript: the no-TCP proof prints TCP\tcount=<n|unknown>.
func TestSurfaceTCPProbeScript(t *testing.T) {
	if !strings.Contains(surfaceTCPProbeScript, `printf 'TCP\tcount=%s\n' "$n"`) {
		t.Errorf("tcp probe lost the count line:\n%s", surfaceTCPProbeScript)
	}
	if !strings.Contains(surfaceTCPProbeScript, "ss -H -tln") {
		t.Errorf("tcp probe must listen via ss -tln (no own listener):\n%s", surfaceTCPProbeScript)
	}
	if !strings.Contains(surfaceTCPProbeScript, "n=unknown") {
		t.Errorf("tcp probe must report unknown (unproven) when ss is absent:\n%s", surfaceTCPProbeScript)
	}
}

// TestSurfaceCheckScriptCarriesAgentPaths: the --check script addresses the
// per-agent runtime dir (not %t), reports the dir's owner+mode, and counts
// listening TCP sockets.
func TestSurfaceCheckScriptCarriesAgentPaths(t *testing.T) {
	script := surfaceCheckScript("abc123")
	for _, want := range []string{
		`p='/run/bunker/abc123/toolsd.sock'`,
		`d=/run/bunker/abc123`,
		"dir_meta",
		"tcp_listeners",
		"socket_active",
		"socket_meta",
		"toolsd_procs",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("check script lost %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "id -u") {
		t.Errorf("check script must derive paths from the agent id, not the uid:\n%s", script)
	}
}

// TestSurfaceVerifyScriptCarriesAgentPaths: the verify RESULT line carries
// the runtime-dir socket path and the dir mode.
func TestSurfaceVerifyScriptCarriesAgentPaths(t *testing.T) {
	script := surfaceVerifyScript("abc123")
	for _, want := range []string{
		`p='/run/bunker/abc123/toolsd.sock'`,
		"dirmode=",
		`stat -c '%a' /run/bunker/abc123`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("verify script lost %q:\n%s", want, script)
		}
	}
}

// TestSurfaceBuildersUTF8: every builder output is valid UTF-8 (the scripts
// travel through the exec RPC as text).
func TestSurfaceBuildersUTF8(t *testing.T) {
	for name, s := range map[string]string{
		"install":   buildSurfaceInstallScript("abc123"),
		"verify":    surfaceVerifyScript("abc123"),
		"remove":    surfaceRemoveScript("abc123"),
		"check":     surfaceCheckScript("abc123"),
		"tcp":       surfaceTCPProbeScript,
		"preflight": buildSurfacePreflightScript(),
	} {
		if !utf8.ValidString(s) {
			t.Errorf("%s builder produced invalid UTF-8", name)
		}
	}
}

// TestParseSurfaceResultLine: the RESULT line is the verify contract. Missing
// fields, malformed fields and non-RESULT lines must never parse as success.
func TestParseSurfaceResultLine(t *testing.T) {
	// (body unchanged from the SURF-006 pins — the RESULT-line wire shape
	// gained only an extra dirmode= field, which the parser tolerates)
	st, ok := parseSurfaceResultLine("RESULT\tstate=active\tsocket=/run/bunker/abc123/toolsd.sock\towner=bunker-abc123\tmode=600\tdirmode=700")
	if !ok {
		t.Fatal("a well-formed RESULT line with dirmode must parse")
	}
	if st.State != "active" || st.SocketPath != "/run/bunker/abc123/toolsd.sock" || st.Owner != "bunker-abc123" || st.Mode != "600" {
		t.Errorf("parsed state = %+v", st)
	}
	if _, ok := parseSurfaceResultLine("not-a-result"); ok {
		t.Error("a non-RESULT line must not parse")
	}
	if _, ok := parseSurfaceResultLine("RESULT\tstate="); ok {
		t.Error("an empty state must not parse as success")
	}
	if _, ok := parseSurfaceResultLine("RESULT\tnotkval\tstate=active"); ok {
		t.Error("a malformed field must refuse the whole line")
	}
}

// TestParseSurfaceVerifyOutput finds the RESULT line among prose.
func TestParseSurfaceVerifyOutput(t *testing.T) {
	out := "prose\nRESULT\tstate=active\tsocket=/run/bunker/abc123/toolsd.sock\towner=bunker-abc123\tmode=600\tdirmode=700\nmore\n"
	st, ok := parseSurfaceVerifyOutput(out)
	if !ok || st.SocketPath != "/run/bunker/abc123/toolsd.sock" {
		t.Errorf("verify output not parsed: %+v ok=%v", st, ok)
	}
}

// TestSurfaceInstallWrote: written vs unchanged verdicts.
func TestSurfaceInstallWrote(t *testing.T) {
	if !surfaceInstallWrote("UNIT\tx\twritten\n") {
		t.Error("written verdict missed")
	}
	if surfaceInstallWrote("UNIT\tx\tunchanged\n") {
		t.Error("unchanged verdict read as written")
	}
}

// TestParseSurfaceRemoveOutput: removal result extraction.
func TestParseSurfaceRemoveOutput(t *testing.T) {
	path, state, ok := parseSurfaceRemoveOutput("RESULT\tsocket=/run/bunker/abc123/toolsd.sock\tstate=removed\tunits=deleted\n")
	if !ok || path != "/run/bunker/abc123/toolsd.sock" || state != "removed" {
		t.Errorf("remove output parsed path=%q state=%q ok=%v", path, state, ok)
	}
}

// TestParseSurfaceIsRunning and the manager classification pins.
func TestParseSurfaceIsRunning(t *testing.T) {
	if got := parseSurfaceIsRunning("running\n"); got != "running" {
		t.Errorf("parseSurfaceIsRunning = %q", got)
	}
	if got := parseSurfaceIsRunning("\n\n"); got != "" {
		t.Errorf("blank output must parse as empty, got %q", got)
	}
}

func TestIsUserManagerUnavailable(t *testing.T) {
	for _, live := range []string{"running", "degraded", ""} {
		if isUserManagerUnavailable(live) {
			t.Errorf("%q must read as a live (or askable) manager", live)
		}
	}
	for _, dead := range []string{"offline", "Failed to connect to bus", "not been booted"} {
		if !isUserManagerUnavailable(dead) {
			t.Errorf("%q must read as unavailable", dead)
		}
	}
}

func TestClassifySurfaceManagerFailure(t *testing.T) {
	err := classifySurfaceManagerFailure("agent7", "")
	if err == nil || !strings.Contains(err.Error(), "user manager") {
		t.Errorf("empty preflight must name the manager failure, got: %v", err)
	}
	err = classifySurfaceManagerFailure("agent7", "offline\n")
	if err == nil || !strings.Contains(err.Error(), "Linger") {
		t.Errorf("unavailable preflight must carry the Linger hint, got: %v", err)
	}
}

// TestSurfaceToolsdProbeParsing: present/absent/unknown.
func TestSurfaceToolsdProbeParsing(t *testing.T) {
	if got := parseSurfaceToolsdProbe("present\n"); got != "present" {
		t.Errorf("probe parse = %q", got)
	}
	if got := parseSurfaceToolsdProbe("absent\n"); got != "absent" {
		t.Errorf("probe parse = %q", got)
	}
	if got := parseSurfaceToolsdProbe("garbage"); got != "unknown" {
		t.Errorf("probe parse = %q, want unknown", got)
	}
}

func TestSurfaceToolsdProbeScript(t *testing.T) {
	if !strings.Contains(surfaceToolsdProbeScript, `"$HOME/bin/toolsd"`) {
		t.Errorf("probe must check $HOME/bin/toolsd:\n%s", surfaceToolsdProbeScript)
	}
}

// TestTailSurfaceOutput: the error-message tail helper.
func TestTailSurfaceOutput(t *testing.T) {
	if got := tailSurfaceOutput("abcdef", 3); got != "…def" {
		t.Errorf("tailSurfaceOutput = %q", got)
	}
	if got := tailSurfaceOutput("ab", 10); got != "ab" {
		t.Errorf("tailSurfaceOutput = %q", got)
	}
}
