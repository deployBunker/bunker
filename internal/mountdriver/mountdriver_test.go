// Package mountdriver tests: the registry invariant (MOUNT-006 AC c) and
// the named-refusal contract for unknown drivers.
package mountdriver

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRegistryEveryDriverClassifiesOrDeclaresAbsence is the seam's core
// invariant (board AC c): every registered driver EITHER provides a failure
// classifier OR declares its absence with a non-empty reason. A driver that
// silently inherited the sshfs-shaped durability layer — or silently skipped
// it — would make retry behaviour unattributable; this test turns that into
// a hard failure at registration time (Validate panics) and again here, so
// the invariant holds no matter which future driver rows (rclone,
// FUSE/io_uring) add entries.
func TestRegistryEveryDriverClassifiesOrDeclaresAbsence(t *testing.T) {
	if err := RegistryInvariantError(); err != nil {
		t.Fatalf("registry invariant violated: %v", err)
	}
	names := RegisteredNames()
	if len(names) == 0 {
		t.Fatal("no drivers registered — the mount driver registry is empty")
	}
	for _, name := range names {
		d, err := Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q) failed right after listing it: %v", name, err)
		}
		switch {
		case d.Classify != nil:
			// Classifier present: nothing more to require.
		case d.NoClassifier != nil:
			if strings.TrimSpace(d.NoClassifier.Why) == "" {
				t.Errorf("driver %q declares no classifier but gives no reason", name)
			}
		default:
			t.Errorf("driver %q provides neither a classifier nor a declared absence", name)
		}
	}
}

// TestDefaultDriverIsSSHFS pins the default: the driver every request that
// does not name one resolves to must be sshfs, so the legacy opaque
// sshfs_mount contract keeps its meaning.
func TestDefaultDriverIsSSHFS(t *testing.T) {
	d, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\") failed: %v", err)
	}
	if d.Name != DefaultDriver {
		t.Errorf("default driver = %q, want %q", d.Name, DefaultDriver)
	}
	if d.Name != "sshfs" {
		t.Errorf("default driver = %q, want \"sshfs\" (the legacy default must not move)", d.Name)
	}
	if d.Classify == nil {
		t.Error("sshfs driver must carry a failure classifier")
	}
}

// TestResolveUnknownDriverIsNamedRefusal proves the refusal contract: an
// unknown driver name errors, names the offender, and never silently falls
// back to the default driver.
func TestResolveUnknownDriverIsNamedRefusal(t *testing.T) {
	d, err := Resolve("rclone-does-not-exist")
	if err == nil {
		t.Fatal("unknown driver resolved without error — silent fallback?")
	}
	if d.Name != "" {
		t.Errorf("unknown-driver Resolve returned a driver (%q); it must return none", d.Name)
	}
	if !errors.Is(err, ErrUnknownDriver) {
		t.Errorf("error does not match ErrUnknownDriver: %v", err)
	}
	if !strings.Contains(err.Error(), "rclone-does-not-exist") {
		t.Errorf("refusal does not name the offending driver: %v", err)
	}
	if !strings.Contains(err.Error(), "sshfs") {
		t.Errorf("refusal should list registered drivers for diagnosability: %v", err)
	}
}

// TestClassifySSHFS pins the moved sshfs classifier's precedence: permanent
// fragments beat transient ones even when both appear (retrying an auth
// failure would only add noise).
func TestClassifySSHFS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		output     string
		err        error
		wantClass  ClassifierClass
		wantReason string
	}{
		{name: "permanent beats transient", output: "Permission denied\nConnection reset by peer", wantClass: ClassPermanent, wantReason: "permission denied"},
		{name: "transient fragment", output: "read: Connection reset by peer", wantClass: ClassTransient, wantReason: "connection reset by peer"},
		{name: "signal kill is transient", output: "", err: &exec.ExitError{ProcessState: signaledState(t)}, wantClass: ClassTransient, wantReason: "killed by signal"},
		{name: "no signal is unknown", output: "some noise", wantClass: ClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, reason := classifySSHFS(tc.output, tc.err)
			if class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
			if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", reason, tc.wantReason)
			}
		})
	}
}

// TestTransientFragmentsSSHFS is a copy-guard: the exposed fragments must
// stay lowercase (the contains-check contract) and keep the canonical
// session-limit fragment.
func TestTransientFragmentsSSHFS(t *testing.T) {
	frags := TransientFragmentsSSHFS()
	if len(frags) == 0 {
		t.Fatal("no transient fragments exposed")
	}
	found := false
	for _, f := range frags {
		if f != strings.ToLower(f) {
			t.Errorf("fragment %q is not lowercase", f)
		}
		if f == "connection reset by peer" {
			found = true
		}
	}
	if !found {
		t.Error("canonical transient fragment 'connection reset by peer' missing")
	}
}

// TestValidateRejectsUndeclaredDrivers pins the invariant at the validator:
// a driver without either a classifier or a declared absence must never be
// registrable.
func TestValidateRejectsUndeclaredDrivers(t *testing.T) {
	cases := []struct {
		name   string
		driver Driver
	}{
		{name: "neither", driver: Driver{Name: "no-invariants"}},
		{name: "both", driver: Driver{Name: "schizophrenic", Classify: classifySSHFS, NoClassifier: &NoClassifierReason{Why: "n/a"}}},
		{name: "unnamed", driver: Driver{Classify: classifySSHFS}},
	}
	for _, tc := range cases {
		if err := tc.driver.Validate(); err == nil {
			t.Errorf("%s: Validate accepted an invalid driver", tc.name)
		}
	}
}

// signaledState builds a ProcessState whose WaitStatus records a signal, so
// the classifier's signal branch is reachable in a unit test. ProcessState
// cannot be constructed directly; a real process killed by a signal provides
// one.
func signaledState(t *testing.T) *os.ProcessState {
	t.Helper()
	cmd := exec.Command("sh", "-c", "kill -TERM $$")
	_ = cmd.Run() // the shell dies by signal; the error IS the fixture
	if cmd.ProcessState == nil {
		t.Fatal("fixture process produced no ProcessState")
	}
	return cmd.ProcessState
}
