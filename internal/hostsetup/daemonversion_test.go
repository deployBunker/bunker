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
// cmd/bunkerd/main.go prints it.
func daemonVersionFixture(version, commit, built string) []byte {
	return []byte(fmt.Sprintf("bunkerd %s\n  commit:     %s\n  built:      %s\n  go version: go1.11.15\n", version, commit, built))
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
var daemonSkewCases = []struct {
	name    string
	answer  []byte // raw --version output; nil = build a fixture from version/commit/built
	version string
	commit  string
	built   string
	probeEr error
	allow   bool
	state   DaemonProbeState
	refuse  bool
}{
	{name: "older daemon is refused", version: "0.1.3", commit: "a1b2c3d", built: "2026-08-30T00:00:00Z", state: DaemonSkewSkewed, refuse: true},
	{name: "equal daemon proceeds", version: "0.1.4", commit: "b2c3d4e", built: "2026-09-12T00:00:00Z", state: DaemonSkewOK},
	{name: "newer daemon proceeds", version: "0.2.0", commit: "c3d4e5f", built: "2026-10-01T00:00:00Z", state: DaemonSkewOK},
	{name: "newer patch proceeds", version: "0.1.5", commit: "d4e5f6a", built: "2026-09-20T00:00:00Z", state: DaemonSkewOK},
	{name: "v-prefixed newer daemon proceeds", version: "v0.2.1", commit: "e5f6a7b", built: "2026-10-02T00:00:00Z", state: DaemonSkewOK},
	{name: "binary absent is a warning, not a refusal", probeEr: exec.ErrNotFound, state: DaemonSkewUnknown},
	{name: "probe timeout is a warning, not a refusal", probeEr: context.DeadlineExceeded, state: DaemonSkewUnknown},
	{name: "unparseable output is a warning, not a refusal", answer: []byte("go version: go1.11.15\n"), state: DaemonSkewUnknown},
	{name: "unknown version is a warning, not a refusal", version: "unknown", commit: "unknown", built: "unknown", state: DaemonSkewUnknown},
	{name: "operator override proceeds past a skew", version: "0.1.3", commit: "a1b2c3d", built: "2026-08-30T00:00:00Z", allow: true, state: DaemonSkewSkewed},
}

func TestCheckDaemonSkew_DecisionTable(t *testing.T) {
	for _, tc := range daemonSkewCases {
		t.Run(tc.name, func(t *testing.T) {
			answer := tc.answer
			if answer == nil {
				answer = daemonVersionFixture(tc.version, tc.commit, tc.built)
			}
			o := fakeDaemonVersionOptions(t, answer, tc.probeEr)

			build, state, perr := o.ProbeDaemonVersion(context.Background())
			if state != tc.state {
				t.Fatalf("probe state = %q, want %q (perr=%v, build=%+v)", state, tc.state, perr, build)
			}
			if !tc.refuse && tc.state == DaemonSkewSkewed && perr != nil {
				t.Fatalf("skewed probe unexpectedly errored: %v", perr)
			}
			if tc.state == DaemonSkewOK && build.Version != strings.TrimPrefix(tc.version, "v") {
				t.Errorf("parsed version = %q, want %q", build.Version, strings.TrimPrefix(tc.version, "v"))
			}

			var warn bytes.Buffer
			err := o.CheckDaemonSkew(context.Background(), tc.allow, &warn)
			if tc.refuse {
				if err == nil {
					t.Fatalf("CheckDaemonSkew = nil, want refusal for version %q", tc.version)
				}
				msg := err.Error()
				for _, want := range []string{
					"0.1.3",            // the installed version
					"commit a1b2c3d",   // the installed commit
					"built 2026-08-30", // the installed build date
					MinDaemonVersion,   // the required minimum
					"exit status 254",  // the operator-visible failure mode
					"bunker exec, mount and cp",
					"upgrade the daemon",
					"--uninstall --apply", // remediation 2
					"Never hand-delete only the PAM drop-in",
				} {
					if !strings.Contains(msg, want) {
						t.Errorf("refusal message missing %q:\n%s", want, msg)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckDaemonSkew = %v, want nil", err)
			}
			if tc.allow && tc.state == DaemonSkewSkewed {
				w := warn.String()
				if !strings.Contains(w, "WARNING") || !strings.Contains(w, "--allow-daemon-skew") || !strings.Contains(w, "0.1.3") {
					t.Errorf("override warning missing/broken:\n%s", w)
				}
			}
			if tc.state == DaemonSkewOK && warn.Len() != 0 {
				t.Errorf("OK daemon must not warn, got:\n%s", warn.String())
			}
		})
	}
}

func TestCheckDaemonSkew_UnknownWarnsAndNamesTheProbe(t *testing.T) {
	o, rec := sandboxOptions(t, nil, exec.ErrNotFound)
	var warn bytes.Buffer
	if err := o.CheckDaemonSkew(context.Background(), false, &warn); err != nil {
		t.Fatalf("probe failure must not refuse: %v", err)
	}
	_ = rec
	w := warn.String()
	for _, want := range []string{"WARNING", "could NOT be ruled out", DefaultDaemonBinary, MinDaemonVersion, "exit 254"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q:\n%s", want, w)
		}
	}
}

func TestApply_RefusesOlderDaemonBeforeAnyMutation(t *testing.T) {
	// A full sandbox host whose daemon probe answers 0.1.3: Apply must
	// return the refusal verbatim and issue NO host command at all — not
	// even the group lookup a plan run would do.
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
	for _, want := range []string{"--allow-daemon-skew", "0.1.3", "exit 254"} {
		if !strings.Contains(w, want) {
			t.Errorf("skewed hint missing %q: %s", want, w)
		}
	}
	uw := DaemonSkewHint(DaemonSkewUnknown, DaemonBuild{Binary: DefaultDaemonBinary}, errors.New("binary not found"))
	for _, want := range []string{"WARNING", "not found", MinDaemonVersion} {
		if !strings.Contains(uw, want) {
			t.Errorf("unknown hint missing %q: %s", want, uw)
		}
	}
}
