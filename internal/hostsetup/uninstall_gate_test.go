package hostsetup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DF-BUNKER-60: `make test-short` failed on every fresh machine where the
// boundary is provisioned. The uninstall core's first privileged step — the
// atomic rewrite of the sshd PAM stack through its .bunker-tmp sibling — ran
// regardless of who called it: the CLI uninstall tests drive
// RemoveTmpNamespace with the production paths, so an unprivileged test
// process attempted (and on a provisioned host failed with permission denied
// while) rewriting /etc/pam.d/sshd. These tests pin the gate: an
// unprivileged uninstall against REAL-host-shaped options (no Options.Root
// sandbox) is a reported no-op, while a sandboxed uninstall still removes.
//
// Every path below lives under t.TempDir() (individually overridden Options
// fields with no Root — which is exactly the shape the gate keys on), so the
// suite never touches /etc.

// managedSandboxOptions returns real-host-shaped options (no sandbox Root)
// with every managed path redirected into the test's own temp directory.
func managedSandboxOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		SSHDConfigPath:    filepath.Join(dir, "pam.d", "sshd"),
		NamespaceConfDir:  filepath.Join(dir, "security", "namespace.d"),
		NamespaceConfFile: filepath.Join(dir, "security", "namespace.conf"),
		PamHelperDir:      filepath.Join(dir, "lib", "bunker"),
		TmpInstanceRoot:   filepath.Join(dir, "agent-tmp"),
	}.WithDefaults()
}

func writeSandboxSSHD(t *testing.T, o Options, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(o.SSHDConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.SSHDConfigPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTmpNamespace_UnprivilegedRealHostIsAReportedNoOp(t *testing.T) {
	if privileged() {
		t.Skip("running as root: the unprivileged real-host gate cannot engage")
	}
	for _, tc := range []struct {
		name string
		// install writes the fixtures and returns the managed paths that
		// must be reported present.
		install func(t *testing.T, o Options) []string
	}{
		{
			name: "managed PAM block present",
			install: func(t *testing.T, o Options) []string {
				body := samplePAM + strings.Join(o.NamespacePAMBlockForOptions(), "\n") + "\n"
				writeSandboxSSHD(t, o, body)
				return []string{o.SSHDConfigPath}
			},
		},
		{
			name: "namespace drop-in present without a managed block",
			install: func(t *testing.T, o Options) []string {
				writeSandboxSSHD(t, o, samplePAM)
				if err := os.MkdirAll(o.NamespaceConfDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(o.NamespaceConfPath(), []byte(NamespaceConf(o.TmpInstanceRoot)), 0o644); err != nil {
					t.Fatal(err)
				}
				return []string{o.NamespaceConfPath()}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := managedSandboxOptions(t)
			present := tc.install(t, o)
			before := readFileString(t, o.SSHDConfigPath)

			rep, err := o.RemoveTmpNamespace(context.Background())
			if err != nil {
				t.Fatalf("RemoveTmpNamespace() error = %v, want a reported no-op", err)
			}
			if got := readFileString(t, o.SSHDConfigPath); got != before {
				t.Errorf("unprivileged uninstall mutated the sshd PAM stack:\n%q\nwant\n%q", got, before)
			}
			if m := rep.Mutations(); len(m) != 0 {
				t.Errorf("unprivileged uninstall mutated host state: %+v", m)
			}
			var named []string
			for _, c := range rep.Changes {
				if c.Action == "skip" && strings.Contains(c.Detail, "root") {
					named = append(named, c.Target)
				}
			}
			if len(named) != len(present) {
				t.Errorf("report named %d root-required target(s) (%v), want %d (%v):\n%s", len(named), named, len(present), present, rep)
			}
			for _, want := range present {
				found := false
				for _, got := range named {
					if got == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("report does not name the present target %q as root-required:\n%s", want, rep)
				}
			}
		})
	}
}

// TestRemoveTmpNamespace_UnprivilegedNothingPresentFallsThrough pins the
// fall-through: on an unprovisioned host the gate stays quiet and the report
// is the usual sequence of absences with no error.
func TestRemoveTmpNamespace_UnprivilegedNothingPresentFallsThrough(t *testing.T) {
	if privileged() {
		t.Skip("running as root: the unprivileged real-host gate cannot engage")
	}
	o := managedSandboxOptions(t)
	writeSandboxSSHD(t, o, samplePAM)

	rep, err := o.RemoveTmpNamespace(context.Background())
	if err != nil {
		t.Fatalf("RemoveTmpNamespace() error = %v", err)
	}
	if m := rep.Mutations(); len(m) != 0 {
		t.Errorf("nothing to remove, yet the report records mutations: %+v", m)
	}
	if !rep.Has("skip") {
		t.Errorf("the absences are not reported:\n%s", rep)
	}
}

// TestRemoveTmpNamespace_SandboxRootStillRemoves pins the other half: with a
// sandbox Root the gate must stay quiet even for an unprivileged caller, so
// the sandboxed uninstall tests keep proving the real removal.
func TestRemoveTmpNamespace_SandboxRootStillRemoves(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)
	installed := samplePAM + strings.Join(o.NamespacePAMBlockForOptions(), "\n") + "\n"
	if err := os.WriteFile(o.SSHDConfigPath, []byte(installed), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := o.RemoveTmpNamespace(context.Background())
	if err != nil {
		t.Fatalf("RemoveTmpNamespace() error = %v", err)
	}
	if got := readFileString(t, o.SSHDConfigPath); got != samplePAM {
		t.Errorf("sandboxed uninstall did not restore the file:\n%q\nwant\n%q", got, samplePAM)
	}
	if !rep.Has("write") {
		t.Errorf("the sandboxed removal is not reported as a write:\n%s", rep)
	}
}

// TestRemoveTmpNamespace_PrivilegedRealHostShapeStillRemoves pins the ROOT
// path as unchanged: a root caller against real-host-shaped options (no
// sandbox Root) with a managed block on disk must still get the full removal
// — the gate never engages for the only caller that can actually perform it.
func TestRemoveTmpNamespace_PrivilegedRealHostShapeStillRemoves(t *testing.T) {
	if !privileged() {
		t.Skip("not running as root: the root-path proof needs a privileged process")
	}
	o := managedSandboxOptions(t)
	writeSandboxSSHD(t, o, samplePAM+strings.Join(o.NamespacePAMBlockForOptions(), "\n")+"\n")

	rep, err := o.RemoveTmpNamespace(context.Background())
	if err != nil {
		t.Fatalf("RemoveTmpNamespace() error = %v", err)
	}
	if got := readFileString(t, o.SSHDConfigPath); got != samplePAM {
		t.Errorf("root uninstall did not restore the file:\n%q\nwant\n%q", got, samplePAM)
	}
	if m := rep.Mutations(); len(m) == 0 {
		t.Errorf("root uninstall reported no mutations:\n%s", rep)
	}
}
