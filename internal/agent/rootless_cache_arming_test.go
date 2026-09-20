package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// uuValidInstaller is a payload that passes validateCachedInstaller (shebang
// + minimum viable size): the same artifact shape the existing
// rootless_userunit_test.go cache tests use.
var uuValidInstaller = []byte("#!/bin/sh\n# cached rootless installer for tests\nset -euo pipefail\n# payload ensures the cached artifact is large enough for the production\n# minimum-size validation.\necho cached\nexit 0\n")

// TestSetRootlessInstallerCacheDir_ArmsCachePath is the GAP-091 regression:
// arming the cache through the production seam (SetRootlessInstallerCacheDir,
// the exact function cmd/bunkerd calls at startup) must produce a real CACHE
// HIT on the second download — not merely a changed variable. The uncached
// download seam is counted; if it is ever reached the test fails.
func TestSetRootlessInstallerCacheDir_ArmsCachePath(t *testing.T) {
	cacheDir := t.TempDir()
	prev := rootlessInstallerCacheDir
	SetRootlessInstallerCacheDir(cacheDir)
	t.Cleanup(func() { rootlessInstallerCacheDir = prev })

	// Assert the seam sets the var…
	if rootlessInstallerCacheDir != cacheDir {
		t.Fatalf("SetRootlessInstallerCacheDir(%q) left the var at %q", cacheDir, rootlessInstallerCacheDir)
	}
	// …and that the read seam agrees (what an observer outside the package sees).
	if got := GetRootlessInstallerCacheDir(); got != cacheDir {
		t.Fatalf("GetRootlessInstallerCacheDir() = %q, want %q", got, cacheDir)
	}

	// Pre-seed a valid cached installer (same shape as
	// TestCachedRootlessInstallerDownload_CacheHit).
	if err := os.WriteFile(filepath.Join(cacheDir, rootlessInstallerCacheKey()), uuValidInstaller, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	downloads := 0
	prevDownload := rootlessInstallerDownload
	rootlessInstallerDownload = func(ctx context.Context, path string) ([]byte, error) {
		downloads++
		return nil, os.WriteFile(path, uuValidInstaller, 0o644)
	}
	t.Cleanup(func() { rootlessInstallerDownload = prevDownload })

	// First download: a pre-seeded valid cache entry means ZERO network
	// fetches already.
	dst1 := filepath.Join(t.TempDir(), "installer-1.sh")
	out1, err := cachedRootlessInstallerDownload(context.Background(), dst1)
	if err != nil {
		t.Fatalf("first cached download: %v", err)
	}

	// Second download: must be served from the cache with no second fetch.
	dst2 := filepath.Join(t.TempDir(), "installer-2.sh")
	out2, err := cachedRootlessInstallerDownload(context.Background(), dst2)
	if err != nil {
		t.Fatalf("second cached download: %v", err)
	}

	if downloads != 0 {
		t.Fatalf("armed cache produced %d network downloads, want 0 (cache must serve both calls)", downloads)
	}
	if !bytes.Equal(out1, uuValidInstaller) || !bytes.Equal(out2, uuValidInstaller) {
		t.Fatalf("cached downloads returned wrong bytes: out1=%q out2=%q", out1, out2)
	}
	installed, err := os.ReadFile(dst2)
	if err != nil {
		t.Fatalf("read installed installer: %v", err)
	}
	if !bytes.Equal(installed, uuValidInstaller) {
		t.Fatalf("installed installer = %q, want the cached bytes", installed)
	}
}

// TestSetRootlessInstallerCacheDir_MissPopulatesThenHits proves the armed
// cache also works from COLD start: the first call downloads exactly once and
// populates the cache, the second call is served from it with no second fetch.
func TestSetRootlessInstallerCacheDir_MissPopulatesThenHits(t *testing.T) {
	cacheDir := t.TempDir()
	prev := rootlessInstallerCacheDir
	SetRootlessInstallerCacheDir(cacheDir)
	t.Cleanup(func() { rootlessInstallerCacheDir = prev })

	downloads := 0
	prevDownload := rootlessInstallerDownload
	rootlessInstallerDownload = func(ctx context.Context, path string) ([]byte, error) {
		downloads++
		return nil, os.WriteFile(path, uuValidInstaller, 0o644)
	}
	t.Cleanup(func() { rootlessInstallerDownload = prevDownload })

	dst1 := filepath.Join(t.TempDir(), "installer-1.sh")
	if _, err := cachedRootlessInstallerDownload(context.Background(), dst1); err != nil {
		t.Fatalf("first cached download: %v", err)
	}
	if downloads != 1 {
		t.Fatalf("cache miss must download exactly once, got %d", downloads)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, rootlessInstallerCacheKey())); err != nil {
		t.Fatalf("cache was not populated on miss: %v", err)
	}

	dst2 := filepath.Join(t.TempDir(), "installer-2.sh")
	out2, err := cachedRootlessInstallerDownload(context.Background(), dst2)
	if err != nil {
		t.Fatalf("second cached download: %v", err)
	}
	if downloads != 1 {
		t.Fatalf("second download must be served from cache (downloads=%d, want 1)", downloads)
	}
	if !bytes.Equal(out2, uuValidInstaller) {
		t.Fatalf("cache-hit bytes = %q, want the downloaded installer", out2)
	}
}

// TestUnarmedCacheDir_KeepsLegacyUncachedPath is the backward-compat half of
// GAP-091: with the cache dir empty (an operator that never configured the
// knob, or explicit rootless_installer_cache_dir: ""), the cached download
// fails fast WITHOUT touching the network or the filesystem, and the
// production install path falls back to the legacy uncached download seam.
func TestUnarmedCacheDir_KeepsLegacyUncachedPath(t *testing.T) {
	prev := rootlessInstallerCacheDir
	SetRootlessInstallerCacheDir("")
	t.Cleanup(func() { rootlessInstallerCacheDir = prev })

	cachedCalls := 0
	prevCached := cachedRootlessInstallerDownload
	cachedRootlessInstallerDownload = func(ctx context.Context, path string) ([]byte, error) {
		cachedCalls++
		return downloadRootlessInstallerCached(ctx, path)
	}
	t.Cleanup(func() { cachedRootlessInstallerDownload = prevCached })

	uncachedCalls := 0
	prevDownload := rootlessInstallerDownload
	rootlessInstallerDownload = func(ctx context.Context, path string) ([]byte, error) {
		uncachedCalls++
		return nil, os.WriteFile(path, uuValidInstaller, 0o644)
	}
	t.Cleanup(func() { rootlessInstallerDownload = prevDownload })

	// Mirror the production fallback shape (rootless.go, install path):
	// consult the cache first; when it fails and the dir is empty, take the
	// legacy uncached seam.
	installerPath := filepath.Join(t.TempDir(), "installer.sh")
	if _, err := cachedRootlessInstallerDownload(context.Background(), installerPath); err == nil {
		t.Fatal("cached download with empty cache dir must fail (empty means no host cache)")
	} else if rootlessInstallerCacheDir == "" {
		if _, err := rootlessInstallerDownload(context.Background(), installerPath); err != nil {
			t.Fatalf("legacy uncached fallback: %v", err)
		}
	} else {
		t.Fatalf("expected the legacy uncached branch to be taken, cache dir = %q", rootlessInstallerCacheDir)
	}

	if cachedCalls != 1 {
		t.Fatalf("cached seam consulted %d times, want 1", cachedCalls)
	}
	if uncachedCalls != 1 {
		t.Fatalf("legacy uncached seam called %d times, want exactly 1", uncachedCalls)
	}
	installed, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatalf("legacy path did not deliver the installer: %v", err)
	}
	if !bytes.Equal(installed, uuValidInstaller) {
		t.Fatalf("legacy-installed installer = %q, want the uncached bytes", installed)
	}
}
