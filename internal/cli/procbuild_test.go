package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// GAP-090 load hygiene: the real-process tests in this package used to compile
// `go build ./cmd/bunker` once PER TEST — four newProcTestHarness callers plus
// two tunnel reaping tests — so one `go test ./internal/cli` run paid for six
// full builds (measured: 646 child-process spawns per package run, the worst
// host-load offender in the suite). The binary is identical every time, so the
// build now happens ONCE per test process in TestMain and every consumer links
// the same shared binary. Assertions are unchanged: each test still drives the
// real binary, real main().
//
// The build's internal parallelism is bounded so a single package cannot eat
// the whole box (fleet directive 2026-09-19): BUNKER_TEST_CLI_BUILD_CONCURRENCY
// feeds `go build -p`, clamped to [2, 16], default 8.
//
// INT-CI-023 (worktree cache isolation): the cache was keyed on the uid ALONE
// — $TMPDIR/bunker-cli-build-cache-uid1000/bunker — so every worktree of this
// repo on one host built into and exec'd the SAME file. flock serialises BUILD
// starts; it cannot stop last-writer-wins on the artifact. Live incident
// (DF-BUNKER-28, 2026-09-20): the main checkout's rebuild overwrote the cache
// mid-guard and TestTunnelCommand_SIGKILLReapsSSHChild exec'd a binary built
// from a different branch whose tunnel refuses to start — the 15s fixture
// timed out with no `--- FAIL` line naming the real cause, and the polluted
// binary carried an error string that existed nowhere in the worker's tree.
//
// The key is therefore derived from the CHECKOUT (its git worktree root), not
// from the uid: see testCLICacheKey / sharedCLIBinaryPath. Two worktrees of
// this repo can no longer share a binary, while everything the GAP-090 shape
// guarantees is preserved — one build per test process, one shared binary per
// session, a stable path that repeated runs inside one checkout REUSE (it is
// still a cache, not a per-run or per-PID directory), and the same flock for
// concurrent runs inside one worktree.
//
// QA-BUNKER-001 (multi-tenant host EACCES): the cache ROOT above the checkout
// key was one fixed shared name, so whichever uid ran the suite first owned it
// 0700 and every later uid failed in TestMain with "create build cache dir:
// ... permission denied" — no cross-uid sharing existed below it, so the fix
// embeds the uid in the ROOT name (testCLICacheRoot) while the checkout key,
// sweep, legacy purge, flock, and marker/staleness behavior are unchanged;
// the sweep simply walks this uid's root.
const (
	// minTestCLIBuildConcurrency keeps a lone build from stalling on a
	// mistyped "0"/"1" knob value.
	minTestCLIBuildConcurrency = 2
	// maxTestCLIBuildConcurrency is the ceiling: above this the build is the
	// box's dominant load for its whole duration.
	maxTestCLIBuildConcurrency = 16
	// defaultTestCLIBuildConcurrency leaves headroom for sibling packages of
	// a concurrent `go test ./...` on the same host.
	defaultTestCLIBuildConcurrency = 8

	// cliBuildConcurrencyEnv is the knob. Unset/blank/garbage → the default.
	cliBuildConcurrencyEnv = "BUNKER_TEST_CLI_BUILD_CONCURRENCY"

	// cliBuildCacheRootName is the cache ROOT name under the OS temp dir;
	// the uid is appended to it (testCLICacheRoot) so each uid gets its own
	// root (QA-BUNKER-001). One checkout's cache dir sits one level below
	// that root, so the root (not the temp dir itself) is what the
	// housekeeping sweep walks.
	cliBuildCacheRootName = "bunker-cli-build-cache-roots"

	// cliBuildCacheKeyEnv pins the cache key explicitly: CI, or a checkout
	// that is not a git work tree. An unusable value — blank, ".", ".." or
	// anything carrying a path separator — is ignored in favour of the
	// derived key, so a typo cannot relocate the cache outside the root nor
	// make two checkouts collide silently.
	cliBuildCacheKeyEnv = "BUNKER_TEST_CLI_CACHE_KEY"

	// cliBuildCacheBinaryName is the shared test CLI's file name inside a
	// cache dir (unchanged from the pre-fix layout).
	cliBuildCacheBinaryName = "bunker"

	// cliBuildCacheLockName is the flock file serialising concurrent builds
	// INSIDE one checkout's cache dir.
	cliBuildCacheLockName = "build.lock"

	// cliBuildCacheMarkerName records which checkout owns a cache dir. Its
	// mtime is the staleness clock the sweeper reads.
	cliBuildCacheMarkerName = "checkout"

	// cliBuildCacheRetention is how long an untouched cache dir survives
	// before the sweeper reaps it: a checkout that has not run this package
	// for a day has gone away (wave worktrees are deleted with their branch),
	// so its binary is no longer anyone's cache.
	cliBuildCacheRetention = 24 * time.Hour

	// cliBuildCacheKeyEntropy is the number of leading hex chars of the
	// checkout-root hash used in the key: 12 hex chars = 48 bits, far past
	// collision range for the handful of worktrees sharing one host, while
	// keeping `ls $TMPDIR` readable.
	cliBuildCacheKeyEntropy = 12

	// legacyCLIBuildCachePrefix is the pre-INT-CI-023 cache dir name
	// ("bunker-cli-build-cache-uid1000", keyed on the uid alone). Nothing
	// builds there any more; it is retained so the suite can prove the derived
	// key is not it, and so the sweeper can retire a leftover dir once stale.
	legacyCLIBuildCachePrefix = "bunker-cli-build-cache-uid"
)

// clampTestCLIBuildConcurrency parses the knob's raw value: unset, blank or
// non-numeric falls back to def; anything outside the clamp is pinned to the
// nearer bound.
func clampTestCLIBuildConcurrency(raw string, def int) int {
	v := strings.TrimSpace(raw)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < minTestCLIBuildConcurrency {
		return minTestCLIBuildConcurrency
	}
	if n > maxTestCLIBuildConcurrency {
		return maxTestCLIBuildConcurrency
	}
	return n
}

// testCLIBuildConcurrency is the clamped knob value for this process.
func testCLIBuildConcurrency() int {
	return clampTestCLIBuildConcurrency(os.Getenv(cliBuildConcurrencyEnv), defaultTestCLIBuildConcurrency)
}

// cacheKeySegment reduces s to one readable path element. Cache keys are read
// by people looking at `ls $TMPDIR`, so the checkout's directory name is kept,
// minus anything that could confuse a path or a shell.
func cacheKeySegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "tree"
	}
	return out
}

// sanitizeTestCLICacheKey validates an explicitly supplied key: exactly one
// path element, never a traversal segment and never a nested path. Anything
// unusable returns "" so the caller falls back to the derived key.
func sanitizeTestCLICacheKey(raw string) string {
	k := strings.TrimSpace(raw)
	if k == "" || k == "." || k == ".." {
		return ""
	}
	if strings.ContainsAny(k, `/\`) {
		return ""
	}
	return k
}

// testCLICacheKey derives the cache key for ONE checkout: a readable prefix
// from the checkout directory's name plus a hash of its absolute root path.
// Two worktrees of this repo on the same host — the wave case that broke
// DF-BUNKER-28 — get different keys; the same checkout always gets the same
// key, so the artifact stays a cache (reused run after run) instead of a
// per-run build.
func testCLICacheKey(worktreeRoot string) string {
	root := filepath.Clean(worktreeRoot)
	sum := sha256.Sum256([]byte(root))
	hash := hex.EncodeToString(sum[:])[:cliBuildCacheKeyEntropy]
	return cacheKeySegment(filepath.Base(root)) + "-" + hash
}

// testCLICacheKeyForRoot applies the explicit-key override (if usable) over
// the derived key.
func testCLICacheKeyForRoot(worktreeRoot, explicit string) string {
	if k := sanitizeTestCLICacheKey(explicit); k != "" {
		return k
	}
	return testCLICacheKey(worktreeRoot)
}

// testCLICacheRoot is the per-UID cache root under tempDir
// (QA-BUNKER-001): the uid is embedded in the ROOT name so each uid on a
// multi-tenant host owns its own 0700 root. The shared single-name root let
// whichever uid ran the suite first own it 0700 and every later uid die with
// EACCES in TestMain before any test ran. The per-CHECKOUT key sits below
// this root and is unchanged (INT-CI-023).
func testCLICacheRoot(tempDir string) string {
	return filepath.Join(tempDir, fmt.Sprintf("%s-%d", cliBuildCacheRootName, os.Getuid()))
}

// testCLICacheDir is one checkout's cache dir:
// <temp>/bunker-cli-build-cache-roots-<uid>/<key>.
func testCLICacheDir(tempDir, key string) string {
	return filepath.Join(testCLICacheRoot(tempDir), key)
}

// testCLICacheBinaryPath is the shared test CLI for one checkout:
// <temp>/bunker-cli-build-cache-roots-<uid>/<checkout-key>/bunker.
func testCLICacheBinaryPath(tempDir, key string) string {
	return filepath.Join(testCLICacheDir(tempDir, key), cliBuildCacheBinaryName)
}

// cliCacheMarkerPath is the ownership marker inside a cache dir.
func cliCacheMarkerPath(dir string) string {
	return filepath.Join(dir, cliBuildCacheMarkerName)
}

// legacyUIDCacheDir is the PRE-FIX cache dir: keyed on the uid alone, shared
// by every worktree of this repo on the host. Do not build into it.
func legacyUIDCacheDir(tempDir string, uid int) string {
	return filepath.Join(tempDir, fmt.Sprintf("%s%d", legacyCLIBuildCachePrefix, uid))
}

// legacyUIDCacheBinaryPath is the PRE-FIX shared binary path. Nothing writes
// here any more (INT-CI-023); it exists so the suite can assert the derived key
// is not the uid-only key, and it is what a stale-cache purge targets.
func legacyUIDCacheBinaryPath() string {
	return filepath.Join(legacyUIDCacheDir(os.TempDir(), os.Getuid()), cliBuildCacheBinaryName)
}

// testCLICheckout identifies the checkout this test process is running in —
// the thing the cache is keyed on.
type testCLICheckout struct {
	// root is the checkout root: the git worktree root when git can name it
	// (inside a wave worktree this is the WORKTREE, not the main clone), else
	// the module root derived from the working directory.
	root string
	// moduleRoot is where `go build ./cmd/bunker` must run. This repo has a
	// single module at the checkout root; a nested module would need its own
	// resolution here.
	moduleRoot string
	// key is testCLICacheKeyForRoot(root, $BUNKER_TEST_CLI_CACHE_KEY).
	key string
}

// newTestCLICheckout builds a checkout record for an already-resolved root.
func newTestCLICheckout(root string) testCLICheckout {
	root = filepath.Clean(root)
	return testCLICheckout{root: root, moduleRoot: root, key: testCLICacheKey(root)}
}

// gitToplevel asks git for the worktree root, which is authoritative when
// available: it names THIS checkout regardless of the process's cwd.
func gitToplevel() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveTestCLICheckout identifies the running checkout. A non-git checkout
// (an exported tarball, a `go test -c` binary run elsewhere) falls back to the
// module root above the package dir rather than failing: a test harness must
// degrade, not explode.
func resolveTestCLICheckout(getenv func(string) string, getwd func() (string, error)) testCLICheckout {
	var co testCLICheckout
	if root := gitToplevel(); root != "" {
		co = newTestCLICheckout(root)
	} else {
		// The package dir is internal/cli, so the module root is two levels up.
		wd, err := getwd()
		if err != nil {
			wd = getenv("PWD")
		}
		co = newTestCLICheckout(filepath.Join(wd, "..", ".."))
	}
	co.key = testCLICacheKeyForRoot(co.root, getenv(cliBuildCacheKeyEnv))
	return co
}

var (
	// cliCheckoutOnce memoises the resolution: it shells out to git, and the
	// answer cannot change inside one test process.
	cliCheckoutOnce sync.Once
	cliCheckout     testCLICheckout
)

// testCLICheckoutInfo returns this process's checkout record.
func testCLICheckoutInfo() testCLICheckout {
	cliCheckoutOnce.Do(func() {
		cliCheckout = resolveTestCLICheckout(os.Getenv, os.Getwd)
	})
	return cliCheckout
}

// sharedCLIBinaryPath is where the shared test CLI lands: a per-CHECKOUT cache
// dir under this uid's cache root in the OS temp dir — outside the repo and outside any t.TempDir (which
// the harness's HOME override must never relocate; see the HOME notes in the
// tunnel tests) — and keyed on the checkout so no other worktree of this repo
// can build over it.
func sharedCLIBinaryPath() string {
	return testCLICacheBinaryPath(os.TempDir(), testCLICheckoutInfo().key)
}

// writeCLICacheMarker records the owning checkout inside dir. Best-effort: a
// read-only temp dir must not fail the build, and a missing marker only makes
// the dir look stale to a LATER process — the sweeper always keeps its own key.
func writeCLICacheMarker(dir, root string) {
	_ = os.WriteFile(cliCacheMarkerPath(dir), []byte(root+"\n"), 0o600)
}

// testCLICacheStale reports whether a cache dir's marker is old enough to reap.
// An absent or unreadable marker yields the zero time and counts as stale.
func testCLICacheStale(markerMod time.Time, now time.Time, retention time.Duration) bool {
	if markerMod.IsZero() {
		return true
	}
	return now.Sub(markerMod) > retention
}

// sweepStaleCLICaches best-effort removes cache dirs whose marker has gone
// stale, keeping ownKey unconditionally (never reap the cache this process is
// building into). It returns the keys it removed.
//
// Housekeeping, not a contract: every failure is swallowed, because reaping
// must never fail a test run, and the number of dirs it can leak is bounded by
// the number of live checkouts.
func sweepStaleCLICaches(rootDir, ownKey string, now time.Time) []string {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ownKey {
			continue
		}
		var mod time.Time
		if fi, err := os.Stat(cliCacheMarkerPath(filepath.Join(rootDir, e.Name()))); err == nil {
			mod = fi.ModTime()
		}
		if !testCLICacheStale(mod, now, cliBuildCacheRetention) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(rootDir, e.Name())); err != nil {
			continue
		}
		removed = append(removed, e.Name())
	}
	return removed
}

// purgeLegacyUIDCacheDir retires this uid's pre-INT-CI-023 cache dir — the one
// every worktree used to share — once its binary has gone stale. A live pre-fix
// test run rebuilds that binary first, so a FRESH one means somebody is still
// using it and it is left alone. Best-effort, non-fatal.
func purgeLegacyUIDCacheDir(tempDir string, uid int, now time.Time) bool {
	dir := legacyUIDCacheDir(tempDir, uid)
	fi, err := os.Stat(filepath.Join(dir, cliBuildCacheBinaryName))
	if err != nil {
		return false
	}
	if !testCLICacheStale(fi.ModTime(), now, cliBuildCacheRetention) {
		return false
	}
	if err := os.RemoveAll(dir); err != nil {
		return false
	}
	return true
}

var (
	// cliBinOnce guards the fallback build for callers that run without this
	// package's TestMain (e.g. a single test binary run with -run from
	// another entry point); with TestMain present the once is a no-op hit.
	cliBinOnce     sync.Once
	cliBinPath     string
	cliBinBuildErr error
)

// buildTestCLI is the shared builder: exactly one `go build ./cmd/bunker` per
// test process, into THIS checkout's cache dir. TestMain calls it before any
// test runs so the cost leaves the measured test window; buildCLIOnce wraps it
// with the same sync.Once so a caller without TestMain still gets the
// single-build contract.
func buildTestCLI() error {
	if _, err := exec.LookPath("go"); err != nil {
		// No toolchain: skip the pre-build here; the real-process tests
		// hit their existing LookPath guards and skip themselves.
		return nil
	}
	co := testCLICheckoutInfo()
	dir := testCLICacheDir(os.TempDir(), co.key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create build cache dir: %w", err)
	}
	// Housekeeping, best-effort and never fatal: claim this dir for the
	// checkout (its marker mtime is the staleness clock), drop sibling caches
	// whose checkout is gone, and retire the pre-fix uid-only dir so the
	// shared binary it holds stops being a cross-worktree hazard.
	now := time.Now()
	writeCLICacheMarker(dir, co.root)
	sweepStaleCLICaches(testCLICacheRoot(os.TempDir()), co.key, now)
	purgeLegacyUIDCacheDir(os.TempDir(), os.Getuid(), now)

	// Best-effort cross-process lock: two concurrent `go test` processes of
	// THIS checkout (race runs, two invocations) would otherwise build into
	// the same output path simultaneously. The lock only guards the build;
	// failure to take it degrades to the pre-fix behavior, not to an error.
	if f, ferr := os.OpenFile(filepath.Join(dir, cliBuildCacheLockName), os.O_CREATE|os.O_RDWR, 0o600); ferr == nil {
		if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); lockErr == nil {
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		defer f.Close()
	}
	build := exec.Command("go", "build",
		"-p", strconv.Itoa(testCLIBuildConcurrency()),
		"-o", sharedCLIBinaryPath(), "./cmd/bunker")
	// The binary must come from THIS checkout: build from the resolved
	// checkout root rather than from the process's cwd (which a `go test -c`
	// binary run elsewhere would not guarantee).
	build.Dir = co.moduleRoot
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build ./cmd/bunker: %w\n%s", err, out)
	}
	cliBinPath = sharedCLIBinaryPath()
	return nil
}

// TestMain pre-builds the shared test CLI once, outside the measured test
// window, with build parallelism bounded by BUNKER_TEST_CLI_BUILD_CONCURRENCY
// (clamped 2-16, default 8 — GAP-090 load hygiene) and into this checkout's own
// cache dir (INT-CI-023).
func TestMain(m *testing.M) {
	if err := buildTestCLI(); err != nil {
		fmt.Fprintf(os.Stderr, "gap090: shared test CLI build failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// buildCLIOnce returns the shared CLI binary path, building it lazily (once)
// only when TestMain could not — the sync.Once is the contract that this
// package never compiles ./cmd/bunker more than once per test process.
func buildCLIOnce(t *testing.T) string {
	t.Helper()
	cliBinOnce.Do(func() {
		if cliBinPath == "" {
			cliBinBuildErr = buildTestCLI()
		}
	})
	if cliBinBuildErr != nil {
		t.Fatalf("build shared test CLI: %v", cliBinBuildErr)
	}
	if cliBinPath == "" {
		t.Skip("go toolchain not on PATH: no shared test CLI could be built")
	}
	return cliBinPath
}

// TestClampTestCLIBuildConcurrency pins the GAP-090 knob's clamp window:
// unset/blank/garbage fall back to the default, values below 2 or above 16 are
// pinned to the nearer bound, and in-range values pass through unchanged.
func TestClampTestCLIBuildConcurrency(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		def  int
		want int
	}{
		"unset_falls_back":     {raw: "", def: 8, want: 8},
		"blank_falls_back":     {raw: "   ", def: 8, want: 8},
		"garbage_falls_back":   {raw: "eight", def: 8, want: 8},
		"zero_clamped_to_min":  {raw: "0", def: 8, want: 2},
		"one_clamped_to_min":   {raw: "1", def: 8, want: 2},
		"two_kept":             {raw: "2", def: 8, want: 2},
		"midrange_kept":        {raw: "7", def: 8, want: 7},
		"sixteen_kept":         {raw: "16", def: 8, want: 16},
		"seventeen_to_max":     {raw: "17", def: 8, want: 16},
		"huge_to_max":          {raw: "4096", def: 8, want: 16},
		"negative_clamped_min": {raw: "-3", def: 8, want: 2},
		"blank_with_low_def":   {raw: "", def: 4, want: 4},
	} {
		t.Run(name, func(t *testing.T) {
			if got := clampTestCLIBuildConcurrency(tc.raw, tc.def); got != tc.want {
				t.Fatalf("clampTestCLIBuildConcurrency(%q, %d) = %d, want %d", tc.raw, tc.def, got, tc.want)
			}
		})
	}
}

// isLowerHex reports whether s is non-empty and made only of lowercase hex
// digits — the shape of the checkout hash in a cache key.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// INT-CI-023 criterion 1 + 2: the cache path is keyed on the CHECKOUT. Two
// fabricated worktree roots (the wave case) must produce different paths, the
// same root must produce a stable path, and the pre-fix uid-only key must not
// survive as the key. Driven through the path-derivation functions directly —
// no real worktree is created.
func TestSharedCLIBinaryCachePathPerCheckout(t *testing.T) {
	const tempDir = "/tmp"
	rootA := "/home/kara/worktrees/bunker-INT-CI-023"
	rootB := "/home/kara/worktrees/bunker-DF-BUNKER-40"

	for name, tc := range map[string]struct {
		root  string
		other string
		same  bool
	}{
		"same_root_is_stable":            {root: rootA, other: rootA, same: true},
		"different_worktrees_differ":     {root: rootA, other: rootB},
		"trailing_separator_same_root":   {root: rootA + string(filepath.Separator), other: rootA, same: true},
		"dot_segments_are_the_same_root": {root: rootA + "/./.", other: rootA, same: true},
		"same_basename_other_parent":     {root: "/w/bunker-A", other: "/other/bunker-A"},
		"main_clone_vs_worktree":         {root: "/home/kara/bunker", other: rootA},
	} {
		t.Run(name, func(t *testing.T) {
			got := testCLICacheBinaryPath(tempDir, testCLICacheKey(tc.root))
			other := testCLICacheBinaryPath(tempDir, testCLICacheKey(tc.other))
			if tc.same && got != other {
				t.Fatalf("path for the same checkout drifted: %q != %q", got, other)
			}
			if !tc.same && got == other {
				t.Fatalf("two checkouts share one cache binary: %q for %q and %q", got, tc.root, tc.other)
			}
		})
	}

	t.Run("path_shape", func(t *testing.T) {
		key := testCLICacheKey(rootA)
		p := testCLICacheBinaryPath(tempDir, key)
		// QA-BUNKER-001: the root is per-UID, so the expected shape embeds
		// this uid via the same derivation the builder uses.
		if want := filepath.Join(testCLICacheRoot(tempDir), key, cliBuildCacheBinaryName); p != want {
			t.Fatalf("path = %q, want %q", p, want)
		}
		if filepath.Base(p) != cliBuildCacheBinaryName {
			t.Fatalf("binary file name = %q, want %q", filepath.Base(p), cliBuildCacheBinaryName)
		}
		// The key stays human-readable (the checkout's directory name, so a
		// temp-dir listing says WHICH tree owns a binary) plus a fixed-width
		// hash of the checkout root, re-derived here independently so this is
		// an assertion about the derivation and not a restatement of it.
		sum := sha256.Sum256([]byte(filepath.Clean(rootA)))
		wantHash := hex.EncodeToString(sum[:])[:cliBuildCacheKeyEntropy]
		hashSuffix := key[strings.LastIndex(key, "-")+1:]
		checks := []struct {
			what string
			ok   bool
		}{
			{"carries the checkout name", strings.HasPrefix(key, "bunker-INT-CI-023-")},
			{"is a single path element", !strings.ContainsAny(key, `/\`)},
			{"ends in a hex hash", isLowerHex(hashSuffix)},
			{"hash is the configured width", len(hashSuffix) == cliBuildCacheKeyEntropy},
			{"hash is of the checkout root", hashSuffix == wantHash},
		}
		for _, c := range checks {
			if !c.ok {
				t.Fatalf("cache key %q does not satisfy: %s", key, c.what)
			}
		}
	})

	// A per-run or per-uid path would be a REGRESSION, not a fix: a PID key
	// defeats the shared build (one compile per run) and re-creates the uid
	// collision the task exists to remove.
	t.Run("not_per_run_not_per_uid", func(t *testing.T) {
		p := testCLICacheBinaryPath(tempDir, testCLICacheKey(rootA))
		if strings.Contains(p, strconv.Itoa(os.Getpid())) {
			t.Fatalf("cache path carries the PID (per-run dir, not a cache): %q", p)
		}
		if filepath.Dir(p) == legacyUIDCacheDir(tempDir, os.Getuid()) {
			t.Fatalf("cache path IS the pre-fix uid-only dir: %q", p)
		}
		if base := filepath.Base(filepath.Dir(p)); base == fmt.Sprintf("%s%d", legacyCLIBuildCachePrefix, os.Getuid()) {
			t.Fatalf("cache dir is keyed on the uid alone: %q", base)
		}
	})
}

// INT-CI-023 criterion 2 against the LIVE derivation: the path this process
// actually uses comes from the checkout it is running in, not from the uid.
func TestSharedCLIBinaryCacheKeyIsNotUIDOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	got := sharedCLIBinaryPath()
	legacy := legacyUIDCacheBinaryPath()
	if got == legacy {
		t.Fatalf("shared binary is still the uid-only path: %q", got)
	}
	if filepath.Dir(got) == filepath.Dir(legacy) {
		t.Fatalf("shared cache dir is still the uid-only dir: %q", filepath.Dir(got))
	}
	if strings.Contains(filepath.Base(filepath.Dir(got)), fmt.Sprintf("uid%d", os.Getuid())) {
		t.Fatalf("cache dir still keyed by uid: %q", filepath.Base(filepath.Dir(got)))
	}

	// The derivation must be anchored on the CHECKOUT, not on the process
	// environment or cwd: git's own answer must be the root the key hashes.
	co := testCLICheckoutInfo()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("git rev-parse in this checkout: %v", err)
	}
	toplevel := filepath.Clean(strings.TrimSpace(string(out)))
	if co.root != toplevel {
		t.Fatalf("cache key root = %q, want this checkout %q", co.root, toplevel)
	}
	if want := testCLICacheBinaryPath(os.TempDir(), testCLICacheKey(toplevel)); got != want {
		t.Fatalf("sharedCLIBinaryPath() = %q, want the path derived from this checkout %q", got, want)
	}
}

// INT-CI-023 criterion 4 + wiring: the dir this process builds into is claimed
// by THIS checkout, lives under the cache root, and is not the pre-fix shared
// dir. Read-only — it asserts the live artifact the suite execs.
func TestSharedCLICacheDirOwnedByThisCheckout(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}
	co := testCLICheckoutInfo()
	dir := testCLICacheDir(os.TempDir(), co.key)
	if dir == legacyUIDCacheDir(os.TempDir(), os.Getuid()) {
		t.Fatalf("build cache dir is the pre-fix uid-only dir: %q", dir)
	}
	if filepath.Dir(dir) != testCLICacheRoot(os.TempDir()) {
		t.Fatalf("build cache dir %q is not under the cache root %q", dir, testCLICacheRoot(os.TempDir()))
	}
	data, err := os.ReadFile(cliCacheMarkerPath(dir))
	if err != nil {
		t.Fatalf("read cache marker: %v", err)
	}
	if owner := strings.TrimSpace(string(data)); owner != co.root {
		t.Fatalf("cache marker names %q, want this checkout %q", owner, co.root)
	}
	if _, err := os.Stat(filepath.Join(dir, cliBuildCacheBinaryName)); err != nil {
		t.Fatalf("shared binary missing from this checkout's cache dir: %v", err)
	}
}

// INT-CI-023: the explicit-key override accepts exactly one path element and
// falls back to the derived key for anything unusable, so a typo cannot point
// two checkouts at one binary or escape the cache root.
func TestTestCLICacheKeyOverride(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want string
	}{
		"unset_ignored":      {raw: "", want: ""},
		"blank_ignored":      {raw: "   ", want: ""},
		"dot_ignored":        {raw: ".", want: ""},
		"dotdot_ignored":     {raw: "..", want: ""},
		"relative_path":      {raw: "a/b", want: ""},
		"absolute_path":      {raw: "/etc", want: ""},
		"backslash_ignored":  {raw: `a\b`, want: ""},
		"plain_key_kept":     {raw: "ci-lane-1", want: "ci-lane-1"},
		"padded_key_trimmed": {raw: "  ci-lane-1 ", want: "ci-lane-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sanitizeTestCLICacheKey(tc.raw); got != tc.want {
				t.Fatalf("sanitizeTestCLICacheKey(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}

	const root = "/home/kara/worktrees/bunker-INT-CI-023"
	for name, tc := range map[string]struct {
		explicit string
		want     string
	}{
		"no_override_derives":  {explicit: "", want: testCLICacheKey(root)},
		"bad_override_derives": {explicit: "../escape", want: testCLICacheKey(root)},
		"override_wins":        {explicit: "ci-lane-1", want: "ci-lane-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := testCLICacheKeyForRoot(root, tc.explicit); got != tc.want {
				t.Fatalf("testCLICacheKeyForRoot(%q, %q) = %q, want %q", root, tc.explicit, got, tc.want)
			}
		})
	}
}

// INT-CI-023 stale-cache semantics: a dir whose marker is old (or missing —
// an unreadable marker) is reapable, a recently used one is not, and a future
// timestamp is never stale.
func TestTestCLICacheStaleDecision(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		marker time.Time
		want   bool
	}{
		"missing_marker_is_stale": {marker: time.Time{}, want: true},
		"just_used_is_fresh":      {marker: now, want: false},
		"one_hour_is_fresh":       {marker: now.Add(-time.Hour), want: false},
		"one_minute_under":        {marker: now.Add(-cliBuildCacheRetention + time.Minute), want: false},
		"exactly_at_retention":    {marker: now.Add(-cliBuildCacheRetention), want: false},
		"one_second_over":         {marker: now.Add(-cliBuildCacheRetention - time.Second), want: true},
		"two_days_old_is_stale":   {marker: now.Add(-48 * time.Hour), want: true},
		"future_marker_is_fresh":  {marker: now.Add(time.Hour), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := testCLICacheStale(tc.marker, now, cliBuildCacheRetention); got != tc.want {
				t.Fatalf("testCLICacheStale(%v, %v) = %v, want %v", tc.marker, now, got, tc.want)
			}
		})
	}
}

// INT-CI-023 criterion 4 on the sweeper: the sweep only ever touches dirs under
// the root it is handed, reaps exactly the stale ones, and always keeps the
// key this process is using.
func TestSweepStaleCLICaches(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	mkCache := func(key string, markerAge time.Duration, withMarker bool) string {
		t.Helper()
		dir := filepath.Join(root, key)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, cliBuildCacheBinaryName), []byte("x"), 0o700); err != nil {
			t.Fatalf("write binary: %v", err)
		}
		if withMarker {
			marker := cliCacheMarkerPath(dir)
			if err := os.WriteFile(marker, []byte("/gone/away\n"), 0o600); err != nil {
				t.Fatalf("write marker: %v", err)
			}
			mod := now.Add(-markerAge)
			if err := os.Chtimes(marker, mod, mod); err != nil {
				t.Fatalf("chtimes: %v", err)
			}
		}
		return dir
	}

	fresh := mkCache("bunker-live-abc123", time.Hour, true)
	stale := mkCache("bunker-gone-def456", 48*time.Hour, true)
	unmarked := mkCache("bunker-nomarker", 0, false)
	ownKey := "bunker-own-xyz789"
	own := mkCache(ownKey, 48*time.Hour, true) // even stale: it is OUR key

	got := sweepStaleCLICaches(root, ownKey, now)
	want := []string{"bunker-gone-def456", "bunker-nomarker"}
	if len(got) != len(want) {
		t.Fatalf("swept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("swept %v, want %v", got, want)
		}
	}
	for _, keep := range []string{fresh, own} {
		if _, err := os.Stat(filepath.Join(keep, cliBuildCacheBinaryName)); err != nil {
			t.Fatalf("survivor %s was reaped: %v", keep, err)
		}
	}
	for _, gone := range []string{stale, unmarked} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("stale dir %s survived: %v", gone, err)
		}
	}

	t.Run("missing_root_is_not_fatal", func(t *testing.T) {
		if rem := sweepStaleCLICaches(filepath.Join(root, "does-not-exist"), ownKey, now); rem != nil {
			t.Fatalf("sweep of a missing root returned %v, want nil", rem)
		}
	})
}

// INT-CI-023 cleanup: the pre-fix uid-only dir is retired only once its binary
// is stale — a live pre-fix run rebuilds it first, so a fresh one means someone
// still uses it and it is left alone.
func TestPurgeLegacyUIDCacheDir(t *testing.T) {
	tempDir := t.TempDir()
	uid := os.Getuid()
	now := time.Now()
	dir := legacyUIDCacheDir(tempDir, uid)
	binary := filepath.Join(dir, cliBuildCacheBinaryName)

	t.Run("absent_dir_is_noop", func(t *testing.T) {
		if purgeLegacyUIDCacheDir(tempDir, uid, now) {
			t.Fatalf("purged %s which does not exist", dir)
		}
	})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(binary, []byte("legacy"), 0o700); err != nil {
		t.Fatalf("write %s: %v", binary, err)
	}

	t.Run("fresh_dir_kept", func(t *testing.T) {
		recent := now.Add(-time.Hour)
		if err := os.Chtimes(binary, recent, recent); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if purgeLegacyUIDCacheDir(tempDir, uid, now) {
			t.Fatalf("purged a fresh %s (a pre-fix run may still be using it)", dir)
		}
		if _, err := os.Stat(binary); err != nil {
			t.Fatalf("fresh legacy binary removed: %v", err)
		}
	})

	t.Run("stale_dir_purged", func(t *testing.T) {
		old := now.Add(-48 * time.Hour)
		if err := os.Chtimes(binary, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if !purgeLegacyUIDCacheDir(tempDir, uid, now) {
			t.Fatalf("stale legacy dir %s was not purged", dir)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("stale legacy dir survived: %v", err)
		}
	})
}

// sharedCLIBuildMarker is the line TestSharedCLIBinarySingleBuildHelper prints
// once per build it observes; the parent asserts it appears exactly once.
const sharedCLIBuildMarker = "SHARED_CLI_BUILD observed="

// TestSharedCLIBinarySingleBuildHelper is the child process behind
// TestSharedCLIBinarySingleBuildPerProcess: it exercises the consumer seam
// three times (the pre-fix shape — one build per consumer call) and reports
// how many builds it observed.
func TestSharedCLIBinarySingleBuildHelper(t *testing.T) {
	for i := 0; i < 3; i++ {
		p := buildCLIOnce(t)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("shared CLI binary %q: %v", p, err)
		}
	}
	fmt.Printf("%s%d concurrency=%d\n", sharedCLIBuildMarker, 1, testCLIBuildConcurrency())
}

// TestSharedCLIBinarySingleBuildPerProcess proves the single-build contract
// end-to-end: a fresh test binary of this package that calls the consumer seam
// three times performs at most ONE `go build` (TestMain's), no matter how many
// consumers link the binary. Pre-fix — one `go build` per consumer call — a
// three-consumer process compiled the CLI three times.
func TestSharedCLIBinarySingleBuildPerProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child test binary; skipped in -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedCLIBinarySingleBuildHelper$", "-test.v=true")
	cmd.Env = append(os.Environ(), cliBuildConcurrencyEnv+"=12") // in-range: must survive the clamp
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	var observed []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, sharedCLIBuildMarker) {
			observed = append(observed, line)
		}
	}
	if len(observed) != 1 {
		t.Fatalf("expected exactly 1 %s line across 3 consumer calls, got %d:\n%s",
			sharedCLIBuildMarker, len(observed), out)
	}
	if !strings.Contains(observed[0], "concurrency=12") {
		t.Errorf("marker does not carry the in-range knob value 12: %s", observed[0])
	}
}

// TestNoPerTestCLIBuildsOutsideSharedBuilder is the mutation-proof half of the
// GAP-090 gate: the child-process probe above can only see the seam it calls,
// so this source invariant scans every test file in the package (except the
// shared builder itself) for a private `go build` invocation — the pre-fix
// shape of one full CLI compile per consumer. Re-introduce one anywhere in the
// package and this test reddens.
func TestNoPerTestCLIBuildsOutsideSharedBuilder(t *testing.T) {
	patterns := []string{
		`exec.Command("go", "build"`,
		"exec.Command(`go`, `build`",
	}
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob *_test.go in package dir: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if f == "procbuild_test.go" {
			continue // the sanctioned shared builder
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, p := range patterns {
			if strings.Contains(string(data), p) {
				t.Errorf("%s re-introduces a private `go build` (per-test compile — the GAP-090 offender shape): %q", f, p)
			}
		}
	}
}

// QA-BUNKER-001: the cache ROOT is per-UID. The multi-tenant failure it fixes:
// the root used to be one fixed shared name under the OS temp dir; whichever
// uid ran the suite first owned it 0700 and every later uid died in TestMain
// with "gap090: shared test CLI build failed: create build cache dir: mkdir
// ...: permission denied" before any test ran. Embedding the uid in the ROOT
// name gives each uid its own 0700 root; the per-CHECKOUT key below it is
// unchanged (INT-CI-023's fix is not being relitigated — the uid is in the
// ROOT level only, never in the checkout key).
//
// Cannot run as a different uid unprivileged, so the cross-uid claim is
// proven by construction: this uid's root name carries THIS uid, and a 0700
// root named for a DIFFERENT uid does not collide with (nor get walked by)
// this uid's derivation. EEXIST-tolerance on the MkdirAll is covered by
// buildTestCLI already succeeding on a pre-existing root — asserted here via
// the real builder's contract with a same-uid root pre-created 0700.
func TestCLICacheRootIsPerUID(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: a 0700 root owned by another uid is still writable, the per-uid separation cannot be exercised")
	}

	t.Run("root_name_carries_uid", func(t *testing.T) {
		tempDir := t.TempDir()
		root := testCLICacheRoot(tempDir)
		wantSuffix := fmt.Sprintf("%s-%d", cliBuildCacheRootName, os.Getuid())
		if want, got := filepath.Join(tempDir, wantSuffix), root; got != want {
			t.Fatalf("cache root = %q, want per-uid root %q", got, want)
		}
		if filepath.Base(root) != wantSuffix {
			t.Fatalf("cache root base = %q, want %q", filepath.Base(root), wantSuffix)
		}
		// One path element: the uid rides in the name, not in a subdir.
		if rel, err := filepath.Rel(tempDir, root); err != nil || strings.Contains(rel, string(filepath.Separator)) {
			t.Fatalf("cache root %q is not a single element under %q (rel %q, err %v)", root, tempDir, rel, err)
		}
	})

	t.Run("other_uid_root_does_not_collide", func(t *testing.T) {
		tempDir := t.TempDir()
		otherUID := os.Getuid() + 1
		// The shape of the original incident: another uid's 0700 root.
		otherRoot := filepath.Join(tempDir, fmt.Sprintf("%s-%d", cliBuildCacheRootName, otherUID))
		if err := os.MkdirAll(otherRoot, 0o700); err != nil {
			t.Fatalf("seed other-uid root: %v", err)
		}
		// Deriving and materialising THIS uid's cache dir must succeed —
		// the derivation lands in a DIFFERENT root, so the other uid's
		// 0700 directory cannot block it (the pre-fix EACCES).
		dir := testCLICacheDir(tempDir, testCLICacheKey(t.TempDir()))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll into own-uid cache dir failed with a foreign 0700 root present: %v", err)
		}
		if filepath.Dir(dir) != testCLICacheRoot(tempDir) {
			t.Fatalf("cache dir %q does not live under the per-uid root %q", dir, testCLICacheRoot(tempDir))
		}
		if _, err := os.Stat(filepath.Join(otherRoot, "probe")); !os.IsNotExist(err) {
			t.Fatalf("own derivation touched the other uid's root: stat probe err=%v", err)
		}
	})

	t.Run("sweep_operates_on_per_uid_root", func(t *testing.T) {
		tempDir := t.TempDir()
		root := testCLICacheRoot(tempDir)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("seed own root: %v", err)
		}
		// A stale sibling CHECKOUT cache dir inside THIS uid's root is
		// reaped (absent marker = stale by contract); ownKey is kept
		// unconditionally; and nothing inside the OTHER uid's root is
		// touched — the sweep walks only the per-uid root, never the
		// shared parent.
		ownStale := filepath.Join(root, "own-stale")
		checkoutStale := filepath.Join(root, "checkout-stale")
		if err := os.MkdirAll(filepath.Join(root, "own-stale"), 0o700); err != nil {
			t.Fatalf("seed own stale dir: %v", err)
		}
		if err := os.MkdirAll(checkoutStale, 0o700); err != nil {
			t.Fatalf("seed stale checkout dir: %v", err)
		}
		otherUID := os.Getuid() + 1
		otherRoot := filepath.Join(tempDir, fmt.Sprintf("%s-%d", cliBuildCacheRootName, otherUID))
		otherStale := filepath.Join(otherRoot, "other-stale")
		if err := os.MkdirAll(otherStale, 0o700); err != nil {
			t.Fatalf("seed other-uid stale dir: %v", err)
		}
		if removed := sweepStaleCLICaches(root, "own-stale", time.Now()); len(removed) != 1 || removed[0] != "checkout-stale" {
			t.Fatalf("sweep removed %v, want [checkout-stale]", removed)
		}
		if _, err := os.Stat(ownStale); err != nil {
			t.Fatalf("sweep reaped ownKey %q (must keep its own): %v", ownStale, err)
		}
		if _, err := os.Stat(checkoutStale); err == nil {
			t.Fatalf("sweep left stale checkout dir %q in place", checkoutStale)
		}
		if _, err := os.Stat(otherStale); err != nil {
			t.Fatalf("sweep walked outside the per-uid root: other uid's dir removed: %v", err)
		}
	})

	t.Run("builder_tolerates_existing_root", func(t *testing.T) {
		// The EEXIST leg: a root that ALREADY exists (the normal steady
		// state, and the pre-fix MkdirAll's implicit-create path) must not
		// fail the build. Proven on the real builder's precondition —
		// MkdirAll on an existing 0700 dir of this uid returns nil.
		tempDir := t.TempDir()
		root := testCLICacheRoot(tempDir)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("seed root: %v", err)
		}
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatalf("chmod root: %v", err)
		}
		dir := testCLICacheDir(tempDir, testCLICacheKey(t.TempDir()))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll tolerated an existing root elsewhere but not here: %v", err)
		}
	})
}
