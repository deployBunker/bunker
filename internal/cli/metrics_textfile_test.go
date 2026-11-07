package cli

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMetricsTextfileSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const scraped = "# HELP bunker_uptime_seconds uptime\nbunker_uptime_seconds 12.5\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, scraped)
	}))
	defer server.Close()

	dir := t.TempDir()
	destination := filepath.Join(dir, "bunkerd.prom")
	cmd := NewMetricsCommand()
	cmd.SetArgs([]string{"--prometheus-textfile", destination, "--metrics-url", server.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	content := string(got)
	if !strings.Contains(content, "bunker_uptime_seconds 12.5") {
		t.Fatalf("scraped sample missing from output:\n%s", content)
	}
	if !strings.HasPrefix(content, strings.TrimRight(scraped, "\n")+"\n") {
		t.Fatalf("scraped body was not preserved with exactly one trailing newline:\n%q", content)
	}
	const stampPrefix = "bunker_metrics_last_success_timestamp_seconds "
	stampLine := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, stampPrefix) {
			stampLine = line
		}
	}
	if stampLine == "" {
		t.Fatalf("freshness sample missing from output:\n%s", content)
	}
	var timestamp int64
	if _, err := fmt.Sscanf(strings.TrimPrefix(stampLine, stampPrefix), "%d", &timestamp); err != nil {
		t.Fatalf("parse timestamp %q: %v", stampLine, err)
	}
	delta := time.Now().Unix() - timestamp
	if delta < 0 || delta > 3 {
		t.Fatalf("freshness timestamp %d is not within a few seconds of now (%d seconds old)", timestamp, delta)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read destination directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "bunkerd.prom" {
		t.Fatalf("temporary artifacts remain after success: %v", entries)
	}
}

func TestMetricsTextfileFailurePreservesDestinationAndCleansTemp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	dir := t.TempDir()
	destination := filepath.Join(dir, "bunkerd.prom")
	previous := []byte("previous metrics\n")
	if err := os.WriteFile(destination, previous, 0644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory before: %v", err)
	}
	beforeNames := entryNames(before)

	var stderr bytes.Buffer
	cmd := NewMetricsCommand()
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--prometheus-textfile", destination, "--metrics-url", server.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatal("Execute succeeded for an HTTP 500 response")
	}
	if strings.TrimSpace(stderr.String()) == "" {
		t.Fatal("expected a one-line diagnostic on stderr")
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read destination after failure: %v", err)
	}
	if !bytes.Equal(got, previous) {
		t.Fatalf("destination changed after failed scrape: got %q, want %q", got, previous)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory after: %v", err)
	}
	if gotNames := entryNames(after); !equalStrings(gotNames, beforeNames) {
		t.Fatalf("directory entries changed after failure: before %v, after %v", beforeNames, gotNames)
	}
}

func TestMetricsTextfileEmptyBodyRejected(t *testing.T) {
	assertRejectedScrapePreservesDestination(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 with a completely empty body: exactly the shape a
		// misrouted --metrics-url can produce. Must be refused.
		w.WriteHeader(http.StatusOK)
	}))
}

func TestMetricsTextfileHTMLBodyRejected(t *testing.T) {
	assertRejectedScrapePreservesDestination(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTML error page from some other service. The bunker_ mention
		// sits MID-LINE on purpose: the guard must be line-anchored
		// (^bunker_), not a substring search.
		const html = "<!doctype html>\n<html><head><title>502 Bad Gateway</title></head>\n<body><pre>bunker_uptime_seconds 12.5 (raw)</pre></body></html>\n"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, html)
	}))
}

// assertRejectedScrapePreservesDestination seeds a real destination file,
// scrapes from the given handler, and asserts the publish is refused: the
// command exits non-zero with a stderr diagnostic and the destination stays
// byte-identical (sha256 before == after), with no temp artifacts left.
func assertRejectedScrapePreservesDestination(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()

	dir := t.TempDir()
	destination := filepath.Join(dir, "bunkerd.prom")
	previous := []byte("# HELP bunker_uptime_seconds uptime\nbunker_uptime_seconds 12.5\n")
	if err := os.WriteFile(destination, previous, 0644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	before := sha256.Sum256(previous)
	beforeNames := entryNames(mustReadDir(t, dir))

	var stderr bytes.Buffer
	cmd := NewMetricsCommand()
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--prometheus-textfile", destination, "--metrics-url", server.URL})
	if err := cmd.Execute(); err == nil {
		t.Fatal("Execute succeeded for a non-bunkerd HTTP 200 body")
	}
	if strings.TrimSpace(stderr.String()) == "" {
		t.Fatal("expected a refusal diagnostic on stderr")
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read destination after refusal: %v", err)
	}
	after := sha256.Sum256(got)
	if before != after {
		t.Fatalf("destination changed after refused scrape: sha256 %x -> %x (%q)", before, after, got)
	}
	afterNames := entryNames(mustReadDir(t, dir))
	if !equalStrings(afterNames, beforeNames) {
		t.Fatalf("directory entries changed after refusal: before %v, after %v", beforeNames, afterNames)
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	return entries
}

func TestMetricsTextfileFlagsAreCommandLocal(t *testing.T) {
	cmd := NewMetricsCommand()
	for _, name := range []string{"prometheus-textfile", "metrics-url"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing metrics command flag --%s", name)
		}
		if cmd.PersistentFlags().Lookup(name) != nil {
			t.Errorf("--%s must not be persistent", name)
		}
	}
	if got := cmd.Flags().Lookup("metrics-url").DefValue; got != "http://127.0.0.1:8080/metrics" {
		t.Errorf("metrics-url default = %q", got)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
