package cli

// The WIRED daemon-skew tests (INT-DEMO-001 rework). The decision table in
// internal/hostsetup covers Options.CheckDaemonSkew directly; these tests
// drive the cobra command end to end, because the first revision of the gate
// was unit-green and broken when wired: the CLI-level check honored
// --allow-daemon-skew while Options.Apply ran its own check with allow
// hardcoded false, so the override warned and then was refused anyway.
//
// Every path below lives under t.TempDir() (--daemon-binary, --scratch-root,
// --private-tmp-root) and --apply is NEVER passed: the tests run the plan/
// refusal paths only and must not touch the real host.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/hostsetup"
)

// writeDaemonVersionFixture writes a shell script that answers `--version`
// the way bunkerd does (see cmd/bunkerd/main.go) with the given version.
func writeDaemonVersionFixture(t *testing.T, version string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bunkerd-version-fixture")
	script := "#!/bin/sh\necho 'bunkerd " + version + "'\n" +
		"echo '  commit:     abcdef0'\n" +
		"echo '  built:      2026-09-16T00:00:00Z'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// runHostProvision builds the command in-process, points the mutable paths at
// the sandbox tree, runs it with ExecuteContext and returns (stdout, stderr,
// error). applyArgs are the flags under test.
func runHostProvision(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	sandbox := t.TempDir()
	cmd := NewHostProvisionCommand()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{
		"--scratch-root", filepath.Join(sandbox, "share"),
		"--private-tmp-root", filepath.Join(sandbox, "agent-tmp"),
	}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// TestHostProvisionCommand_AllowDaemonSkewProceeds is THE regression guard
// for the rework defect: the override flag must let the WIRED dry run
// complete, with exactly one override WARNING on stderr. It failed against
// the pre-fix tree, where Apply refused with allow hardcoded false.
func TestHostProvisionCommand_AllowDaemonSkewProceeds(t *testing.T) {
	bin := writeDaemonVersionFixture(t, "0.1.3")
	out, errOut, err := runHostProvision(t, "--daemon-binary", bin, "--allow-daemon-skew")
	if err != nil {
		t.Fatalf("--allow-daemon-skew dry run must proceed, got error:\n%v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "plan") {
		t.Errorf("the plan was not printed:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	// Exactly ONE override warning: the CLI-level check is the one that
	// warns; Apply's internal call stays silent.
	if n := strings.Count(errOut, "WARNING"); n != 1 {
		t.Errorf("stderr carries %d WARNING lines, want exactly 1:\n%s", n, errOut)
	}
	if !strings.Contains(errOut, "proceeding with --allow-daemon-skew") {
		t.Errorf("stderr missing the override warning:\n%s", errOut)
	}
	for _, leaked := range []string{"refusing to provision"} {
		if strings.Contains(errOut, leaked) || strings.Contains(out, leaked) {
			t.Errorf("the refusal text leaked into an overridden run:\nstdout:\n%s\nstderr:\n%s", out, errOut)
		}
	}
}

func TestHostProvisionCommand_DaemonSkewRefusal(t *testing.T) {
	bin := writeDaemonVersionFixture(t, "0.1.3")
	out, _, err := runHostProvision(t, "--daemon-binary", bin)
	if err == nil {
		t.Fatalf("skewed daemon without the override must refuse, stdout:\n%s", out)
	}
	// The refusal must name BOTH the installed revision and the required
	// minimum (it does not go through the overridden path).
	if !strings.Contains(err.Error(), "0.1.3") || !strings.Contains(err.Error(), hostsetup.MinDaemonVersion) {
		t.Errorf("refusal must name the installed revision and the minimum:\n%v", err)
	}
}

func TestHostProvisionCommand_CurrentDaemonNoWarning(t *testing.T) {
	bin := writeDaemonVersionFixture(t, hostsetup.MinDaemonVersion)
	out, errOut, err := runHostProvision(t, "--daemon-binary", bin)
	if err != nil {
		t.Fatalf("current daemon must proceed:\n%v\nstdout:\n%s", err, out)
	}
	if strings.Contains(errOut, "WARNING") {
		t.Errorf("current daemon must not warn, stderr:\n%s", errOut)
	}
}

func TestHostProvisionCommand_UninstallNeverGatedByDaemonSkew(t *testing.T) {
	// The uninstall path is deliberately never gated (INT-DEMO-001):
	// returning the host to a shared /tmp must always remain possible,
	// whatever daemon is installed — even one reporting an old version.
	bin := writeDaemonVersionFixture(t, "0.1.3")
	out, errOut, err := runHostProvision(t, "--daemon-binary", bin, "--uninstall")
	if err != nil {
		t.Fatalf("uninstall dry run must never be gated by the daemon skew:\n%v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "dry run") {
		t.Errorf("uninstall dry run did not say so:\n%s", out)
	}
	if strings.Contains(errOut, "WARNING") {
		t.Errorf("uninstall must not run the skew probe warning path, stderr:\n%s", errOut)
	}
}
