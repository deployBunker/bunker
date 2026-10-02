package cli

// QA-BUNKER-23 tripwire: a test in this package used to write the REAL
// operator ~/.bunker/config.yaml (TestInfoCommand_OrphanUIDWarning reached
// SaveCLIConfig without t.Setenv("HOME", ...)), clobbering the operator's
// server registry. These helpers harden the whole package against that class
// of bug:
//
//  1. pinSentinelTestHome points the test process's HOME at a disposable
//     sentinel directory for the entire run, so even a future un-isolated
//     config write lands in the sentinel instead of the operator's home.
//     It also neutralizes the rest of the GAP-181 resolution chain's env
//     tiers — XDG_CONFIG_HOME, BUNKER_HOME, BUNKER_CONFIG_HOME — because a
//     hosted runner exports XDG_CONFIG_HOME and the chain prefers it over
//     $HOME/.config: with only HOME pinned, every HOME-only test resolved
//     the config to one fixed runner-level dir OUTSIDE its t.TempDir(),
//     writes vanished there, and later tests read the leaked file (the
//     INT-CI-56 hosted-CI failure class).
//  2. checkSentinelTestHome fails the run when anything appeared under
//     <sentinel>/.bunker (legacy) or <sentinel>/.config/bunker (the
//     documented post-GAP-181 config dir) — a leaky test must not pass just
//     because the sentinel absorbed the damage.
//
// The pin happens in TestMain (procbuild_test.go) around m.Run(); the
// GOCACHE/GOPATH mirroring keeps `go` child processes (the shared test-CLI
// build and the concurrency-clamp test's build) on the ambient HOME's real
// caches — go derives both from $HOME, and a cold sentinel cache would slow
// every run.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// pinSentinelTestHome swaps the process HOME for a fresh sentinel directory
// and returns its path. It exits the process (fail-closed) when the sentinel
// cannot be created: without it a leaky config write would hit the real home.
//
// Helper-mode exemption: the proc-lifecycle tests re-execute this same test
// binary as a helper child (proc_lifecycle_test.go runs
// TestLongLivedChildHelperProcess via os.Args[0]) and SIGKILL it mid-run BY
// DESIGN — its TestMain can never reach its own cleanup. Without this
// exemption every full-suite run leaked one empty sentinel dir under /tmp.
// The helper is safe to exempt: it performs no config writes (it execs a
// parked `sh` child) and inherits the parent's already-isolated HOME via
// os.Environ().
func pinSentinelTestHome() string {
	if os.Getenv("BUNKER_PROC_HELPER") == "1" {
		return ""
	}
	sentinel, err := os.MkdirTemp("", "bunker-cli-test-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "qabunker23: cannot create sentinel test HOME: %v\n", err)
		os.Exit(1)
	}

	// Mirror go's $HOME-derived defaults BEFORE the swap so child `go build`
	// invocations inside the suite keep using the ambient caches.
	if origHome := os.Getenv("HOME"); origHome != "" {
		if os.Getenv("GOCACHE") == "" {
			cacheBase := os.Getenv("XDG_CACHE_HOME")
			if cacheBase == "" {
				cacheBase = filepath.Join(origHome, ".cache")
			}
			_ = os.Setenv("GOCACHE", filepath.Join(cacheBase, "go-build"))
		}
		if os.Getenv("GOPATH") == "" {
			_ = os.Setenv("GOPATH", filepath.Join(origHome, "go"))
		}
	}

	// INT-CI-56: neutralize the ambient resolution seams alongside HOME. A
	// hosted runner exports XDG_CONFIG_HOME and the GAP-181 chain prefers it
	// over $HOME/.config; with only HOME pinned, the per-test t.Setenv pins
	// below stopped matching where the chain actually resolved — every test
	// shared one fixed runner-level config dir OUTSIDE its t.TempDir(),
	// writes vanished there (ENOENT at the test's hand-built path) and later
	// tests read the leaked file (live dials to leaked servers). Point the
	// XDG base at the sentinel and blank the explicit BUNKER_* tiers so the
	// per-test HOME pins are, again, the only resolution variable a test
	// must set. GOCACHE/GOPATH are mirrored first on purpose: they must keep
	// following the ambient home while HOME itself is pointed at the
	// sentinel.
	for _, k := range []string{"XDG_CONFIG_HOME", "BUNKER_HOME", "BUNKER_CONFIG_HOME"} {
		_ = os.Setenv(k, "")
	}
	if err := os.Setenv("HOME", sentinel); err != nil {
		fmt.Fprintf(os.Stderr, "qabunker23: cannot pin test HOME: %v\n", err)
		os.Exit(1)
	}
	return sentinel
}

// checkSentinelTestHome fails the run (exit 1) when the sentinel HOME gained
// a .bunker directory (legacy layout) or a .config/bunker directory (the
// documented post-GAP-181 config dir) — proof that some test resolved the
// config path outside its own t.TempDir()/BUNKER_HOME isolation. A clean
// sentinel is removed; a dirty one is kept on disk for inspection.
func checkSentinelTestHome(sentinel string) {
	if sentinel == "" {
		return // helper child: never pinned, nothing to check
	}
	for _, bunkerDir := range []string{
		filepath.Join(sentinel, ".bunker"),
		filepath.Join(sentinel, ".config", "bunker"),
	} {
		if names, dirty := sentinelDirEntries(bunkerDir); dirty {
			fmt.Fprintf(os.Stderr,
				"qabunker23: FAIL — a test wrote outside its isolated HOME: %s contains %v. "+
					"Every config-writing test must isolate via t.Setenv(\"HOME\", t.TempDir()) "+
					"(or BUNKER_HOME) before its first config write.\n", bunkerDir, names)
			os.Exit(1)
		}
	}
	removeCleanSentinel(sentinel)
}

// sentinelDirEntries reports the entries of one leak-detector directory under
// the sentinel HOME; dirty=true means the run cannot inspect the directory
// (treated as a leak, fail-closed) or the directory is non-empty (a leak).
func sentinelDirEntries(bunkerDir string) ([]string, bool) {
	entries, err := os.ReadDir(bunkerDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		fmt.Fprintf(os.Stderr, "qabunker23: cannot inspect sentinel dir %s: %v\n", bunkerDir, err)
		return nil, true
	}
	if len(entries) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, true
}

// removeCleanSentinel deletes a sentinel that stayed clean. A failed removal
// is litter, not a leak, so it warns on stderr instead of failing the run;
// one retry covers the race with a child process dying while holding the dir.
func removeCleanSentinel(sentinel string) {
	for attempt := 0; ; attempt++ {
		err := os.RemoveAll(sentinel)
		if err == nil {
			return
		}
		if attempt == 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		fmt.Fprintf(os.Stderr, "qabunker23: warning: clean sentinel %s could not be removed: %v\n", sentinel, err)
		return
	}
}
