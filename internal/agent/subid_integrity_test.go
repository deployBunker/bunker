package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── startup gate (PASS criterion 3) ─────────────────────────────────────────

func TestVerifySubIDPath(t *testing.T) {
	t.Run("detects a cross-tenant overlap", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "subuid")
		writeFile(t, p, "bunker-a:524288:65536\nbunker-b:524289:65536\n")
		err := VerifySubIDPath(p)
		if err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Fatalf("want an overlap error, got %v", err)
		}
	})
	t.Run("adjacent ranges are clean", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "subuid")
		writeFile(t, p, "karoid:100000:65536\nbunker-a:524288:65536\nbunker-b:589824:65536\n")
		if err := VerifySubIDPath(p); err != nil {
			t.Fatalf("adjacent ranges flagged: %v", err)
		}
	})
	t.Run("missing file is clean", func(t *testing.T) {
		if err := VerifySubIDPath(filepath.Join(t.TempDir(), "nope")); err != nil {
			t.Fatalf("missing file flagged: %v", err)
		}
	})
	t.Run("malformed line is reported", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "subuid")
		writeFile(t, p, "garbage-line\n")
		if err := VerifySubIDPath(p); err == nil {
			t.Fatal("want a malformed-line error")
		}
	})
	t.Run("a duplicate line for one user is not a cross-tenant overlap", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "subuid")
		writeFile(t, p, "bunker-a:524288:65536\nbunker-a:524288:65536\n")
		if err := VerifySubIDPath(p); err != nil {
			t.Fatalf("same-user duplicate flagged: %v", err)
		}
	})
}

func TestCheckSubIDOverlaps(t *testing.T) {
	u := filepath.Join(t.TempDir(), "subuid")
	g := filepath.Join(t.TempDir(), "subgid")
	swapStringSeam(t, &subUIDPath, u)
	swapStringSeam(t, &subGIDPath, g)

	writeFile(t, u, "a:524288:65536\nb:524289:65536\n")
	writeFile(t, g, "a:524288:65536\n")
	if err := CheckSubIDOverlaps(); err == nil {
		t.Fatal("expected refusal on overlapping subuid")
	}

	writeFile(t, u, "a:524288:65536\nb:589824:65536\n")
	if err := CheckSubIDOverlaps(); err != nil {
		t.Fatalf("clean databases refused: %v", err)
	}
}

// ── migration (PASS criterion 4) ────────────────────────────────────────────

func TestMigrateSubIDPath_RewritesManagedOnly(t *testing.T) {
	dir := t.TempDir()
	swapStringSeam(t, &subIDLockDir, dir)
	p := filepath.Join(dir, "subuid")
	// The exact defect shape: uid-derived, overlapping managed entries, plus a
	// human user that must survive untouched.
	writeFile(t, p, "kara:100000:65536\nbunker-1001:1001:65536\nbunker-1002:1002:65536\n")

	moved, err := MigrateSubIDPath(p)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}
	out := mustRead(t, p)
	if !strings.Contains(out, "kara:100000:65536") {
		t.Errorf("non-managed user changed:\n%s", out)
	}
	if strings.Contains(out, "bunker-1001:1001:") || strings.Contains(out, "bunker-1002:1002:") {
		t.Errorf("managed entries not rewritten:\n%s", out)
	}
	if err := VerifySubIDPath(p); err != nil {
		t.Errorf("migration left an overlap: %v", err)
	}
}

func TestMigrateSubIDPath_NoOpWhenDisjoint(t *testing.T) {
	dir := t.TempDir()
	swapStringSeam(t, &subIDLockDir, dir)
	p := filepath.Join(dir, "subuid")
	writeFile(t, p, "kara:100000:65536\nbunker-a:524288:65536\nbunker-b:589824:65536\n")
	before := mustRead(t, p)
	moved, err := MigrateSubIDPath(p)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if moved != 0 {
		t.Fatalf("moved = %d, want 0", moved)
	}
	if after := mustRead(t, p); after != before {
		t.Errorf("disjoint file rewritten:\n before=%q\n after =%q", before, after)
	}
}

func TestMigrateSubIDPath_PreservesCommentsAndMalformed(t *testing.T) {
	dir := t.TempDir()
	swapStringSeam(t, &subIDLockDir, dir)
	p := filepath.Join(dir, "subgid")
	writeFile(t, p, "# managed by bunker\nbunker-1001:1001:65536\nkara:100000:65536\n")
	if _, err := MigrateSubIDPath(p); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	out := mustRead(t, p)
	if !strings.Contains(out, "# managed by bunker") || !strings.Contains(out, "kara:100000:65536") {
		t.Errorf("migration dropped a non-managed line:\n%s", out)
	}
}

// After a migration of BOTH databases the whole system must be pairwise
// disjoint and readable by the startup gate.
func TestMigrateSubIDs_LeavesBothDatabasesClean(t *testing.T) {
	dir := t.TempDir()
	swapStringSeam(t, &subIDLockDir, dir)
	u := filepath.Join(dir, "subuid")
	g := filepath.Join(dir, "subgid")
	swapStringSeam(t, &subUIDPath, u)
	swapStringSeam(t, &subGIDPath, g)
	defect := "kara:100000:65536\nbunker-1001:1001:65536\nbunker-1002:1002:65536\n"
	writeFile(t, u, defect)
	writeFile(t, g, defect)

	n, err := MigrateSubIDs()
	if err != nil {
		t.Fatalf("migrate both: %v", err)
	}
	if n != 4 {
		t.Fatalf("moved = %d, want 4", n)
	}
	if err := CheckSubIDOverlaps(); err != nil {
		t.Fatalf("post-migration gate refused: %v", err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// writeFile writes a fixture database.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
