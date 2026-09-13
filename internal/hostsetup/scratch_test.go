package hostsetup

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
func TestEnsureAgentScratch(t *testing.T) {
	const cap = uint64(268435456)

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
	}{
		{
			name: "fresh provision mounts bounded tmpfs and sets setgid ownership",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
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
			},
			wantMount:      true,
			wantChownChmod: true,
		},
		{
			name: "missing group is created",
			set: func(r *recorder, o *Options) {
				r.groupExists = false
			},
			wantMount:      true,
			wantChownChmod: true,
			wantUsermod:    true,
		},
		{
			name: "already mounted at the configured cap only re-asserts ownership",
			set: func(r *recorder, o *Options) {
				r.groupExists = true
				r.groupMembers["bunker-agent-a"] = true
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
