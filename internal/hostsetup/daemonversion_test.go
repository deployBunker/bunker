package hostsetup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// daemonVersionFixture renders `bunkerd --version` output the way
// cmd/bunkerd/main.go prints it for a build that carries the spawn-side grant:
// the "bunkerd <version>" line, commit/built, then the caps line.
func daemonVersionFixture(version, commit, built string) []byte {
	return daemonVersionFixtureCaps(version, commit, built, []string{GrantCapability})
}

// legacyDaemonVersionFixture renders the PRE-GAP-082 block — the five lines a
// v0.1.4 (or older) build prints, with NO caps line at all. That is the exact
// shape that used to be accepted at version 0.1.4 while carrying no grant.
func legacyDaemonVersionFixture(version, commit, built string) []byte {
	return daemonVersionFixtureCaps(version, commit, built, nil)
}

// daemonVersionFixtureCaps renders the block with the given caps values: nil
// renders NO caps line, while a non-nil empty slice renders an empty `caps:`
// line ("no capabilities reported").
func daemonVersionFixtureCaps(version, commit, built string, caps []string) []byte {
	block := fmt.Sprintf("bunkerd %s\n  commit:     %s\n  built:      %s\n", version, commit, built)
	if caps != nil {
		block += "  caps:       " + strings.Join(caps, ",") + "\n"
	}
	return []byte(block + "  go version: go1.11.15\n  platform:   linux/amd64\n")
}

// fakeDaemonVersionOptions returns options whose daemon probe is answered by
// answer (or by err); the options' host Runner is a stub that approves every
// probe, so NO real binary and NO real host state is ever touched.
func fakeDaemonVersionOptions(t *testing.T, answer []byte, probeErr error) Options {
	t.Helper()
	dv := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return answer, probeErr
	}
	host := func(ctx context.Context, name string, args ...string) ([]byte, error) { return nil, nil }
	o := Options{Runner: host, DaemonVersionRunner: dv, ScratchEnabled: true}
	return o.WithDefaults()
}

// sandboxOptions builds a full sandbox host (fake PAM modules + sshd file)
// the way sshSandbox does, plus a daemon probe answered by answer/err, so the
// real installer path can run without touching the host. Host commands go to
// a recorder whose calls the test can read back via rec.calls.
func sandboxOptions(t *testing.T, answer []byte, probeErr error) (Options, *recorder) {
	t.Helper()
	rec := newRecorder(t)
	o := rec.options(nil)
	o.DaemonVersionRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return answer, probeErr
	}
	o = o.WithDefaults()
	writeSandboxPAMFixtures(t, o, rec.root)
	return o, rec
}

// writeSandboxPAMFixtures drops the fake PAM modules and the sshd file into
// the sandbox root so the installer's module probe (a REAL os.Stat walk, not
// a runner command) finds them, as in sshSandbox.
func writeSandboxPAMFixtures(t *testing.T, o Options, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(o.SSHDConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.SSHDConfigPath, []byte("#%PAM-1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{"pam_namespace.so", "pam_succeed_if.so", "pam_exec.so"} {
		module := filepath.Join(root, "lib/x86_64-linux-gnu/security", mod)
		if err := os.MkdirAll(filepath.Dir(module), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(module, []byte(classifierModuleStub), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// daemonSkewCases is the decision-logic table (INT-DEMO-001): every row names
// the installed daemon, the expected state and whether CheckDaemonSkew (and
// therefore Apply) must refuse.
//
// The capability rows are the GAP-082 fix: a VERSION can never be sufficient,
// because a bare `go build` of any revision reports the package default version
// (internal/version.Version = "0.1.4"), which is exactly how a v0.1.4 tag build
// claims 0.1.4 while carrying no grant. Absent capability = SKEWED at ANY
// version; the version floor is only the secondary check.
var daemonSkewCases = []struct {
	name    string
	answer  []byte   // raw --version output; nil = build a fixture from the fields below
	version string   //
	commit  string   //
	built   string   //
	noCaps  bool     // render the legacy block: NO caps line (pre-GAP-082 / v0.1.4 shape)
	caps    []string // explicit caps values; nil with noCaps=false = this build's grant token
	probeEr error    //
	allow   bool     // --allow-daemon-skew
	state   DaemonProbeState
	refuse  bool     // CheckDaemonSkew must return the refusal
	wantMsg []string // extra substrings the refusal must carry for this row
}{
	// ── (c) the capability gate: no version may override a missing token ──
	{
		name:    "legacy 0.1.4 build with no caps line is refused (the live v0.1.4 shape)",
		version: "0.1.4", commit: "b2c3d4e", built: "2026-09-12T00:00:00Z", noCaps: true,
		state: DaemonSkewSkewed, refuse: true,
		wantMsg: []string{"reported capabilities: none reported", "the version comparison alone would have accepted it"},
	},
	{
		name:    "caps-less 0.2.0 build is refused even though the version is newer",
		version: "0.2.0", commit: "c3d4e5f", built: "2026-10-01T00:00:00Z", noCaps: true,
		state: DaemonSkewSkewed, refuse: true,
		wantMsg: []string{"reported capabilities: none reported", "the version comparison alone would have accepted it"},
	},
	{
		name:    "empty caps line is refused",
		version: "0.1.4", commit: "b2c3d4e", built: "2026-09-12T00:00:00Z", caps: []string{},
		state: DaemonSkewSkewed, refuse: true,
		wantMsg: []string{"reported capabilities: none reported"},
	},
	{
		name:    "an unrelated capability is refused",
		version: "0.1.4", commit: "b2c3d4e", built: "2026-09-12T00:00:00Z", caps: []string{"other-cap"},
		state: DaemonSkewSkewed, refuse: true,
		wantMsg: []string{"reported capabilities: other-cap"},
	},
	// ── the grant present: the version floor becomes the only remaining check ──
	{name: "grant capability at the floor proceeds", version: "0.1.4", commit: "b2c3d4e", built: "2026-09-12T00:00:00Z", state: DaemonSkewOK},
	{name: "grant capability on a newer patch proceeds", version: "0.1.5", commit: "d4e5f6a", built: "2026-09-20T00:00:00Z", state: DaemonSkewOK},
	{name: "grant capability on a newer minor proceeds", version: "0.2.0", commit: "c3d4e5f", built: "2026-10-01T00:00:00Z", state: DaemonSkewOK},
	{name: "v-prefixed newer daemon proceeds", version: "v0.2.1", commit: "e5f6a7b", built: "2026-10-02T00:00:00Z", state: DaemonSkewOK},
	{
		name:    "grant capability below the floor is refused (the floor still applies)",
		version: "0.1.3", commit: "a1b2c3d", built: "2026-08-30T00:00:00Z",
		state: DaemonSkewSkewed, refuse: true,
		wantMsg: []string{"Its version is also below the " + MinDaemonVersion + " floor"},
	},
	{name: "operator override proceeds past a missing capability", version: "0.1.4", commit: "b2c3d4e", built: "2026-09-12T00:00:00Z", noCaps: true, allow: true, state: DaemonSkewSkewed},
	{name: "operator override proceeds past a version-floor skew", version: "0.1.3", commit: "a1b2c3d", built: "2026-08-30T00:00:00Z", allow: true, state: DaemonSkewSkewed},
	// ── the probe-failure rows: UNKNOWN, warn, never refuse ──
	{name: "binary absent is a warning, not a refusal", probeEr: exec.ErrNotFound, state: DaemonSkewUnknown},
	{name: "probe timeout is a warning, not a refusal", probeEr: context.DeadlineExceeded, state: DaemonSkewUnknown},
	{name: "unparseable output is a warning, not a refusal", answer: []byte("go version: go1.11.15\n"), state: DaemonSkewUnknown},
	{name: "unknown version is a warning, not a refusal", version: "unknown", commit: "unknown", built: "unknown", state: DaemonSkewUnknown},
	{name: "caps line without a version is a warning, not a refusal", answer: []byte("  caps:       " + GrantCapability + "\n"), state: DaemonSkewUnknown},
}

func TestCheckDaemonSkew_DecisionTable(t *testing.T) {
	for _, tc := range daemonSkewCases {
		t.Run(tc.name, func(t *testing.T) {
			answer := tc.answer
			if answer == nil {
				var caps []string
				switch {
				case tc.noCaps:
					caps = nil
				case tc.caps != nil:
					caps = tc.caps
				default:
					caps = []string{GrantCapability}
				}
				answer = daemonVersionFixtureCaps(tc.version, tc.commit, tc.built, caps)
			}
			o := fakeDaemonVersionOptions(t, answer, tc.probeEr)

			build, state, perr := o.ProbeDaemonVersion(context.Background())
			if state != tc.state {
				t.Fatalf("probe state = %q, want %q (perr=%v, build=%+v)", state, tc.state, perr, build)
			}
			if !tc.refuse && tc.state == DaemonSkewSkewed && perr != nil {
				t.Fatalf("skewed probe unexpectedly errored: %v", perr)
			}
			if tc.state == DaemonSkewOK {
				if build.Version != strings.TrimPrefix(tc.version, "v") {
					t.Errorf("parsed version = %q, want %q", build.Version, strings.TrimPrefix(tc.version, "v"))
				}
				if !build.HasCapability(GrantCapability) {
					t.Errorf("OK build does not report %s: %+v", GrantCapability, build)
				}
			}
			// UNKNOWN (binary absent, timed out, unparseable, version
			// "unknown") must WARN and proceed — warn-never-refuse is the
			// documented policy, because a host may legitimately be
			// provisioned before the daemon is installed. (DaemonSkew itself
			// stays a pure predicate over a PARSED build and is only reached
			// from the SKEWED branch above; an unparseable build never gets
			// past ProbeDaemonVersion.)
			if tc.state == DaemonSkewUnknown {
				var unknownWarn bytes.Buffer
				if err := o.CheckDaemonSkew(context.Background(), false, &unknownWarn); err != nil {
					t.Fatalf("UNKNOWN must never refuse: %v", err)
				}
				if !strings.Contains(unknownWarn.String(), "WARNING") {
					t.Errorf("UNKNOWN must warn, got:\n%s", unknownWarn.String())
				}
				return
			}

			var warn bytes.Buffer
			err := o.CheckDaemonSkew(context.Background(), tc.allow, &warn)
			if tc.refuse {
				if err == nil {
					t.Fatalf("CheckDaemonSkew = nil, want refusal for version %q caps=%v", tc.version, build.Capabilities)
				}
				msg := err.Error()
				want := []string{
					GrantCapability,          // THE requirement, named in the message
					"reported capabilities:", // what the daemon actually advertised
					MinDaemonVersion,         // the secondary floor, still named
					"exit status 254",        // the operator-visible failure mode
					"bunker exec, mount and cp",
					"upgrade the daemon",
					"--uninstall --apply", // remediation 2
					"Never hand-delete only the PAM drop-in",
				}
				if tc.version != "" {
					want = append(want, strings.TrimPrefix(tc.version, "v"))
				}
				if tc.commit != "" && tc.commit != "unknown" {
					want = append(want, "commit "+tc.commit)
				}
				if len(tc.built) >= 10 && tc.built != "unknown" {
					want = append(want, "built "+tc.built[:10])
				}
				want = append(want, tc.wantMsg...)
				for _, w := range want {
					if !strings.Contains(msg, w) {
						t.Errorf("refusal message missing %q:\n%s", w, msg)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckDaemonSkew = %v, want nil", err)
			}
			if tc.allow && tc.state == DaemonSkewSkewed {
				w := warn.String()
				for _, wantSub := range []string{"WARNING", "--allow-daemon-skew", strings.TrimPrefix(tc.version, "v")} {
					if !strings.Contains(w, wantSub) {
						t.Errorf("override warning missing %q:\n%s", wantSub, w)
					}
				}
			}
			if tc.state == DaemonSkewOK && warn.Len() != 0 {
				t.Errorf("OK daemon must not warn, got:\n%s", warn.String())
			}
		})
	}
}

// TestCapabilityIsAuthoritativeOverVersion pins the decision rule itself, so a
// future refactor cannot quietly restore the version-only floor: the SAME
// version yields OK with the token and SKEWED without it.
func TestCapabilityIsAuthoritativeOverVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build DaemonBuild
		want  DaemonProbeState
	}{
		{"token, version at the floor", DaemonBuild{Version: "0.1.4", Capabilities: []string{GrantCapability}}, DaemonSkewOK},
		{"no token, version at the floor", DaemonBuild{Version: "0.1.4"}, DaemonSkewSkewed},
		{"no token, version far above the floor", DaemonBuild{Version: "9.9.9"}, DaemonSkewSkewed},
		{"token, version below the floor", DaemonBuild{Version: "0.1.3", Capabilities: []string{GrantCapability}}, DaemonSkewSkewed},
		{"token among others, version at the floor", DaemonBuild{Version: "0.1.4", Capabilities: []string{"a", GrantCapability, "b"}}, DaemonSkewOK},
		{"token with different case and padding", DaemonBuild{Version: "0.1.4", Capabilities: []string{" " + strings.ToUpper(GrantCapability) + " "}}, DaemonSkewOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonSkewState(tc.build); got != tc.want {
				t.Errorf("daemonSkewState(%+v) = %q, want %q", tc.build, got, tc.want)
			}
			err := DaemonSkew(tc.build)
			if tc.want == DaemonSkewOK && err != nil {
				t.Errorf("DaemonSkew(%+v) = %v, want nil", tc.build, err)
			}
			if tc.want == DaemonSkewSkewed {
				if err == nil {
					t.Fatalf("DaemonSkew(%+v) = nil, want refusal", tc.build)
				}
				if !strings.Contains(err.Error(), GrantCapability) {
					t.Errorf("refusal does not name %s:\n%v", GrantCapability, err)
				}
			}
		})
	}
}

// TestGrantProbeAgainstRealBinary drives the FULL probe — real exec of a
// compiled binary, real parse, real decision — against the bunkerd named by
// BUNKER_TEST_DAEMON_BINARY. It is skipped unless that env var is set, so the
// suite stays hermetic (no host state, no ssh, no daemon required), and it is
// what proves criterion (c) on a real binary instead of on a fixture:
//
//	BUNKER_TEST_DAEMON_BINARY=/tmp/bunkerd-prefix go test ./internal/hostsetup -run RealBinary -v
func TestGrantProbeAgainstRealBinary(t *testing.T) {
	bin := os.Getenv("BUNKER_TEST_DAEMON_BINARY")
	if bin == "" {
		t.Skip("BUNKER_TEST_DAEMON_BINARY is unset: set it to a built bunkerd binary to probe a real build")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("BUNKER_TEST_DAEMON_BINARY=%s: %v", bin, err)
	}

	o := Options{DaemonBinary: bin, ScratchEnabled: true}.WithDefaults()
	build, state, perr := o.ProbeDaemonVersion(context.Background())
	t.Logf("probed %s: version=%q commit=%q built=%q caps=%v -> state=%s probeErr=%v",
		bin, build.Version, build.Commit, build.Built, build.Capabilities, state, perr)

	refusal := DaemonSkew(build)
	if build.HasCapability(GrantCapability) {
		if state != DaemonSkewOK {
			t.Errorf("real binary reports %s but probe state = %s, want OK", GrantCapability, state)
		}
		if refusal != nil {
			t.Errorf("real binary reports %s but DaemonSkew refused: %v", GrantCapability, refusal)
		}
		t.Logf("RESULT: OK — the real build advertises %s", GrantCapability)
		return
	}

	if state != DaemonSkewSkewed {
		t.Errorf("real binary does NOT report %s but probe state = %s, want SKEWED (the version must not decide)", GrantCapability, state)
	}
	if refusal == nil {
		t.Fatalf("real binary does not report %s and DaemonSkew returned nil — a version-only floor accepts the very build that lacks the grant: %+v", GrantCapability, build)
	}
	if !strings.Contains(refusal.Error(), GrantCapability) {
		t.Errorf("refusal does not name the missing capability %s:\n%v", GrantCapability, refusal)
	}
	t.Logf("RESULT: SKEWED/refusal naming %s:\n%v", GrantCapability, refusal)
}

// TestGrantFloorMatchesReality is criterion (b): the constant that names the
// first tag carrying the grant cannot drift from the repository's tag list.
// With GrantMinTag empty it asserts NO visible tag's tree carries the token (the
// empty constant is honest today); with a name set it asserts that tag exists and
// DOES carry it. A skip — git missing, not a work tree, or no tags visible
// (CI checks out with fetch-depth 1) — is correct there and never a false red.
func TestGrantFloorMatchesReality(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed: cannot verify GrantMinTag against the release tags")
	}
	root, err := gitOutput(gitPath, "", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Skipf("not inside a git work tree: %v", err)
	}
	root = strings.TrimSpace(root)
	tagsOut, err := gitOutput(gitPath, root, "tag", "--list")
	if err != nil {
		t.Skipf("git tag failed: %v", err)
	}
	tags := strings.Fields(tagsOut)
	if len(tags) == 0 {
		t.Skip("no visible git tags (shallow clone?): cannot verify GrantMinTag")
	}

	if GrantMinTag == "" {
		for _, tag := range tags {
			if tagTreeCarriesGrant(t, gitPath, root, tag) {
				t.Errorf("GrantMinTag is empty but tag %s already carries the %s token under internal/agent: set GrantMinTag = %q",
					tag, GrantCapability, tag)
			}
		}
		t.Logf("GrantMinTag is empty and none of the %d visible tag(s) [%s] carries the %s token — the empty constant is honest",
			len(tags), strings.Join(tags, ", "), GrantCapability)
		return
	}

	if _, err := gitOutput(gitPath, root, "rev-parse", "-q", "--verify", "refs/tags/"+GrantMinTag); err != nil {
		t.Fatalf("GrantMinTag = %q but no such tag exists (visible tags: %s)", GrantMinTag, strings.Join(tags, ", "))
	}
	if !tagTreeCarriesGrant(t, gitPath, root, GrantMinTag) {
		t.Errorf("GrantMinTag = %q but that tag's tree does not contain the %s token under internal/agent: the constant names a release that does not carry the grant",
			GrantMinTag, GrantCapability)
	}
}

// tagTreeCarriesGrant reports whether tag's tree contains the capability token
// anywhere under internal/agent (the package that owns the spawn-side grant).
// `git grep` exits 1 both for "no match" and for a path that does not exist in
// that tree (older tags predate internal/agent), so only exit 0 means FOUND and
// any other code is an error rather than a silent "absent". One invocation per
// tag: the work is bounded by the tag count, never by commit count.
func tagTreeCarriesGrant(t *testing.T, gitPath, repoRoot, tag string) bool {
	t.Helper()
	cmd := exec.Command(gitPath, "grep", "-q", GrantCapability, tag, "--", "internal/agent")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git grep %s %s failed: %v (output: %s)", GrantCapability, tag, err, strings.TrimSpace(string(out)))
	return false
}

// gitOutput runs git with dir as the working directory and returns combined
// output. dir == "" inherits the test's directory.
func gitOutput(gitPath, dir string, args ...string) (string, error) {
	cmd := exec.Command(gitPath, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCheckDaemonSkew_UnknownWarnsAndNamesTheProbe(t *testing.T) {
	o, rec := sandboxOptions(t, nil, exec.ErrNotFound)
	var warn bytes.Buffer
	if err := o.CheckDaemonSkew(context.Background(), false, &warn); err != nil {
		t.Fatalf("probe failure must not refuse: %v", err)
	}
	_ = rec
	w := warn.String()
	for _, want := range []string{"WARNING", "could NOT be ruled out", DefaultDaemonBinary, GrantCapability, MinDaemonVersion, "exit 254"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q:\n%s", want, w)
		}
	}
}

func TestApply_RefusesOlderDaemonBeforeAnyMutation(t *testing.T) {
	// A full sandbox host whose daemon probe answers 0.1.3 WITH the grant: the
	// secondary version floor must still refuse, and Apply must return the
	// refusal verbatim and issue NO host command at all — not even the group
	// lookup a plan run would do.
	o, rec := sandboxOptions(t, daemonVersionFixture("0.1.3", "a1b2c3d", "2026-08-30T00:00:00Z"), nil)

	for _, apply := range []bool{false, true} {
		rep, err := o.Apply(context.Background(), apply)
		if err == nil {
			t.Fatalf("Apply(apply=%v) = nil error, want the daemon-skew refusal", apply)
		}
		if !strings.Contains(err.Error(), MinDaemonVersion) {
			t.Errorf("Apply(apply=%v) error missing the minimum:\n%v", apply, err)
		}
		if len(rep.Mutations()) != 0 {
			t.Errorf("Apply(apply=%v) mutated the host: %v", apply, rep.Mutations())
		}
		if n := len(rec.calls); n != 0 {
			t.Errorf("Apply(apply=%v) issued %d host commands before refusing: %v", apply, n, rec.calls)
		}
	}
}

func TestApply_RefusesLegacyDaemonWithoutTheCapability(t *testing.T) {
	// GAP-082: a REAL v0.1.4 daemon reports version 0.1.4 and carries no grant.
	// Before the fix this exact build was accepted (the version floor passed);
	// now Apply must refuse it before touching the host.
	o, rec := sandboxOptions(t, legacyDaemonVersionFixture("0.1.4", "b2c3d4e", "2026-09-12T00:00:00Z"), nil)

	for _, apply := range []bool{false, true} {
		rep, err := o.Apply(context.Background(), apply)
		if err == nil {
			t.Fatalf("Apply(apply=%v) = nil error, want the missing-capability refusal", apply)
		}
		if !strings.Contains(err.Error(), GrantCapability) {
			t.Errorf("Apply(apply=%v) error does not name the required capability %s:\n%v", apply, GrantCapability, err)
		}
		if len(rep.Mutations()) != 0 {
			t.Errorf("Apply(apply=%v) mutated the host: %v", apply, rep.Mutations())
		}
		if n := len(rec.calls); n != 0 {
			t.Errorf("Apply(apply=%v) issued %d host commands before refusing: %v", apply, n, rec.calls)
		}
	}
}

func TestApply_ProbeFailureWarnsAndProceeds(t *testing.T) {
	// Daemon binary absent (daemon not installed yet): no refusal, and the
	// dry-run plan still renders.
	o, rec := sandboxOptions(t, nil, exec.ErrNotFound)

	var warn bytes.Buffer
	if err := o.CheckDaemonSkew(context.Background(), false, &warn); err != nil {
		t.Fatalf("probe failure must not refuse: %v", err)
	}
	if !strings.Contains(warn.String(), "WARNING") {
		t.Errorf("probe failure must warn, got:\n%s", warn.String())
	}
	rep, err := o.Apply(context.Background(), false)
	if err != nil {
		t.Fatalf("Apply with an unprobeable daemon must proceed: %v", err)
	}
	if len(rep.Mutations()) != 0 {
		t.Errorf("dry run mutated the host: %v", rep.Mutations())
	}
	if !rec.ranPrefix("getent group ") {
		t.Errorf("the plan never rendered (no group probe issued): %v", rec.calls)
	}
}

func TestUninstall_NeverGatedByDaemonSkew(t *testing.T) {
	// Removing the hardening must stay possible on a skewed daemon: the
	// skew check is not on the uninstall path at all. Wire a probe that
	// FAILS THE TEST if it is ever issued, then run the uninstall core.
	dv := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Error("uninstall must not probe the daemon version")
		return nil, nil
	}
	rec := newRecorder(t)
	o := rec.options(nil)
	o.DaemonVersionRunner = dv
	o = o.WithDefaults()

	// RemoveTmpNamespace (the uninstall core) on a sandbox sshd file: the
	// ASSERTION is the probe never running (t.Error above), not the removal
	// result.
	writeSandboxPAMFixtures(t, o, rec.root)
	if _, err := o.RemoveTmpNamespace(context.Background()); err != nil {
		t.Fatalf("uninstall core failed: %v", err)
	}
}

func TestParseDaemonVersionOutput(t *testing.T) {
	t.Run("parses the bunkerd block", func(t *testing.T) {
		b, err := ParseDaemonVersionOutput(daemonVersionFixture("0.1.4", "b2c3d4e", "2026-09-12T22:00:00Z"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if b.Version != "0.1.4" || b.Commit != "b2c3d4e" || b.Built != "2026-09-12T22:00:00Z" {
			t.Errorf("parsed %+v", b)
		}
		if !b.HasCapability(GrantCapability) {
			t.Errorf("parsed build lost the caps line: %+v", b)
		}
	})
	t.Run("rejects prose", func(t *testing.T) {
		if _, err := ParseDaemonVersionOutput([]byte("The daemon is at version something")); err == nil {
			t.Error("prose must not parse")
		}
	})
	t.Run("rejects empty output", func(t *testing.T) {
		if _, err := ParseDaemonVersionOutput(nil); err == nil {
			t.Error("empty output must not parse")
		}
	})
	t.Run("still ignores unknown lines", func(t *testing.T) {
		out := "bunkerd 0.1.4\n  commit:     b2c3d4e\n  something:  new\n  caps:       " + GrantCapability + "\n"
		if _, err := ParseDaemonVersionOutput([]byte(out)); err != nil {
			t.Errorf("lenient parse broke on an unknown line: %v", err)
		}
	})
}

// TestParseDaemonVersionOutput_CapsLine pins the caps grammar: present, absent,
// comma and/or whitespace separated, extra indentation, case preserved and an
// empty value meaning "no capabilities" (not an error).
func TestParseDaemonVersionOutput_CapsLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want []string
	}{
		{"comma separated", "  caps:       isolation-grant,other\n", []string{"isolation-grant", "other"}},
		{"space separated", "  caps:       isolation-grant other\n", []string{"isolation-grant", "other"}},
		{"comma and space", "  caps:       isolation-grant, other\n", []string{"isolation-grant", "other"}},
		{"no space after the colon", "caps:isolation-grant\n", []string{"isolation-grant"}},
		{"extra indentation", "      caps:        isolation-grant\n", []string{"isolation-grant"}},
		{"case preserved", "  caps:       Isolation-Grant\n", []string{"Isolation-Grant"}},
		{"single token", "  caps:       isolation-grant\n", []string{"isolation-grant"}},
		{"empty value means none", "  caps:       \n", nil},
		{"whitespace-only value means none", "  caps:     \t \n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := "bunkerd 0.1.4\n  commit:     b2c3d4e\n  built:      2026-09-12T00:00:00Z\n" + tc.out + "  go version: go1.11.15\n"
			b, err := ParseDaemonVersionOutput([]byte(out))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := strings.Join(b.Capabilities, "|"); got != strings.Join(tc.want, "|") {
				t.Errorf("capabilities = %q, want %q", got, strings.Join(tc.want, "|"))
			}
		})
	}

	t.Run("absent caps line yields no capabilities", func(t *testing.T) {
		b, err := ParseDaemonVersionOutput(legacyDaemonVersionFixture("0.1.4", "b2c3d4e", "2026-09-12T00:00:00Z"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(b.Capabilities) != 0 {
			t.Errorf("legacy block produced capabilities %v, want none", b.Capabilities)
		}
		if b.HasCapability(GrantCapability) {
			t.Error("legacy block must not report the grant capability")
		}
	})
}

func TestDaemonBuildHasCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
		ask  string
		want bool
	}{
		{"exact match", []string{"isolation-grant"}, "isolation-grant", true},
		{"case-insensitive", []string{"isolation-grant"}, "Isolation-Grant", true},
		{"trims the stored value", []string{"  isolation-grant  "}, "isolation-grant", true},
		{"trims the queried name", []string{"isolation-grant"}, " isolation-grant ", true},
		{"found among others", []string{"a", "isolation-grant", "b"}, "isolation-grant", true},
		{"absent", []string{"other"}, "isolation-grant", false},
		{"no capabilities", nil, "isolation-grant", false},
		{"empty name never matches", []string{"isolation-grant"}, "", false},
		{"whitespace name never matches", []string{"isolation-grant"}, "   ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := DaemonBuild{Capabilities: tc.caps}
			if got := b.HasCapability(tc.ask); got != tc.want {
				t.Errorf("HasCapability(%q) with %v = %v, want %v", tc.ask, tc.caps, got, tc.want)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, min string
		want      bool
	}{
		{"0.1.3", "0.1.4", false},
		{"0.1.4", "0.1.4", true},
		{"0.1.5", "0.1.4", true},
		{"0.2.0", "0.1.4", true},
		{"1.0.0", "0.1.4", true},
		{"v0.1.4", "0.1.4", true},
		{"0.1.4", "v0.1.4", true},
		{"0.1", "0.1.4", false},
		{"0.1.4.1", "0.1.4", true},
		{"unknown", "0.1.4", false}, // bare go build: fail-safe
		{"", "0.1.4", false},
		{"abc", "0.1.4", false},
	}
	for _, tc := range cases {
		if got := versionAtLeast(tc.have, tc.min); got != tc.want {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tc.have, tc.min, got, tc.want)
		}
	}
}

func TestDaemonSkewHint(t *testing.T) {
	if s := DaemonSkewHint(DaemonSkewOK, DaemonBuild{}, nil); s != "" {
		t.Errorf("OK state must not warn, got %q", s)
	}
	build := DaemonBuild{Binary: DefaultDaemonBinary, Version: "0.1.3", Commit: "a1b2c3d"}
	w := DaemonSkewHint(DaemonSkewSkewed, build, nil)
	for _, want := range []string{"--allow-daemon-skew", "0.1.3", GrantCapability, "exit 254"} {
		if !strings.Contains(w, want) {
			t.Errorf("skewed hint missing %q: %s", want, w)
		}
	}
	// A caps-less build at a current version must be diagnosed as missing the
	// CAPABILITY, not as an old version.
	legacy := DaemonBuild{Binary: DefaultDaemonBinary, Version: "0.1.4", Commit: "b2c3d4e"}
	lw := DaemonSkewHint(DaemonSkewSkewed, legacy, nil)
	for _, want := range []string{GrantCapability, "none reported", "exit 254"} {
		if !strings.Contains(lw, want) {
			t.Errorf("legacy-build hint missing %q: %s", want, lw)
		}
	}
	// The legacy build already satisfies the version floor, so the hint must
	// not claim it "predates" the floor.
	if strings.Contains(lw, "predates") {
		t.Errorf("hint claims the legacy build predates a floor, but 0.1.4 == the floor: %s", lw)
	}
	uw := DaemonSkewHint(DaemonSkewUnknown, DaemonBuild{Binary: DefaultDaemonBinary}, errors.New("binary not found"))
	for _, want := range []string{"WARNING", "not found", GrantCapability, MinDaemonVersion} {
		if !strings.Contains(uw, want) {
			t.Errorf("unknown hint missing %q: %s", want, uw)
		}
	}
}

// TestDaemonSkewStringNamesTheCapability pins the status line operator-facing
// text: the requirement is the capability, the version floor is secondary, and
// the installed capabilities are shown so a SKEWED reading is actionable.
func TestDaemonSkewStringNamesTheCapability(t *testing.T) {
	legacy := Status{DaemonSkew: DaemonSkewSkewed, DaemonSkewBuild: DaemonBuild{Binary: "/usr/local/bin/bunkerd", Version: "0.1.4", Commit: "b2c3d4e", Built: "2026-09-12T00:00:00Z"}}
	got := legacy.DaemonSkewString()
	for _, want := range []string{"SKEWED", GrantCapability, "version floor " + MinDaemonVersion, "secondary", "caps none reported", "0.1.4"} {
		if !strings.Contains(got, want) {
			t.Errorf("DaemonSkewString missing %q: %s", want, got)
		}
	}
	reporting := Status{DaemonSkew: DaemonSkewOK, DaemonSkewBuild: DaemonBuild{Binary: "/usr/local/bin/bunkerd", Version: "0.1.4", Capabilities: []string{GrantCapability}}}
	if got := reporting.DaemonSkewString(); !strings.Contains(got, "caps "+GrantCapability) {
		t.Errorf("DaemonSkewString must show the reported capabilities: %s", got)
	}
}
