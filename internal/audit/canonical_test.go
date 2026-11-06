package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, count int, summary string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if err := l.Log(Record{TS: "2026-01-01T00:00:00Z", Caller: "test", Method: "/m", Outcome: "ok", Summary: summary}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureLines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	lines := make([][]byte, len(parts))
	for i := range parts {
		lines[i] = []byte(parts[i])
	}
	return lines
}

func setFixtureLines(t *testing.T, path string, lines [][]byte) {
	t.Helper()
	data := []byte(strings.Join(func() []string {
		out := make([]string, len(lines))
		for i := range lines {
			out[i] = string(lines[i])
		}
		return out
	}(), "\n") + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fallbackCanonical(t *testing.T, line []byte) []byte {
	t.Helper()
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatal(err)
	}
	rec.Hash = ""
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCanonicalBytesEquivalentToWriterOutput(t *testing.T) {
	path := writeFixture(t, 4, "equivalence")
	lines := fixtureLines(t, path)
	mismatches, declines := 0, 0
	for _, line := range lines {
		_, _, got, ok, _ := canonicalBytesFromLine(line, nil)
		if !ok {
			declines++
			continue
		}
		if string(got) != string(fallbackCanonical(t, line)) {
			mismatches++
		}
	}
	if mismatches != 0 || declines != 0 {
		t.Fatalf("clean writer fixture: mismatches=%d declines=%d", mismatches, declines)
	}
}

func TestFastPathVerdictParity(t *testing.T) {
	basePath := writeFixture(t, 3, "ordinary")
	base := fixtureLines(t, basePath)
	fixtures := map[string][][]byte{}
	clone := func() [][]byte {
		out := make([][]byte, len(base))
		for i := range base {
			out[i] = append([]byte(nil), base[i]...)
		}
		return out
	}
	fixtures["clean"] = clone()
	lines := clone()
	lines[1] = bytesReplaceFirst(lines[1], []byte("ordinary"), []byte("tampered"))
	fixtures["value flip"] = lines
	lines = clone()
	var wrong Record
	if err := json.Unmarshal(lines[1], &wrong); err != nil {
		t.Fatal(err)
	}
	wrong.PrevHash = strings.Repeat("a", 64)
	lines[1] = rehashRecord(t, wrong)
	fixtures["rehashed wrong prev"] = lines
	lines = clone()
	lines = lines[:2]
	fixtures["truncated anchored tail"] = lines
	lines = clone()
	lines[1] = []byte("not-json")
	fixtures["garbage"] = lines
	lines = clone()
	lines[1] = bytesReplaceFirst(lines[1], []byte(`,"caller"`), []byte(`, "caller"`))
	fixtures["reformatted no rehash"] = lines
	fixtures["hash literal in summary"] = fixtureLines(t, writeFixture(t, 1, `literal "hash":" text`))

	for name, lines := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "case.log")
			setFixtureLines(t, path, lines)
			anchor, err := os.ReadFile(basePath + ".anchor")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".anchor", anchor, 0o600); err != nil {
				t.Fatal(err)
			}
			canonicalFastPath = true
			fast := runFileVerdict(t, path)
			canonicalFastPath = false
			fallback := runFileVerdict(t, path)
			canonicalFastPath = true
			if fast != fallback {
				t.Fatalf("fast=%q fallback=%q", fast, fallback)
			}
		})
	}
}

func TestReformattedAndRehashedRawLineIsAcceptedByFastPath(t *testing.T) {
	line := fixtureLines(t, writeFixture(t, 1, "reformatted"))[0]
	line = bytesReplaceFirst(line, []byte(`,"caller"`), []byte(`, "caller"`))
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatal(err)
	}
	line = rehashRawLine(t, line)
	path := filepath.Join(t.TempDir(), "rehash.log")
	setFixtureLines(t, path, [][]byte{line})
	canonicalFastPath = true
	fast := runFileVerdict(t, path)
	canonicalFastPath = false
	fallback := runFileVerdict(t, path)
	canonicalFastPath = true
	if fast == fallback || !strings.Contains(fast, "bad=0") || !strings.Contains(fallback, "bad=1") {
		t.Fatalf("expected designed divergence; fast=%s fallback=%s", fast, fallback)
	}
}

func runFileVerdict(t *testing.T, path string) string {
	t.Helper()
	n, bad, tail, err := verifyFile(path, "", false)
	message := ""
	if err != nil {
		message = err.Error()
	}
	result, detailErr := VerifyDetailed(path, "")
	detailMessage := ""
	if detailErr != nil {
		detailMessage = detailErr.Error()
	}
	return fmt.Sprintf("records=%d bad=%d tail=%s err=%s detailed=%d/%d/%s", n, bad, tail, message, result.Records, result.FirstBad, detailMessage)
}

func rehashRawLine(t *testing.T, line []byte) []byte {
	t.Helper()
	marker := []byte(`"hash":"`)
	index := strings.Index(string(line), string(marker))
	if index < 0 {
		t.Fatal("hash token not found")
	}
	start := index + len(marker)
	canonical := make([]byte, 0, len(line)-64)
	canonical = append(canonical, line[:start]...)
	canonical = append(canonical, line[start+64:]...)
	sum := sha256.Sum256(canonical)
	rehashed := append([]byte(nil), line...)
	copy(rehashed[start:start+64], hex.EncodeToString(sum[:]))
	return rehashed
}

func rehashRecord(t *testing.T, rec Record) []byte {
	t.Helper()
	rec.Hash = ""
	canonical, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	rec.Hash = hex.EncodeToString(sum[:])
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func bytesReplaceFirst(src, old, replacement []byte) []byte {
	return []byte(strings.Replace(string(src), string(old), string(replacement), 1))
}

func benchmarkVerifyFile(b *testing.B, fast bool) {
	path := filepath.Join(b.TempDir(), "bench.log")
	l, err := New(path)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 47_000; i++ {
		if err := l.Log(Record{TS: "2026-01-01T00:00:00Z", Caller: "bench", Method: "/bunker.v1.Bunkerd/SpawnAgent", RemoteAddr: "192.0.2.10:54321", AgentID: "agent-00000001", SessionID: "session-00000001", DurationMS: int64(i), Outcome: "ok", Summary: "retention ceiling verification benchmark fixture"}); err != nil {
			b.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		b.Fatal(err)
	}
	canonicalFastPath = fast
	defer func() { canonicalFastPath = true }()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := Verify(path); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerifyFileFastPath(b *testing.B) {
	benchmarkVerifyFile(b, true)
}

func BenchmarkVerifyFileParseMarshal(b *testing.B) {
	benchmarkVerifyFile(b, false)
}
