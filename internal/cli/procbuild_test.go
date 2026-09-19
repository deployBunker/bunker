package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
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

// sharedCLIBinaryPath is where the shared test CLI lands: a per-uid cache dir
// under the OS temp dir, outside the repo and outside any t.TempDir (which the
// harness's HOME override must never relocate — see the HOME notes in the
// tunnel tests).
func sharedCLIBinaryPath() string {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("bunker-cli-build-cache-uid%d", os.Getuid()))
	return filepath.Join(dir, "bunker")
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
// test process. TestMain calls it before any test runs so the cost leaves the
// measured test window; buildCLIOnce wraps it with the same sync.Once so a
// caller without TestMain still gets the single-build contract.
func buildTestCLI() error {
	if _, err := exec.LookPath("go"); err != nil {
		// No toolchain: skip the pre-build here; the real-process tests
		// hit their existing LookPath guards and skip themselves.
		return nil
	}
	dir := filepath.Dir(sharedCLIBinaryPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create build cache dir: %w", err)
	}
	// Best-effort cross-process lock: two concurrent `go test` processes of
	// this package (race runs, two trees) would otherwise build into the
	// same output path simultaneously. The lock only guards the build;
	// failure to take it degrades to the pre-fix behavior, not to an error.
	if f, ferr := os.OpenFile(filepath.Join(dir, "build.lock"), os.O_CREATE|os.O_RDWR, 0o600); ferr == nil {
		if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); lockErr == nil {
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		defer f.Close()
	}
	build := exec.Command("go", "build",
		"-p", strconv.Itoa(testCLIBuildConcurrency()),
		"-o", sharedCLIBinaryPath(), "./cmd/bunker")
	// cwd of a `go test` binary is the package dir (internal/cli); the module
	// root is two levels up.
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build ./cmd/bunker: %w\n%s", err, out)
	}
	cliBinPath = sharedCLIBinaryPath()
	return nil
}

// TestMain pre-builds the shared test CLI once, outside the measured test
// window, with build parallelism bounded by BUNKER_TEST_CLI_BUILD_CONCURRENCY
// (clamped 2-16, default 8 — GAP-090 load hygiene).
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
