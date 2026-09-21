package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// INFRA-BACKUP-01 tests: pruneArchiveDir bounds the destroy-time home
// archive directory. All tests seed fake *.tar.gz files (they are only
// ever stat'd, sorted and removed — verification happens in the destroy
// path, which is covered by the sibling destroy-flow test).

// seedArchive writes a fake archive with the real naming convention and a
// distinct mtime, returning its path.
func seedArchive(t *testing.T, dir, name string, size int64, modTime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return path
}

func listTarGz(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) != "" && filepath.Ext(e.Name()) == ".gz" {
			names = append(names, e.Name())
		}
	}
	return names
}

// managerForPrune returns an AgentManager sufficient for pruneArchiveDir,
// which only uses m.logger.
func managerForPrune(t *testing.T) *AgentManager {
	t.Helper()
	m, _, _ := newLifecycleManager(t)
	return m
}

// TestPruneArchiveDir_NewestOnly runs the keep-N scenarios: keep the newest
// N by mtime, 0 disables entirely, and negative behaves like 0 (disabled).
func TestPruneArchiveDir_NewestOnly(t *testing.T) {
	tests := []struct {
		name     string
		keep     int
		maxBytes int64
		seed     []string // oldest .. newest
		wantLeft []string // expected surviving set
	}{
		{
			name:     "keep newest 2 of 4",
			keep:     2,
			seed:     []string{"a-20260101T000000Z.tar.gz", "b-20260102T000000Z.tar.gz", "c-20260103T000000Z.tar.gz", "d-20260104T000000Z.tar.gz"},
			wantLeft: nil, // checked dynamically below
		},
		{
			name:     "keep 0 disables pruning",
			keep:     0,
			seed:     []string{"a-1.tar.gz", "b-2.tar.gz"},
			wantLeft: nil,
		},
		{
			name:     "negative keep disables pruning",
			keep:     -5,
			seed:     []string{"a-1.tar.gz", "b-2.tar.gz"},
			wantLeft: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			base := time.Now()
			for i, name := range tt.seed {
				seedArchive(t, dir, name, 10, base.Add(time.Duration(i)*time.Minute))
			}
			want, wantErr := tt.wantLeft, tt.wantLeft == nil
			_ = wantErr
			// wantLeft nil means "computed expectation": all files with
			// distinct names survive iff keep<=0; for keep=2 the newest two.
			var expect []string
			if tt.keep <= 0 {
				for _, name := range tt.seed {
					expect = append(expect, name)
				}
			} else {
				expected := tt.seed
				if len(expected) > tt.keep {
					expected = expected[len(expected)-tt.keep:]
				}
				expect = append(expect, expected...)
			}
			_ = want

			m := managerForPrune(t)
			removed, err := m.pruneArchiveDir(dir, tt.keep, tt.maxBytes)
			if err != nil {
				t.Fatalf("pruneArchiveDir() error = %v", err)
			}
			got := listTarGz(t, dir)
			if len(got) != len(expect) {
				t.Fatalf("after prune, files = %v, want %v", got, expect)
			}
			gotSet := map[string]bool{}
			for _, n := range got {
				gotSet[n] = true
			}
			for _, n := range expect {
				if !gotSet[n] {
					t.Errorf("expected survivor %q missing; on disk: %v", n, got)
				}
			}
			// removed names must be exactly the deleted ones
			removedSet := map[string]bool{}
			for _, n := range removed {
				removedSet[n] = true
			}
			for _, n := range tt.seed {
				wasRemoved := removedSet[n]
				stillThere := gotSet[n]
				if wasRemoved == stillThere {
					t.Errorf("file %q: removed=%v onDisk=%v (must be mutually exclusive)", n, wasRemoved, stillThere)
				}
			}
		})
	}
}

// TestPruneArchiveDir_KeepsNewestUnderMaxBytes: when the size cap binds,
// the oldest are removed first and the newest file always survives.
func TestPruneArchiveDir_KeepsNewestUnderMaxBytes(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	seedArchive(t, dir, "old1-1.tar.gz", 400, base.Add(-3*time.Minute))
	seedArchive(t, dir, "old2-2.tar.gz", 400, base.Add(-2*time.Minute))
	seedArchive(t, dir, "old3-3.tar.gz", 400, base.Add(-1*time.Minute))
	seedArchive(t, dir, "new-4.tar.gz", 400, base)
	// keep=0 (disabled); cap = 1000 bytes: 1600 > 1000 -> remove the two
	// oldest (old1 then old2) until the total (400+400) is under the cap;
	// new-4 and old3 survive.
	m := managerForPrune(t)
	removed, err := m.pruneArchiveDir(dir, 0, 1000)
	if err != nil {
		t.Fatalf("pruneArchiveDir() error = %v", err)
	}
	got := listTarGz(t, dir)
	if len(got) != 2 {
		t.Fatalf("after size-cap prune, files = %v (removed=%v), want exactly 2", got, removed)
	}
	if !hasElement(got, "new-4.tar.gz") {
		t.Errorf("newest archive deleted: %v", got)
	}
	// The two OLDEST must be the ones removed.
	for _, gone := range []string{"old1-1.tar.gz", "old2-2.tar.gz"} {
		if !hasElement(removed, gone) {
			t.Errorf("expected oldest-first removal missed %q (removed=%v)", gone, removed)
		}
	}
}

// TestPruneArchiveDir_SizeCapNeverDeletesNewestFile: a single archive
// larger than the cap must survive (never prune down to zero).
func TestPruneArchiveDir_SizeCapNeverDeletesNewestFile(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	seedArchive(t, dir, "old-1.tar.gz", 500, base.Add(-time.Minute))
	seedArchive(t, dir, "new-2.tar.gz", 1000, base) // 1000 > cap
	m := managerForPrune(t)
	removed, err := m.pruneArchiveDir(dir, 0, 300)
	if err != nil {
		t.Fatalf("pruneArchiveDir() error = %v", err)
	}
	if !hasElement(removed, "old-1.tar.gz") {
		t.Errorf("old archive not removed: %v", removed)
	}
	got := listTarGz(t, dir)
	if len(got) != 1 || got[0] != "new-2.tar.gz" {
		t.Fatalf("newest archive did not survive the cap: %v", got)
	}
}

// TestPruneArchiveDir_OnlyTarGzTouched: a stray non-archive file in the
// archive dir survives every pass.
func TestPruneArchiveDir_OnlyTarGzTouched(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	for i := 0; i < 5; i++ {
		seedArchive(t, dir, "x-"+string(rune('a'+i))+"-"+time.Now().UTC().Format("20060102T000000Z")+".tar.gz", 10, base.Add(time.Duration(i)*time.Minute))
	}
	stray := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(stray, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	strayMod := filepath.Join(filepath.Dir(stray), "backup.tar") // *.tar, not .tar.gz
	if err := os.WriteFile(strayMod, []byte("keep me too"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := managerForPrune(t)
	if _, err := m.pruneArchiveDir(dir, 2, 0); err != nil {
		t.Fatalf("pruneArchiveDir() error = %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray notes.txt did not survive pruning")
	}
	if _, err := os.Stat(strayMod); err != nil {
		t.Errorf("stray .tar (non-.gz) file did not survive pruning")
	}
}

// TestPruneArchiveDir_MissingDir: a nonexistent archive dir is a no-op,
// not an error.
func TestPruneArchiveDir_MissingDir(t *testing.T) {
	m := managerForPrune(t)
	removed, err := m.pruneArchiveDir(filepath.Join(t.TempDir(), "gone"), 3, 1000)
	if err != nil {
		t.Fatalf("pruneArchiveDir() on missing dir: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty", removed)
	}
}

// hasElement reports whether list contains s.

func hasElement(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
