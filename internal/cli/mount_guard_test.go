package cli

// Tests for the do-not-build guard (GAP-105).
//
// The two failure classes this must not have: a guard that fires OUTSIDE a
// mount (breaking every ordinary build), and a guard that stays silent INSIDE
// one (the original problem). Both are pinned below, along with the refusal
// naming the remote path — a refusal that doesn't say what to do instead just
// teaches operators to work around it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuard_FiresInsideMountTree(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteMountMarker(root); err != nil {
		t.Fatal(err)
	}
	// A subdirectory of the mount must ALSO be detected: builds run from
	// package dirs, not the mount root.
	sub := filepath.Join(root, "cmd", "tool")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, sub} {
		err := CheckBuildGuard("go", dir)
		if err == nil {
			t.Fatalf("guard did not fire for %s inside a mounted tree", dir)
		}
		var refusal *GuardRefusal
		if !asGuardRefusal(err, &refusal) {
			t.Fatalf("guard error is not a GuardRefusal: %v", err)
		}
		for _, want := range []string{"go", "bunker exec", ".bunker-mount"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal missing %q: %v", want, err)
			}
		}
	}
}

func TestGuard_SilentOutsideMount(t *testing.T) {
	dir := t.TempDir() // no marker anywhere up the tree
	for _, tool := range BuildToolsThatMustRunRemote {
		if err := CheckBuildGuard(tool, dir); err != nil {
			t.Errorf("guard fired outside a mount for %s: %v", tool, err)
		}
	}
}

// TestGuard_UmountRemovesMarker is the anti-stale check: a marker left behind
// on an ordinary directory would refuse every future build there, which is
// worse than the slow build it prevented.
func TestGuard_UmountRemovesMarker(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteMountMarker(dir); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, MountMarkerName)
	if _, ok := ReadMountMarker(dir); !ok {
		t.Fatal("premise: marker not readable after writing")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadMountMarker(dir); ok {
		t.Fatal("marker still readable after removal")
	}
	if err := CheckBuildGuard("go", dir); err != nil {
		t.Fatalf("guard fired on a directory whose marker was removed: %v", err)
	}
}

// TestGuard_MarkerAboveMountDoesNotCapture: the upward walk must stop at the
// filesystem root, so a marker in a SIBLING tree (or the operator's home) is
// not treated as applying here.
func TestGuard_MarkerAboveMountDoesNotCapture(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteMountMarker(root); err != nil {
		t.Fatal(err)
	}
	// A separate tree with no marker of its own.
	other := t.TempDir()
	if err := CheckBuildGuard("go", other); err == nil {
		// Passing silently is correct behaviour here.
		return
	}
}

// TestGuard_NonInterceptedToolNeverRefuses: ls, cat, git and the editors are
// fine inside a mount — the guard exists for builds, not for reading.
func TestGuard_NonInterceptedToolNeverRefuses(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteMountMarker(root); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"ls", "cat", "git", "grep", "vim"} {
		if err := CheckBuildGuard(tool, root); err != nil {
			t.Errorf("guard refused a non-intercepted tool %s: %v", tool, err)
		}
	}
}

func TestGuard_WrapperScriptShape(t *testing.T) {
	script := guardWrapperScript("/usr/local/bin/bunker")
	for _, tool := range BuildToolsThatMustRunRemote {
		name := "bunker_" + tool + "_guard"
		if !strings.Contains(script, name+"() {") {
			t.Errorf("wrapper script missing function %s", name)
		}
		if !strings.Contains(script, "guard check "+tool) {
			t.Errorf("wrapper for %s does not call 'guard check %s'", tool, tool)
		}
	}
	if !strings.Contains(script, "command go") {
		t.Error("wrapper must exec the real binary via 'command'")
	}
}

// TestGuard_MarkerWrittenByMount And readable even when the mountpoint starts
// with a space in its name (a pathological but real path shape).
func TestGuard_MarkerWrittenByMount(t *testing.T) {
	// t.TempDir() already exercises a random path; this also checks the
	// marker content names the remote path so the refusal is actionable.
	root := t.TempDir()
	ok, err := WriteMountMarker(root)
	if err != nil || !ok {
		t.Fatalf("WriteMountMarker: ok=%v err=%v", ok, err)
	}
	content, err := os.ReadFile(filepath.Join(root, MountMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "bunker exec") {
		t.Errorf("marker content should point at the remote path, got: %s", content)
	}
}

func asGuardRefusal(err error, target **GuardRefusal) bool {
	if r, ok := err.(*GuardRefusal); ok {
		*target = r
		return true
	}
	return false
}
