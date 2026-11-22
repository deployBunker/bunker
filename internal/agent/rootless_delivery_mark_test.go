package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// PERF-023 tests: the rootless-install delivery must be attributable per
// spawn from the daemon log alone — a mark carrying payload bytes, elapsed
// time, and source (network|cache|invalidated) — and the warm path must serve
// the payload from the GAP-091 host cache without a re-download. The marks
// follow the PERF-022 component-mark format (elapsed_ms integer fields), so
// the spawn's phase table stays uniform. House pattern throughout: injectable
// runners/caches via the package seams, a slog text handler into a buffer,
// no live network and no host state.

// newMarkTestLogger captures the daemon log lines a mark test asserts on.
func newMarkTestLogger() (*slog.Logger, *strings.Builder) {
	var buf strings.Builder
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// markField returns the value of key=value in a slog text-handler line, the
// same field-parsing shape the PERF-022 mark test uses for stage_gap_ms.
func markField(t *testing.T, line, key string) string {
	t.Helper()
	needle := key + "="
	i := strings.Index(line, needle)
	if i < 0 {
		t.Fatalf("mark line missing %s: %s", key, line)
	}
	val := line[i+len(needle):]
	if j := strings.Index(val, " "); j >= 0 {
		val = val[:j]
	}
	return val
}

// armDeliveryCache arms the host cache at a temp dir (through the production
// seam) and restores the previous value on cleanup.
func armDeliveryCache(t *testing.T) string {
	t.Helper()
	cacheDir := t.TempDir()
	prev := rootlessInstallerCacheDir
	SetRootlessInstallerCacheDir(cacheDir)
	t.Cleanup(func() { rootlessInstallerCacheDir = prev })
	return cacheDir
}

// stubDeliveryDownload replaces the uncached download seam with a counted fake
// that writes payload after delay (so a delivery's elapsed_ms is provably
// nonzero) and restores the seam on cleanup.
func stubDeliveryDownload(t *testing.T, payload []byte, delay time.Duration) *int {
	t.Helper()
	downloads := 0
	prevDownload := rootlessInstallerDownload
	rootlessInstallerDownload = func(ctx context.Context, path string) ([]byte, error) {
		downloads++
		if delay > 0 {
			time.Sleep(delay)
		}
		return nil, os.WriteFile(path, payload, 0o644)
	}
	t.Cleanup(func() { rootlessInstallerDownload = prevDownload })
	return &downloads
}

// deliveryMarks returns the log lines carrying the PERF-023 delivery mark.
func deliveryMarks(logs string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if strings.Contains(line, "rootless installer delivered") {
			out = append(out, line)
		}
	}
	return out
}

// TestRootlessDeliveryMark_MissThenHit covers the PERF-023 attribution
// contract end to end on the delivery helper: a cold miss is marked
// source=network with the payload byte count and a nonzero elapsed_ms, and a
// warm second call in-process is marked source=cache with the SAME byte count
// and no second fetch — the warm-cache hit path a re-spawn on the same daemon
// takes.
func TestRootlessDeliveryMark_MissThenHit(t *testing.T) {
	armDeliveryCache(t)
	stubRootlessInstallerVerifier(t)
	downloads := stubDeliveryDownload(t, uuValidInstaller, 40*time.Millisecond)
	logger, buf := newMarkTestLogger()

	dst1 := filepath.Join(t.TempDir(), "installer-1.sh")
	if err := deliverRootlessInstaller(context.Background(), dst1, "probe-a", logger); err != nil {
		t.Fatalf("first delivery (miss): %v", err)
	}
	dst2 := filepath.Join(t.TempDir(), "installer-2.sh")
	if err := deliverRootlessInstaller(context.Background(), dst2, "probe-a", logger); err != nil {
		t.Fatalf("second delivery (warm): %v", err)
	}

	marks := deliveryMarks(buf.String())
	if len(marks) != 2 {
		t.Fatalf("got %d delivery marks, want one per delivery:\n%s", len(marks), buf.String())
	}

	// Miss: source=network, the delivered byte count, nonzero elapsed.
	wantBytes := len(uuValidInstaller)
	if got := markField(t, marks[0], "source"); got != rootlessSourceNetwork {
		t.Errorf("cold miss marked source=%q, want %q", got, rootlessSourceNetwork)
	}
	if got := markField(t, marks[0], "bytes"); got != strconv.Itoa(wantBytes) {
		t.Errorf("cold miss marked bytes=%s, want %d", got, wantBytes)
	}
	if markField(t, marks[0], "elapsed_ms") == "0" {
		t.Errorf("cold miss elapsed_ms=0 despite a 40ms download, line: %s", marks[0])
	}

	// Warm hit: source=cache, SAME byte count, still exactly one fetch.
	if got := markField(t, marks[1], "source"); got != rootlessSourceCache {
		t.Errorf("warm second delivery marked source=%q, want %q", got, rootlessSourceCache)
	}
	if got := markField(t, marks[1], "bytes"); got != itoa(wantBytes) {
		t.Errorf("warm second delivery marked bytes=%s, want the same %d", got, wantBytes)
	}
	if *downloads != 1 {
		t.Fatalf("warm second delivery reached the network (%d downloads), want 1 total", *downloads)
	}
}

// TestRootlessDeliveryMark_InvalidatedEntryDownloadsAndReRecords covers the
// invalidated path: a cached entry that fails validation (a partial or
// poisoned artifact) is removed, a fresh download happens, and the mark
// RE-RECORDS the delivery as source=invalidated — not as a plain network
// miss, so the log distinguishes "first fetch" from "replaced a discarded
// entry". The cache is re-populated with the fresh bytes: the next delivery
// is a hit again.
func TestRootlessDeliveryMark_InvalidatedEntryDownloadsAndReRecords(t *testing.T) {
	cacheDir := armDeliveryCache(t)
	stubRootlessInstallerVerifier(t)
	downloads := stubDeliveryDownload(t, uuValidInstaller, 0)
	logger, buf := newMarkTestLogger()

	// Seed a poisoned entry under the CURRENT key: big enough to pass the
	// size check, wrong shape to pass the shebang check.
	poisoned := []byte("not-a-shebang")
	if err := os.WriteFile(filepath.Join(cacheDir, rootlessInstallerCacheKey()), poisoned, 0o644); err != nil {
		t.Fatalf("seed poisoned cache: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "installer.sh")
	if err := deliverRootlessInstaller(context.Background(), dst, "probe-b", logger); err != nil {
		t.Fatalf("delivery over invalidated entry: %v", err)
	}

	if *downloads != 1 {
		t.Fatalf("invalidated entry must still download exactly once, got %d", *downloads)
	}
	marks := deliveryMarks(buf.String())
	if len(marks) != 1 {
		t.Fatalf("got %d delivery marks, want 1:\n%s", len(marks), buf.String())
	}
	if got := markField(t, marks[0], "source"); got != rootlessSourceInvalidated {
		t.Errorf("invalidated delivery marked source=%q, want %q", got, rootlessSourceInvalidated)
	}
	if got := markField(t, marks[0], "bytes"); got != strconv.Itoa(len(uuValidInstaller)) {
		t.Errorf("invalidated delivery marked bytes=%s, want %d", got, len(uuValidInstaller))
	}

	// Re-recorded: the fresh bytes replaced the discarded entry, so the next
	// delivery is served from cache without a fetch.
	if _, err := os.Stat(filepath.Join(cacheDir, rootlessInstallerCacheKey())); err != nil {
		t.Fatalf("cache was not re-populated after invalidation: %v", err)
	}
	dst2 := filepath.Join(t.TempDir(), "installer-2.sh")
	if err := deliverRootlessInstaller(context.Background(), dst2, "probe-b", logger); err != nil {
		t.Fatalf("post-invalidation delivery: %v", err)
	}
	if *downloads != 1 {
		t.Fatalf("post-invalidation delivery re-fetched (%d downloads), want the cache to serve it", *downloads)
	}
	marks = deliveryMarks(buf.String())
	if got := markField(t, marks[len(marks)-1], "source"); got != rootlessSourceCache {
		t.Errorf("post-invalidation delivery marked source=%q, want %q", got, rootlessSourceCache)
	}
}

// TestRootlessDeliveryMark_StalePinKeyDownloadsAsNetwork covers the pin
// mismatch half of invalidation: the cache key carries the supply-chain pin's
// digest prefix (GAP-135), so an artifact left by a PREVIOUS pin lives under a
// different filename and the current key is a MISS by construction — the
// download happens and the mark records it as source=network. The stale entry
// is never served.
func TestRootlessDeliveryMark_StalePinKeyDownloadsAsNetwork(t *testing.T) {
	cacheDir := armDeliveryCache(t)
	stubRootlessInstallerVerifier(t)
	downloads := stubDeliveryDownload(t, uuValidInstaller, 0)
	logger, buf := newMarkTestLogger()

	// An entry from a previous pin: same URL family, different pin tag.
	staleKey := "rootless-installer-" + hostCacheSuffix(rootlessInstallURL) + "-deadbeefcafe.sh"
	if err := os.WriteFile(filepath.Join(cacheDir, staleKey), uuValidInstaller, 0o644); err != nil {
		t.Fatalf("seed stale-pin cache: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "installer.sh")
	if err := deliverRootlessInstaller(context.Background(), dst, "probe-c", logger); err != nil {
		t.Fatalf("delivery after a re-pin: %v", err)
	}

	if *downloads != 1 {
		t.Fatalf("a re-pin must miss the stale entry and download exactly once, got %d", *downloads)
	}
	marks := deliveryMarks(buf.String())
	if len(marks) != 1 {
		t.Fatalf("got %d delivery marks, want 1:\n%s", len(marks), buf.String())
	}
	if got := markField(t, marks[0], "source"); got != rootlessSourceNetwork {
		t.Errorf("post-re-pin delivery marked source=%q, want %q (a stale pin is a miss, not an invalidation)", got, rootlessSourceNetwork)
	}
}

// TestRootlessDeliveryMark_LegacyUncachedPath covers the cache-disabled host:
// deliverRootlessInstaller falls back to the legacy uncached download seam and
// the mark still attributes the delivery (source=network, real payload bytes)
// — a host that never configured the cache keeps its behavior AND its
// attribution.
func TestRootlessDeliveryMark_LegacyUncachedPath(t *testing.T) {
	prev := rootlessInstallerCacheDir
	SetRootlessInstallerCacheDir("")
	t.Cleanup(func() { rootlessInstallerCacheDir = prev })
	downloads := stubDeliveryDownload(t, uuValidInstaller, 0)
	logger, buf := newMarkTestLogger()

	dst := filepath.Join(t.TempDir(), "installer.sh")
	if err := deliverRootlessInstaller(context.Background(), dst, "probe-d", logger); err != nil {
		t.Fatalf("legacy-path delivery: %v", err)
	}

	if *downloads != 1 {
		t.Fatalf("cache-disabled delivery must use the legacy seam exactly once, got %d", *downloads)
	}
	marks := deliveryMarks(buf.String())
	if len(marks) != 1 {
		t.Fatalf("got %d delivery marks, want 1:\n%s", len(marks), buf.String())
	}
	if got := markField(t, marks[0], "source"); got != rootlessSourceNetwork {
		t.Errorf("legacy-path delivery marked source=%q, want %q", got, rootlessSourceNetwork)
	}
	if got := markField(t, marks[0], "bytes"); got != strconv.Itoa(len(uuValidInstaller)) {
		t.Errorf("legacy-path delivery marked bytes=%s, want the DELIVERED file size %d (not curl's output bytes)", got, len(uuValidInstaller))
	}
}

// TestInstallRootlessDocker_EmitsDeliveryAndExecutionMarks proves WIRING: the
// full production install path (every seam faked, as in the userunit host)
// emits BOTH PERF-023 marks into the daemon log — the delivery mark with
// bytes+elapsed+source, then the execution mark whose elapsed bounds the
// installer-internal ~82MB tarball fetch that the Go path cannot attribute
// byte-wise (the pinned script curls docker.tgz and rootless.tgz itself).
func TestInstallRootlessDocker_EmitsDeliveryAndExecutionMarks(t *testing.T) {
	h := newUserUnitHost(t)
	h.cacheDir = t.TempDir()
	home := h.userHome(t)
	h.install(t, t.TempDir())

	// Pre-seed a valid cache entry so the delivery is a warm hit.
	payload := uuValidInstaller
	if err := os.WriteFile(filepath.Join(h.cacheDir, rootlessInstallerCacheKey()), payload, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	logger, buf := newMarkTestLogger()
	if err := installRootlessDocker(context.Background(), h.username, home, logger); err != nil {
		t.Fatalf("installRootlessDocker failed: %v%s", err, h.callLog())
	}

	logs := buf.String()
	marks := deliveryMarks(logs)
	if len(marks) != 1 {
		t.Fatalf("install path emitted %d delivery marks, want 1:\n%s", len(marks), logs)
	}
	if got := markField(t, marks[0], "source"); got != rootlessSourceCache {
		t.Errorf("warm install delivery marked source=%q, want %q", got, rootlessSourceCache)
	}
	if got := markField(t, marks[0], "bytes"); got != strconv.Itoa(len(payload)) {
		t.Errorf("warm install delivery marked bytes=%s, want %d", got, len(payload))
	}
	if markField(t, marks[0], "elapsed_ms") == "" {
		t.Errorf("delivery mark missing elapsed_ms: %s", marks[0])
	}

	execIdx := strings.Index(logs, "rootless installer executed")
	if execIdx < 0 {
		t.Fatalf("install path emitted no execution mark (the ~82MB installer-internal fetch would be unattributed):\n%s", logs)
	}
	if !strings.Contains(logs[execIdx:], "elapsed_ms=") {
		t.Errorf("execution mark missing elapsed_ms: %s", logs[execIdx:])
	}
	if !strings.Contains(logs[execIdx:], "note=") {
		t.Errorf("execution mark missing the not-byte-attributable note: %s", logs[execIdx:])
	}
	if strings.Index(logs, "rootless installer delivered") > execIdx {
		t.Errorf("delivery mark must precede the execution mark:\n%s", logs)
	}
}
