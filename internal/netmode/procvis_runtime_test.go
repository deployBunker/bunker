package netmode

// Tests for the REAL-/proc half of the ISO-001 gate: the running system
// /proc that hosts every bunkerd unit must carry the host-wide hiding
// (`hidepid=` — the kernel reports the level as the value, e.g.
// hidepid=invisible), or sibling agents' processes remain in each other's
// view. The gate reads /proc/mounts through a package-level path var so the
// hermetic tests drive real and refusal arms from fixtures; the LIVE arm
// reads the real /proc/mounts and asserts the gate is DECIDABLE there (a
// verdict — refusal or pass — is required; silence is not).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withProcMounts points procMountsPath at the given text for the test.
func withProcMounts(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "mounts")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	orig := procMountsPath
	t.Cleanup(func() { procMountsPath = orig })
	procMountsPath = p
}

// A realistic /proc/mounts body: the escapes (\040) fstab-style whitespace,
// a bind of /proc somewhere else (NOT the /proc mountpoint — it must not be
// matched), and the trailing shape the kernel emits.
const realisticMounts = `sysfs /sysfs sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
tmpfs /tmp tmpfs rw,nosuid,nodev 0 0
/dev/nvme0n1p2 / ext4 rw,relatime 0 0
proc /mnt/proc\040bind proc rw 0 0
none /run/user/1005 tmpfs rw,size=16m,mode=700 0 0
`

func TestVerifyProcVisRuntimeRealisticUnhiddenRefuses(t *testing.T) {
	withProcMounts(t, realisticMounts)
	err := VerifyProcVisRuntime()
	if err == nil {
		t.Fatal("a default (-hidepid) real /proc passed the gate — sibling agents' processes would remain visible (the ISO-001 leak)")
	}
	if _, ok := err.(*ProcVisMountOptionsError); !ok {
		t.Fatalf("refusal is %T, want *ProcVisMountOptionsError", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"procvis mode unavailable on this host", // the named refusal prefix
		"hidepid",                               // the missing property is named
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q missing %q", msg, want)
		}
	}
}

func TestVerifyProcVisRuntimeTable(t *testing.T) {
	tests := []struct {
		name       string
		mounts     string
		wantRefuse bool
		wantText   []string // substrings required in the refusal (when refuse)
	}{
		{
			name:   "hidden+gid passes",
			mounts: "proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=2,gid=59999 0 0\n",
		},
		{
			name:   "hidden level-name spelling passes",
			mounts: "proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=invisible,procgroupid=59999 0 0\n",
		},
		{
			name:       "hidden without gid refuses (gate not verifiable on shipped hosts)",
			mounts:     "proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=2 0 0\n",
			wantRefuse: true,
			wantText:   []string{"procvis mode unavailable", "gid/procgroupid"},
		},
		{
			name:       "no proc line refuses loudly",
			mounts:     "sysfs /sys sysfs rw 0 0\n",
			wantRefuse: true,
			wantText:   []string{"procvis mode unavailable", "no procfs line"},
		},
		{
			name:       "\044 does not fool it — a bind under /mnt is never matched",
			mounts:     "proc /proc\044sub proc rw 0 0\nproc /proc proc rw,relatime,hidepid=2,gid=59999 0 0\n",
			wantRefuse: false, // the real /proc line carries both options
		},
		{
			name:       "other filesystems' gid= options never satisfy the proc line check",
			mounts:     "tmpfs /run tmpfs rw,gid=99 0 0\nproc /proc proc rw,relatime 0 0\n",
			wantRefuse: true,
			wantText:   []string{"procvis mode unavailable", "hidepid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withProcMounts(t, tt.mounts)
			err := VerifyProcVisRuntime()
			if got := err != nil; got != tt.wantRefuse {
				t.Fatalf("VerifyProcVisRuntime() refused=%v, want %v (err: %v)", got, tt.wantRefuse, err)
			}
			if err == nil {
				return
			}
			if _, ok := err.(*ProcVisMountOptionsError); !ok {
				t.Fatalf("refusal is %T, want *ProcVisMountOptionsError", err)
			}
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

// TestVerifyProcVisRuntimeFindProcMountLine pins the line selection unit:
// the SECOND field is the mountpoint, and ONLY exactly-"proc" matches; the
// trailing options of unrelated lines and the escaped whitespace forms must
// never produce a false match (a gate pointed at the wrong line would pass
// or refuse for the wrong reason).
func TestVerifyProcVisRuntimeFindProcMountLine(t *testing.T) {
	tests := []struct {
		name     string
		mounts   string
		wantLine string
		wantOK   bool
	}{
		{name: "realistic body matches the real /proc line", mounts: realisticMounts, wantLine: "proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0", wantOK: true},
		{name: "no proc line", mounts: "sysfs /sys sysfs rw 0 0\n", wantOK: false},
		{name: "empty body", mounts: "", wantOK: false},
		{name: "a /proc/bind mount is not /proc", mounts: "proc /proc/bind proc rw 0 0\n", wantOK: false},
		{name: "escaped-space mountpoint matched by its unescaped fields", mounts: "proc /proc proc rw,relatime,hidepid=2,gid=59999 0 0\n", wantLine: "proc /proc proc rw,relatime,hidepid=2,gid=59999 0 0", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := findProcMountLine(tt.mounts)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.wantLine {
				t.Errorf("line = %q, want %q", got, tt.wantLine)
			}
		})
	}
}

// TestVerifyProcVisRuntimeReadFailureRefuses pins the UNREADABLE arm: a
// /proc/mounts that cannot be read is an environment failure, and the gate
// must refuse (never pass silently on an unverifiable host).
func TestVerifyProcVisRuntimeReadFailureRefuses(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	orig := procMountsPath
	t.Cleanup(func() { procMountsPath = orig })
	procMountsPath = missing
	err := VerifyProcVisRuntime()
	if err == nil {
		t.Fatal("an unreadable /proc/mounts passed the gate — refusal required")
	}
	if !strings.Contains(err.Error(), "procvis mode unavailable") {
		t.Errorf("refusal %q missing the named prefix", err.Error())
	}
}
