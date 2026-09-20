package hostsetup

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestScratchMountArgs(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		size uint64
		want []string
	}{
		{
			name: "default cap",
			dir:  "/srv/bunker-share/agent-a",
			size: 268435456,
			want: []string{"-t", "tmpfs", "-o", "size=268435456,mode=0770,nosuid,nodev", "tmpfs", "/srv/bunker-share/agent-a"},
		},
		{
			name: "custom cap",
			dir:  "/srv/bunker-share/agent-b",
			size: 1024,
			want: []string{"-t", "tmpfs", "-o", "size=1024,mode=0770,nosuid,nodev", "tmpfs", "/srv/bunker-share/agent-b"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScratchMountArgs(tc.dir, tc.size)
			if len(got) != len(tc.want) {
				t.Fatalf("ScratchMountArgs() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ScratchMountArgs()[%d] = %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestScratchRemountArgs(t *testing.T) {
	got := ScratchRemountArgs("/srv/bunker-share/a", 4096)
	want := []string{"-o", "remount,size=4096", "/srv/bunker-share/a"}
	if len(got) != len(want) {
		t.Fatalf("ScratchRemountArgs() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("ScratchRemountArgs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestEnsureAgentScratch exercises the whole per-agent provision table: the
// exact mount argv, group membership, the setgid chmod, the bounded remount of
// a changed cap, and the FAIL-CLOSED path where a failed mount must not leave
// an unbounded writable directory behind.
//
// The table drives the non-root side of the privilege split (a process that
// cannot repair the exchange root), so every row whose fixture leaves the root
// missing or wrong is expected to FAIL rather than proceed: see
// setUnrepairable, which pins that side explicitly because the suite's own uid
// decides it. The repairable side — the real daemon's, running as root — is
// covered by TestEnsureAgentScratch_ProvisionsExchangeRoot.
func TestEnsureAgentScratch(t *testing.T) {
	const cap = uint64(268435456)
	setUnrepairable(t)

	tests := []struct {
		name string
		set  func(r *recorder, o *Options)
		// assertions
		wantErr        bool
		wantDirGone    bool
		wantMount      bool
		wantRemount    bool
		wantChownChmod bool
		wantUsermod    bool
		wantSkipOnly   bool
		// wantErrContains asserts the MESSAGE, not just that an error came
		// back: a failure on this path must name the operator remedy, or an
		// operator sees a spawn that quietly lost its exchange directory.
		wantErrContains string
	}{
		{
			name: "fresh provision mounts bounded tmpfs and sets setgid ownership",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				// A host provisioned by the installer has the correct exchange
				// root; this case pins that the root is NOT re-paved for it
				// (the spawn path runs on every spawn).
				r.setScratchRootShape("0", "1001", "2750")
			},
			wantMount:      true,
			wantChownChmod: true,
			wantUsermod:    true,
		},
		{
			name: "agent already in group is not re-added",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				r.groupMembers["bunker-agent-a"] = true
				r.setScratchRootShape("0", "1001", "2750")
			},
			wantMount:      true,
			wantChownChmod: true,
		},
		{
			name: "a group that had to be created leaves the root to the operator",
			set: func(r *recorder, o *Options) {
				r.groupExists = false
			},
			wantErr:         true,
			wantUsermod:     true,
			wantErrContains: "host-provision --apply",
		},
		{
			name: "already mounted at the configured cap only re-asserts ownership",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				r.groupMembers["bunker-agent-a"] = true
				r.setScratchRootShape("0", "1001", "2750")
				dir := o.ScratchDir("agent-a")
				if err := os.MkdirAll(dir, 0o750); err != nil {
					r.t.Fatal(err)
				}
				r.mounts[dir] = true
				r.mountOpts[dir] = ScratchMountOptions(cap)
			},
			wantChownChmod: true,
		},
		{
			name: "already mounted with a stale cap is remounted non-destructively",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				r.groupMembers["bunker-agent-a"] = true
				r.setScratchRootShape("0", "1001", "2750")
				dir := o.ScratchDir("agent-a")
				if err := os.MkdirAll(dir, 0o750); err != nil {
					r.t.Fatal(err)
				}
				r.mounts[dir] = true
				r.mountOpts[dir] = ScratchMountOptions(cap * 2)
			},
			wantRemount:    true,
			wantChownChmod: true,
		},
		{
			name: "mount failure fails closed: no directory, no ownership change",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				r.groupMembers["bunker-agent-a"] = true
				r.setScratchRootShape("0", "1001", "2750")
				r.failMount = true
			},
			wantErr:     true,
			wantDirGone: true,
			wantMount:   true, // the mount is attempted and fails
		},
		{
			name: "disabled config provisions nothing",
			set: func(r *recorder, o *Options) {
				o.ScratchEnabled = false
			},
			wantSkipOnly: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t)
			o := rec.options(func(oo *Options) { oo.ScratchPerAgent = cap })
			if tc.set != nil {
				tc.set(rec, &o)
			}
			dir := o.ScratchDir("agent-a")

			rep, err := o.EnsureAgentScratch(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001)
			if tc.wantErr && err == nil {
				t.Fatalf("EnsureAgentScratch() error = nil, want failure")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("EnsureAgentScratch() error = %v (calls: %s)", err, rec)
			}
			if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErrContains)
			}

			if got := rec.ran("mount -t tmpfs -o " + ScratchMountOptions(cap) + " tmpfs " + dir); got != tc.wantMount {
				t.Errorf("bounded mount issued = %v, want %v (calls: %s)", got, tc.wantMount, rec)
			}
			if got := rec.ran("mount -o remount,size=" + itoa(cap) + " " + dir); got != tc.wantRemount {
				t.Errorf("remount issued = %v, want %v (calls: %s)", got, tc.wantRemount, rec)
			}
			if got := rec.ran("chmod 2770 " + dir); got != tc.wantChownChmod {
				t.Errorf("setgid chmod issued = %v, want %v (calls: %s)", got, tc.wantChownChmod, rec)
			}
			if got := rec.ranPrefix("chown 1001:1001"); got != tc.wantChownChmod {
				t.Errorf("chown issued = %v, want %v (calls: %s)", got, tc.wantChownChmod, rec)
			}
			if got := rec.ran("usermod -aG " + DefaultScratchGroup + " bunker-agent-a"); got != tc.wantUsermod {
				t.Errorf("usermod issued = %v, want %v (calls: %s)", got, tc.wantUsermod, rec)
			}

			if tc.wantSkipOnly && len(rec.calls) != 0 {
				t.Errorf("disabled scratch issued commands: %s", rec)
			}

			_, statErr := os.Stat(dir)
			if tc.wantDirGone && !os.IsNotExist(statErr) {
				t.Errorf("fail-closed mount left %s behind (stat err = %v)", dir, statErr)
			}
			if !tc.wantErr && !tc.wantSkipOnly && os.IsNotExist(statErr) {
				t.Errorf("successful provision did not create %s", dir)
			}
			for _, c := range rep.Changes {
				if c.Action == "skip" && c.Applied {
					t.Errorf("report marked a skip as applied: %+v", c)
				}
			}
		})
	}
}

func TestEnsureAgentScratch_RequiresAgentAndUsername(t *testing.T) {
	rec := newRecorder(t)
	o := rec.options(nil)
	if _, err := o.EnsureAgentScratch(context.Background(), "", "bunker-x", 1, 1); err == nil {
		t.Fatal("expected an error for an empty agent id")
	}
	if _, err := o.EnsureAgentScratch(context.Background(), "x", "", 1, 1); err == nil {
		t.Fatal("expected an error for an empty username")
	}
}

// TestEnsureAgentScratch_ProvisionsExchangeRoot pins the ROOT, not just the
// per-agent directory: on a host where nobody ran `bunker host-provision
// --apply` the root is missing (or was created by a MkdirAll with the wrong
// owner), and a per-agent directory under it is unusable — the agent cannot
// traverse the exchange point at all. The spawn path must therefore bring the
// root to root:<agent-group> 2750 in the same step.
func TestEnsureAgentScratch_ProvisionsExchangeRoot(t *testing.T) {
	const cap = uint64(268435456)

	tests := []struct {
		name string
		set  func(r *recorder, o *Options)
		// assertions
		wantRootCommands bool
		wantChown        string
		wantChmod        string
		wantErr          bool
		wantErrContains  string
		wantErrMsg       string
	}{
		{
			name: "a correct exchange root is verified and not re-paved",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				r.groupMembers["bunker-agent-a"] = true
				r.setScratchRootShape("0", "1001", "2750")
			},
			wantRootCommands: false,
		},
		{
			name: "root:root 0750 is repaired and re-verified",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				r.groupMembers["bunker-agent-a"] = true
				r.setScratchRootShape("0", "0", "750")
			},
			wantRootCommands: true,
			wantChown:        "0:1001",
			wantChmod:        "2750",
		},
		{
			name: "a group-writable root is repaired and re-verified",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				r.groupMembers["bunker-agent-a"] = true
				r.setScratchRootShape("0", "1001", "2775")
			},
			wantRootCommands: true,
			wantChown:        "0:1001",
			wantChmod:        "2750",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The repair half is only reachable where the process is root in
			// reality: chowning the root to 0:<gid> needs it, and a sandboxed
			// path cannot accept a foreign owner. So the repair cases assert
			// what the whole spawn step DOES as root, and every case asserts
			// the final shape the agent will meet.
			if tc.wantRootCommands && !privileged() {
				setRepairable(t)
			}
			rec := newRecorder(t)
			o := rec.options(func(oo *Options) { oo.ScratchPerAgent = cap })
			tc.set(rec, &o)
			root := o.ScratchRoot
			dir := o.ScratchDir("agent-a")

			rep, err := o.EnsureAgentScratch(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001)
			if err == nil && tc.wantErr {
				t.Fatalf("EnsureAgentScratch() error = nil, want failure (calls: %s)", rec)
			}
			if err != nil && !tc.wantErr {
				t.Fatalf("EnsureAgentScratch() error = %v (calls: %s)", err, rec)
			}
			if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErrContains)
			}
			if tc.wantErrMsg != "" && !strings.Contains(err.Error(), tc.wantErrMsg) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErrMsg)
			}

			// The root commands are scoped to the ROOT path: a chmod on the
			// per-agent directory is expected in every case and must not be
			// mistaken for a root repair.
			for _, cmd := range []string{"chown " + tc.wantChown + " " + root, "chmod " + tc.wantChmod + " " + root} {
				if got, want := rec.ran(cmd), tc.wantRootCommands; got != want {
					t.Errorf("exchange root command %q issued = %v, want %v (calls: %s)", cmd, got, want, rec)
				}
			}

			if tc.wantErr {
				// A spawn that cannot reach the exchange root must not leave a
				// per-agent directory behind that the agent cannot use, and
				// must never report success.
				if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
					t.Errorf("failed scratch step left %s behind (stat err = %v)", dir, statErr)
				}
				if rec.ranPrefix("mount -t tmpfs") {
					t.Errorf("scratch was mounted under an unusable exchange root: %s", rec)
				}
				return
			}

			// Where the process really is root, the exchange root must be
			// correct on disk by the end of the step — the shape the agent
			// needs to traverse and exchange through.
			if privileged() {
				fi, statErr := os.Stat(root)
				if statErr != nil || !fi.IsDir() {
					t.Fatalf("exchange root missing after a successful scratch step: %v", statErr)
				}
				if fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm()&0o022 != 0 {
					t.Errorf("exchange root left wider than documented: mode %s", octalMode(fi.Mode().Perm()|(fi.Mode()&os.ModeSetgid)))
				}
			}

			// The observation is recorded as a check that was MADE (applied
			// false), so a healthy spawn can be told apart from one that never
			// looked at the root.
			observed := false
			for _, c := range rep.Changes {
				if c.Target == root && strings.Contains(c.Detail, "exchange root verified") {
					observed = true
					if c.Applied {
						t.Errorf("a verified root must be reported as a check, not a mutation: %+v", c)
					}
				}
			}
			if !observed {
				t.Errorf("no exchange-root verification recorded in the report: %+v", rep.Changes)
			}
		})
	}
}

// TestEnsureAgentScratch_RepairThatDoesNotTakeFails: "the chown exited 0" is
// not proof the root changed. The ownership half of the boundary is the one
// that has to be RE-OBSERVED rather than assumed: the root can be left
// root:root by exactly the MkdirAll this replaces, so a spawn that repaired the
// mode and then reported "shared scratch ready" while the root stayed root:root
// would leave the agent unable to traverse the exchange tree. This is the
// property EnsureSharedScratch's stat-back exists for, enforced on the
// per-agent path.
//
// The MODE half deliberately has no such fixture: the provisioner re-asserts it
// in-process with os.Chmod, so on a real filesystem a mode repair always takes,
// and where it cannot (a read-only filesystem) the chmod error surfaces
// directly. The ownership half is the one that can silently survive a command
// that reported success.
func TestEnsureAgentScratch_RepairThatDoesNotTakeFails(t *testing.T) {
	const cap = uint64(268435456)

	// Only the process that really may repair the root can be driven into the
	// repair path at all; the observation of the failure is what is asserted.
	setRepairable(t)

	rec := newRecorder(t)
	o := rec.options(func(oo *Options) { oo.ScratchPerAgent = cap })
	rec.groupExists = true
	rec.groupMembers["bunker-agent-a"] = true
	// The observed owner is not root, and the chown/chmod are accepted while
	// the root keeps reporting that owner: the repair did not take.
	rec.setScratchRootShape("1001", "0", "750")
	rec.scratchRootRepairIneffective = true
	dir := o.ScratchDir("agent-a")

	_, err := o.EnsureAgentScratch(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001)
	if err == nil {
		// As root the chown really lands on the fixture's directory, so this
		// simulated ineffective repair cannot be staged — the assertion below
		// is for the branch a real root does not take.
		if privileged() {
			t.Skip("a real root performs the chown, so the simulated ineffective repair cannot fail there")
		}
		t.Fatalf("a repair that did not take was reported as success (calls: %s)", rec)
	}
	if !strings.Contains(err.Error(), "host-provision --apply") {
		t.Errorf("error = %v, want it to name the operator remedy", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("failed scratch step left %s behind (stat err = %v)", dir, statErr)
	}
	if rec.ranPrefix("mount -t tmpfs") {
		t.Errorf("scratch was mounted under an unusable exchange root: %s", rec)
	}
}

// setUnrepairable pins the non-root side of the exchange-root gate: this
// process cannot chown/chmod the root, so an unusable root must fail the
// scratch step instead of being repaired. Restored automatically.
//
// It exists because "am I root" is decided by where the suite runs, not by the
// test: on a machine where `go test` executes as root the same fixture would be
// REPAIRED, and a table that only passed because the runner happened to be
// unprivileged would be testing the environment, not the code.
func setUnrepairable(t *testing.T) {
	t.Helper()
	restore := scratchRootRepairable
	scratchRootRepairable = func() bool { return false }
	t.Cleanup(func() { scratchRootRepairable = restore })
}

// setRepairable pins the root side: the process may repair the exchange root,
// which is the real daemon's situation.
func setRepairable(t *testing.T) {
	t.Helper()
	restore := scratchRootRepairable
	scratchRootRepairable = func() bool { return true }
	t.Cleanup(func() { scratchRootRepairable = restore })
}

// TestEnsureAgentScratch_UnprivilegedSurfacesWrongRoot covers the FAIL-CLOSED
// half of the gate: a process that cannot change the root (not root — a manual
// or test-context daemon) still has to CHECK it, and a wrong root there is an
// error naming the operator remedy rather than a silent "ready". Without this
// branch the check would be "only when we could have fixed it", which is
// exactly the class of check that let the defect through.
func TestEnsureAgentScratch_UnprivilegedSurfacesWrongRoot(t *testing.T) {
	const cap = uint64(268435456)

	tests := []struct {
		name            string
		shape           [3]string // owner, group, octal; empty owner = unobservable
		wantErr         bool
		wantErrContains string
	}{
		{
			name:  "a correct root passes the check",
			shape: [3]string{"0", "1001", "2750"},
		},
		{
			name:            "root:root 0750 fails and names the remedy",
			shape:           [3]string{"0", "0", "750"},
			wantErr:         true,
			wantErrContains: "host-provision --apply",
		},
		{
			name:            "a group-writable root fails and names the remedy",
			shape:           [3]string{"0", "1001", "2775"},
			wantErr:         true,
			wantErrContains: "host-provision --apply",
		},
		{
			// Owner unobservable (an unprivileged `stat` that prints nothing):
			// ownership cannot be enforced, so the mode — always observable —
			// is the check that applies, and a correct mode must not fail.
			name:  "an unobservable owner with a correct mode is not an error",
			shape: [3]string{"", "", "2750"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setUnrepairable(t)

			rec := newRecorder(t)
			o := rec.options(func(oo *Options) { oo.ScratchPerAgent = cap })
			rec.groupExists = true
			rec.groupMembers["bunker-agent-a"] = true
			rec.setScratchRootShape(tc.shape[0], tc.shape[1], tc.shape[2])
			dir := o.ScratchDir("agent-a")

			_, err := o.EnsureAgentScratch(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001)
			if err == nil && tc.wantErr {
				t.Fatalf("EnsureAgentScratch() error = nil, want failure (calls: %s)", rec)
			}
			if err != nil && !tc.wantErr {
				t.Fatalf("EnsureAgentScratch() error = %v (calls: %s)", err, rec)
			}
			if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErrContains)
			}
			// Nothing was changed, and nothing was left half-provisioned.
			if rec.ran("chmod 2750 "+o.ScratchRoot) || rec.ranPrefix("chown 0:") {
				t.Errorf("an unprivileged process attempted to change the exchange root: %s", rec)
			}
			if tc.wantErr {
				if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
					t.Errorf("failed scratch step left %s behind (stat err = %v)", dir, statErr)
				}
				if rec.ranPrefix("mount -t tmpfs") {
					t.Errorf("scratch was mounted under an unusable exchange root: %s", rec)
				}
			}
		})
	}
}

// TestEnsureAgentScratch_DoesNotRepaveACorrectRoot is the spawn-path cost and
// idempotency contract: the gate runs on EVERY spawn, so a host whose root is
// already correct must be left completely untouched — no chown/chmod command
// against the root at all — and a second call must be indistinguishable from
// the first. It runs on BOTH sides of the privilege split, because "we may
// repair it" must never mean "we repair it every time".
func TestEnsureAgentScratch_DoesNotRepaveACorrectRoot(t *testing.T) {
	const cap = uint64(268435456)

	for _, tc := range []struct {
		name       string
		repairable bool
	}{
		{name: "root daemon", repairable: true},
		{name: "unprivileged process", repairable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.repairable {
				setRepairable(t)
			} else {
				setUnrepairable(t)
			}

			rec := newRecorder(t)
			o := rec.options(func(oo *Options) { oo.ScratchPerAgent = cap })
			rec.groupExists = true
			rec.groupMembers["bunker-agent-a"] = true
			rec.setScratchRootShape("0", "1001", "2750")

			for run := 1; run <= 2; run++ {
				rec.calls = nil
				if _, err := o.EnsureAgentScratch(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001); err != nil {
					t.Fatalf("run %d: EnsureAgentScratch() error = %v (calls: %s)", run, err, rec)
				}
				// Only commands that could CHANGE the root are forbidden: the
				// read-only probe that proves it is correct is the gate doing
				// its job (see recorder.mutating).
				for _, c := range rec.mutating() {
					if strings.HasSuffix(c, " "+o.ScratchRoot) {
						t.Errorf("run %d: correct exchange root was still changed by %q (calls: %s, mutating: %v)", run, c, rec, rec.mutating())
					}
				}
			}

			fi, err := os.Stat(o.ScratchRoot)
			if err != nil || !fi.IsDir() {
				t.Fatalf("exchange root missing after a successful scratch step: %v", err)
			}
			if fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm()&0o022 != 0 {
				t.Errorf("exchange root left wider than documented: mode %s", octalMode(fi.Mode().Perm()|(fi.Mode()&os.ModeSetgid)))
			}
		})
	}
}

// TestScratchRootStatParsesRealShapes pins the observation the gate acts on,
// including the invariant the whole defect rests on: `stat -c '%a'` reports
// the setgid bit, so 2750 is what a correct root reads as and 750 is what the
// measured broken root reads as. A parser that dropped the setgid bit would
// make the gate compare 750 against 2750 and pass every broken host — and a
// parser that turned an empty answer into a parse error would fail a host
// whose ownership is merely unobservable.
//
// The two verifiers are asserted separately because they check different
// halves and must not collapse into one another: verifyScratchRoot owns the
// mode (always observable) and verifyScratchRootOwnership owns root/group
// ownership (unobservable under an unprivileged stat, where it must SKIP
// rather than pass or fail).
func TestScratchRootStatParsesRealShapes(t *testing.T) {
	const wantGID = "989"
	tests := []struct {
		name        string
		answer      string
		diskOctal   string // the real directory's permission bits
		diskSetgid  bool
		wantOwner   string
		wantGroup   string
		wantOctal   string
		wantModeOK  bool // verifyScratchRoot accepts the real directory
		wantOwnerOK bool // verifyScratchRootOwnership accepts the owner
	}{
		{
			name: "correct root", answer: "0:989 2750", diskOctal: "2750", diskSetgid: true,
			wantOwner: "0", wantGroup: "989", wantOctal: "2750",
			wantModeOK: true, wantOwnerOK: true,
		},
		{
			name: "the measured defect: root:root 0750", answer: "0:0 750", diskOctal: "750",
			wantOwner: "0", wantGroup: "0", wantOctal: "750",
			wantModeOK: false, wantOwnerOK: false,
		},
		{
			name: "group-writable root", answer: "0:989 2775", diskOctal: "2775", diskSetgid: true,
			wantOwner: "0", wantGroup: "989", wantOctal: "2775",
			wantModeOK: false, wantOwnerOK: true,
		},
		{
			name: "no owner observable", answer: "", diskOctal: "2750", diskSetgid: true,
			wantOwner: "", wantGroup: "", wantOctal: "",
			wantModeOK: true, wantOwnerOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t)
			o := rec.options(nil)
			perm := os.FileMode(0o750)
			if tc.diskOctal == "2750" {
				perm = 0o750
			}
			if tc.diskOctal == "2775" {
				perm = 0o775
			}
			setgid := os.FileMode(0)
			if tc.diskSetgid {
				setgid = os.ModeSetgid
			}
			if err := os.MkdirAll(o.ScratchRoot, perm); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(o.ScratchRoot, setgid|perm); err != nil {
				t.Fatal(err)
			}
			if tc.answer != "" {
				rec.scratchOwners[o.ScratchRoot] = tc.answer
			}

			st, err := o.statScratchRoot(context.Background())
			if err != nil {
				t.Fatalf("statScratchRoot() error = %v", err)
			}
			if st.Owner != tc.wantOwner || st.Group != tc.wantGroup || st.Octal != tc.wantOctal {
				t.Errorf("statScratchRoot() = %s:%s mode %s, want %s:%s mode %s",
					st.Owner, st.Group, st.Octal, tc.wantOwner, tc.wantGroup, tc.wantOctal)
			}
			if !st.isDirectory() {
				t.Error("statScratchRoot() did not report an existing directory")
			}
			if got := verifyScratchRoot(o.ScratchRoot) == nil; got != tc.wantModeOK {
				t.Errorf("verifyScratchRoot() accepted = %v, want %v (real mode %s)", got, tc.wantModeOK, tc.diskOctal)
			}
			if got := o.verifyScratchRootOwnership(wantGID) == nil; got != tc.wantOwnerOK {
				t.Errorf("verifyScratchRootOwnership() accepted = %v, want %v (observation %q)", got, tc.wantOwnerOK, tc.answer)
			}
		})
	}
}

func TestEnsureSharedScratch(t *testing.T) {
	rec := newRecorder(t)
	o := rec.options(nil)

	rep, err := o.EnsureSharedScratch(context.Background())
	if err != nil {
		t.Fatalf("EnsureSharedScratch() error = %v", err)
	}
	if !rec.ran("groupadd --system " + DefaultScratchGroup) {
		t.Errorf("missing group was not created (calls: %s)", rec)
	}
	if !rec.ran("chown 0:1001 " + o.ScratchRoot) {
		t.Errorf("scratch root ownership not enforced (calls: %s)", rec)
	}
	if !rec.ran("chmod 2750 " + o.ScratchRoot) {
		t.Errorf("scratch root mode not enforced (calls: %s)", rec)
	}
	fi, err := os.Stat(o.ScratchRoot)
	if err != nil || !fi.IsDir() {
		t.Fatalf("scratch root not created: %v", err)
	}
	if got := fi.Mode().Perm(); got != ScratchRootMode.Perm() {
		t.Errorf("scratch root mode = %04o, want %04o", got, ScratchRootMode)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("scratch root %s lost the setgid bit: new entries would not inherit the agent group", o.ScratchRoot)
	}
	if rep.Applied() == 0 {
		t.Error("report shows no applied changes")
	}

	// Second run: the group exists, so no groupadd.
	rec.calls = nil
	if _, err := o.EnsureSharedScratch(context.Background()); err != nil {
		t.Fatalf("second EnsureSharedScratch() error = %v", err)
	}
	if rec.ran("groupadd --system " + DefaultScratchGroup) {
		t.Errorf("idempotent run recreated the group (calls: %s)", rec)
	}
}

// TestScratchRootBoundary pins the exchange root as a TABLE, because it is the
// one directory every agent can reach: it must be setgid (so entries created
// inside a per-agent directory inherit the agent group) and it must NOT be
// writable by the agent group or the world (so no agent can create an arbitrary
// plain directory or file next to its own capped tmpfs — the bypass that would
// make the per-agent caps meaningless).
func TestScratchRootBoundary(t *testing.T) {
	if ScratchRootMode.Perm()&0o022 != 0 {
		t.Fatalf("ScratchRootMode %04o is group/world writable", ScratchRootMode)
	}
	if ScratchRootMode&os.ModeSetgid == 0 && ScratchRootMode&0o2000 == 0 {
		t.Fatalf("ScratchRootMode %04o has no setgid bit", ScratchRootMode)
	}

	t.Run("an existing group-writable root is repaired and verified", func(t *testing.T) {
		rec := newRecorder(t)
		o := rec.options(nil)
		if err := os.MkdirAll(o.ScratchRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		// The bypass being closed: an operator (or an earlier revision) left
		// the exchange root group-writable.
		if err := os.Chmod(o.ScratchRoot, os.ModeSetgid|0o775); err != nil {
			t.Fatal(err)
		}
		if _, err := o.EnsureSharedScratch(context.Background()); err != nil {
			t.Fatalf("EnsureSharedScratch() error = %v", err)
		}
		fi, err := os.Stat(o.ScratchRoot)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("pre-existing group-writable root was not repaired: mode %04o", fi.Mode().Perm())
		}
	})

	t.Run("verifyScratchRoot rejects a writable or non-setgid root", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			mode    os.FileMode
			wantErr bool
		}{
			{name: "2750 is accepted", mode: os.ModeSetgid | 0o750},
			{name: "setgid + group write is rejected", mode: os.ModeSetgid | 0o770, wantErr: true},
			{name: "setgid + world write is rejected", mode: os.ModeSetgid | 0o752, wantErr: true},
			{name: "without setgid is rejected", mode: 0o750, wantErr: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "bunker-share")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, tc.mode); err != nil {
					t.Fatal(err)
				}
				err := verifyScratchRoot(dir)
				if tc.wantErr && err == nil {
					t.Fatalf("verifyScratchRoot accepted mode %04o", tc.mode)
				}
				if !tc.wantErr && err != nil {
					t.Fatalf("verifyScratchRoot(%04o) = %v, want nil", tc.mode, err)
				}
			})
		}
	})
}

func TestRemoveAgentScratch(t *testing.T) {
	rec := newRecorder(t)
	o := rec.options(nil)
	if err := os.MkdirAll(o.ScratchDir("agent-a"), 0o750); err != nil {
		t.Fatal(err)
	}

	// Mounted: umount then remove.
	dir := o.ScratchDir("agent-a")
	rec.mounts[dir] = true
	rec.mountOpts[dir] = ScratchMountOptions(o.ScratchPerAgent)
	rep, err := o.RemoveAgentScratch(context.Background(), "agent-a")
	if err != nil {
		t.Fatalf("RemoveAgentScratch() error = %v", err)
	}
	if !rec.ran("umount -l " + dir) {
		t.Errorf("mounted scratch was not unmounted (calls: %s)", rec)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("scratch dir still present after removal: %v", err)
	}
	if rep.Applied() == 0 {
		t.Error("report shows no applied changes")
	}

	// Idempotent: a second removal is a skip, not an error.
	rec.calls = nil
	if _, err := o.RemoveAgentScratch(context.Background(), "agent-a"); err != nil {
		t.Fatalf("second RemoveAgentScratch() error = %v", err)
	}
	if rec.ranPrefix("umount") {
		t.Errorf("absent scratch was still unmounted (calls: %s)", rec)
	}
}

func TestAgentScratchStatus(t *testing.T) {
	rec := newRecorder(t)
	o := rec.options(nil)
	dir := o.ScratchDir("agent-a")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	rec.mounts[dir] = true
	rec.mountOpts[dir] = ScratchMountOptions(o.ScratchPerAgent)

	st, err := o.AgentScratchStatus(context.Background(), "agent-a")
	if err != nil {
		t.Fatalf("AgentScratchStatus() error = %v", err)
	}
	if !st.Exists || !st.Mounted {
		t.Errorf("status = %+v, want exists+mounted", st)
	}
	if want := "size=" + itoa(o.ScratchPerAgent); st.Size != want {
		t.Errorf("status size = %q, want %q", st.Size, want)
	}

	missing, err := o.AgentScratchStatus(context.Background(), "never-provisioned")
	if err != nil {
		t.Fatalf("AgentScratchStatus(missing) error = %v", err)
	}
	if missing.Exists {
		t.Errorf("status for an unprovisioned agent claims existence: %+v", missing)
	}
}

// itoa keeps the assertions above readable.
func itoa(v uint64) string { return strconv.FormatUint(v, 10) }
