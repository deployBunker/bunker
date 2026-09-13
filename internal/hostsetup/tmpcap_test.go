package hostsetup

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestMergeTmpMountOptions(t *testing.T) {
	const cap = uint64(2147483648)
	tests := []struct {
		name     string
		existing string
		want     string
	}{
		{"appends when absent", "mode=1777,strictatime,nosuid,nodev", "mode=1777,strictatime,nosuid,nodev,size=2147483648"},
		{"replaces in place, preserving order", "rw,relatime,size=1024,nosuid", "rw,relatime,size=2147483648,nosuid"},
		{"only a cap", "", "size=2147483648"},
		{"drops empty tokens", "rw,,nodev", "rw,nodev,size=2147483648"},
		{"trims whitespace", " rw , nosuid ", "rw,nosuid,size=2147483648"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeTmpMountOptions(tc.existing, cap); got != tc.want {
				t.Errorf("MergeTmpMountOptions(%q) = %q, want %q", tc.existing, got, tc.want)
			}
		})
	}
}

// TestRenderTmpMountDropIn pins the installed drop-in and asserts the one
// property the task demands explicitly: no /etc/fstab involvement.
func TestRenderTmpMountDropIn(t *testing.T) {
	const cap = uint64(2147483648)
	got := RenderTmpMountDropIn(DefaultTmpMountOptions, cap)

	mustContain := []string{
		"[Mount]",
		"Options=mode=1777,strictatime,nosuid,nodev,size=2147483648",
		"GAP-075",
		"tmp.mount",
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("drop-in missing %q:\n%s", want, got)
		}
	}
	// A comment may explain that fstab is untouched; a CONFIGURATION line that
	// references it would mean Bunker installed an fstab entry.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "fstab") {
			t.Errorf("drop-in has a non-comment fstab reference: %q", line)
		}
	}
	if strings.Contains(got, "tmpfs /tmp") {
		t.Errorf("drop-in contains a mount command rather than unit configuration:\n%s", got)
	}
}

func TestSizeOption(t *testing.T) {
	tests := []struct {
		options string
		want    uint64
	}{
		{"rw,size=1048576,nodev", 1048576},
		{"size=4096", 4096},
		{"rw,nodev", 0},
		{"", 0},
		{"rw,size=notanumber", 0},
	}
	for _, tc := range tests {
		if got := sizeOption(tc.options); got != tc.want {
			t.Errorf("sizeOption(%q) = %d, want %d", tc.options, got, tc.want)
		}
	}
}

func TestEnsureHostTmpCap_Apply(t *testing.T) {
	const cap = uint64(2147483648)

	t.Run("non-tmpfs /tmp installs the drop-in and skips the live step", func(t *testing.T) {
		rec := newRecorder(t)
		rec.tmpFSType = "ext4"
		rec.tmpOptions = "rw,relatime"
		o := rec.options(func(oo *Options) { oo.HostTmpMaxBytes = cap })

		rep, err := o.EnsureHostTmpCap(context.Background(), true)
		if err != nil {
			t.Fatalf("EnsureHostTmpCap() error = %v", err)
		}
		body, err := os.ReadFile(o.TmpMountDropInPath())
		if err != nil {
			t.Fatalf("drop-in not written: %v", err)
		}
		// The drop-in must carry the stock tmp.mount options plus the cap:
		// the live options of a non-tmpfs /tmp are irrelevant to tmp.mount.
		if want := "Options=" + DefaultTmpMountOptions + ",size=2147483648"; !strings.Contains(string(body), want) {
			t.Errorf("drop-in = %s, want it to contain %q", body, want)
		}
		if rec.ranPrefix("mount -o remount") {
			t.Errorf("remounted a non-tmpfs /tmp (calls: %s)", rec)
		}
		if !rep.Has("write") {
			t.Errorf("report has no write action: %s", rep)
		}
	})

	t.Run("live tmpfs is remounted non-destructively at the cap", func(t *testing.T) {
		rec := newRecorder(t)
		o := rec.options(func(oo *Options) { oo.HostTmpMaxBytes = cap })
		if _, err := o.EnsureHostTmpCap(context.Background(), true); err != nil {
			t.Fatalf("EnsureHostTmpCap() error = %v", err)
		}
		if !rec.ran("mount -o remount,size=2147483648 /tmp") {
			t.Errorf("live cap not applied (calls: %s)", rec)
		}
		if !rec.ran("systemctl daemon-reload") {
			t.Errorf("systemd not reloaded after writing the drop-in (calls: %s)", rec)
		}
	})

	t.Run("already bounded tmpfs is a no-op on the second run", func(t *testing.T) {
		rec := newRecorder(t)
		o := rec.options(func(oo *Options) { oo.HostTmpMaxBytes = cap })
		if _, err := o.EnsureHostTmpCap(context.Background(), true); err != nil {
			t.Fatalf("first apply: %v", err)
		}
		before, err := os.ReadFile(o.TmpMountDropInPath())
		if err != nil {
			t.Fatal(err)
		}
		rec.calls = nil
		rep, err := o.EnsureHostTmpCap(context.Background(), true)
		if err != nil {
			t.Fatalf("second apply: %v", err)
		}
		after, _ := os.ReadFile(o.TmpMountDropInPath())
		if string(before) != string(after) {
			t.Errorf("second apply rewrote the drop-in")
		}
		if rec.ranPrefix("mount -o remount") {
			t.Errorf("second apply remounted an already-capped /tmp (calls: %s)", rec)
		}
		if m := rep.Mutations(); len(m) != 0 {
			t.Errorf("second apply mutated host state: %+v", m)
		}
	})

	t.Run("dry run changes nothing", func(t *testing.T) {
		rec := newRecorder(t)
		o := rec.options(func(oo *Options) { oo.HostTmpMaxBytes = cap })
		rep, err := o.EnsureHostTmpCap(context.Background(), false)
		if err != nil {
			t.Fatalf("EnsureHostTmpCap(dry) error = %v", err)
		}
		if _, err := os.Stat(o.TmpMountDropInPath()); !os.IsNotExist(err) {
			t.Errorf("dry run created the drop-in: %v", err)
		}
		if mutating := rec.mutating(); len(mutating) != 0 {
			t.Errorf("dry run issued mutating commands: %v", mutating)
		}
		if m := rep.Mutations(); len(m) != 0 {
			t.Errorf("dry run reported mutations: %+v", m)
		}
	})
}

// TestHostTmpCap_NeverTouchesFstab is the anti-regression check for the one
// explicit prohibition in the task: no code path may read or rewrite fstab.
func TestHostTmpCap_NeverTouchesFstab(t *testing.T) {
	rec := newRecorder(t)
	o := rec.options(nil)

	steps := []func() error{
		func() error { _, err := o.EnsureHostTmpCap(context.Background(), true); return err },
		func() error { _, err := o.RemoveHostTmpCap(context.Background(), true); return err },
		func() error { _, err := o.EnsureHostTmpCap(context.Background(), false); return err },
		func() error { _, err := o.Status(context.Background()); return err },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	for _, c := range rec.calls {
		if strings.Contains(c, "fstab") {
			t.Errorf("a command referenced fstab: %q", c)
		}
	}
}

func TestRemoveHostTmpCap(t *testing.T) {
	rec := newRecorder(t)
	o := rec.options(nil)

	// Absent: skip, no error.
	rep, err := o.RemoveHostTmpCap(context.Background(), true)
	if err != nil {
		t.Fatalf("RemoveHostTmpCap(absent) error = %v", err)
	}
	if m := rep.Mutations(); len(m) != 0 {
		t.Errorf("absent drop-in reported mutations: %+v", m)
	}

	// Installed: dry run keeps it, apply removes it.
	if _, err := o.EnsureHostTmpCap(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := o.RemoveHostTmpCap(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.TmpMountDropInPath()); err != nil {
		t.Errorf("dry-run removal deleted the drop-in: %v", err)
	}
	if _, err := o.RemoveHostTmpCap(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.TmpMountDropInPath()); !os.IsNotExist(err) {
		t.Errorf("drop-in still present after removal: %v", err)
	}
}

func TestHostTmpStatus(t *testing.T) {
	rec := newRecorder(t)
	rec.tmpFSType = "tmpfs"
	rec.tmpOptions = "rw,relatime,size=536870912,nosuid,nodev"
	o := rec.options(func(oo *Options) { oo.HostTmpMaxBytes = 1073741824 })

	st, err := o.HostTmpStatus(context.Background())
	if err != nil {
		t.Fatalf("HostTmpStatus() error = %v", err)
	}
	if !st.IsTmpfs || st.FSType != "tmpfs" {
		t.Errorf("status = %+v, want tmpfs", st)
	}
	if st.SizeBytes != 536870912 {
		t.Errorf("live size = %d, want 536870912", st.SizeBytes)
	}
	if st.DropInPresent {
		t.Errorf("status claims a drop-in that was never installed")
	}

	if _, err := o.EnsureHostTmpCap(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	st, err = o.HostTmpStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.DropInPresent || st.DropInCappedBytes != 1073741824 {
		t.Errorf("status after install = %+v, want drop-in at 1073741824", st)
	}
}

// TestApply_SkipHostTmpCap proves the opt-out is honored: hosts whose /tmp is
// managed elsewhere must be able to run the installer without it touching the
// host /tmp configuration at all.
func TestApply_SkipHostTmpCap(t *testing.T) {
	rec := newRecorder(t)
	o, _ := sshSandbox(t, rec, samplePAM)
	o.SkipHostTmpCap = true

	rep, err := o.Apply(context.Background(), true)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if _, err := os.Stat(o.TmpMountDropInPath()); !os.IsNotExist(err) {
		t.Errorf("skip-host-tmp-cap still installed the drop-in: %v", err)
	}
	if rec.ranPrefix("mount -o remount") {
		t.Errorf("skip-host-tmp-cap remounted /tmp: %v", rec.calls)
	}
	if !rep.Has("skip") {
		t.Errorf("report does not record the skip: %s", rep)
	}
}
