package fsclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// docCommentOf returns the doc comment block immediately above the first line
// of the named file (this package's directory) that contains marker. It reads
// from disk, exactly as a reviewer would, so the pinned text is the text that
// ships — and because the claim lives on the function it describes, it can
// only drift if someone edits both.
func docCommentOf(t *testing.T, file, marker string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, line := range lines {
		if strings.Contains(line, marker) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("marker %q not found in %s", marker, file)
	}
	end := start
	for end > 0 && strings.HasPrefix(strings.TrimSpace(lines[end-1]), "//") {
		end--
	}
	if end == start {
		t.Fatalf("the line naming %q in %s carries no doc comment to pin", marker, file)
	}
	return strings.Join(lines[end:start+1], "\n")
}
