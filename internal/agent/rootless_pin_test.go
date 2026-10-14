package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/supplychain"
)

// This file owns the GAP-135 gate for the rootless INSTALLER: the script that
// the install path downloads and then executes as the agent user. Three things
// are proven here, and nothing here touches the network:
//
//  1. the production download (the DEFAULT verifier, against the embedded
//     manifest) rejects bytes that are not the pinned artifact, removes them,
//     and fails with the named checksum error;
//  2. the gate is actually wired into that download — a verifier that reports a
//     failure stops the download, not just the check;
//  3. a cache entry whose bytes no longer match the pin is refused loudly
//     rather than executed (the cache is a file on disk).
//
// Mechanism: a stub `curl` on PATH provides the "network" bytes, so the real
// production code path runs end to end without a socket.

// stubRootlessInstallerVerifier swaps the checksum gate for a no-op. It exists
// for the CACHE-PLUMBING tests, which seed synthetic payloads instead of the
// 9 KB upstream script; the gate itself is what this file tests, with the
// production default in place.
func stubRootlessInstallerVerifier(t *testing.T) {
	t.Helper()
	prev := rootlessInstallerVerifier
	rootlessInstallerVerifier = func(string, string, string) error { return nil }
	t.Cleanup(func() { rootlessInstallerVerifier = prev })
}

// fixturePinVerifier pins payload's sha256 under the installer artifact name
// and returns the verification function that enforces it, so a test can drive
// the REAL gate logic (VerifyFileWith) over its own fixture instead of the
// 9 KB upstream script.
func fixturePinVerifier(t *testing.T, payload []byte) supplychain.VerifyFn {
	t.Helper()
	sum := sha256.Sum256(payload)
	raw := `{"schema_version":1,"artifacts":[{"name":"` + supplychain.ArtifactRootlessInstaller +
		`","version":"fixture","variants":[{"arch":"any","url":"https://example.invalid/fixture-installer","sha256":"` +
		hex.EncodeToString(sum[:]) + `"}]}],"images":[]}`
	m, err := supplychain.ParseManifest([]byte(raw))
	if err != nil {
		t.Fatalf("fixture pin manifest does not parse: %v", err)
	}
	return func(name, arch, path string) error {
		return supplychain.VerifyFileWith(m, name, arch, path)
	}
}

// installStubCurl puts a `curl` FIRST on PATH that writes payload to the -o
// target and exits 0 — the "network" for the tests below. The rest of PATH is
// kept so the stub's own `cat` resolves, and the stub is exercised once here
// (the positive control): without it a broken stub would deliver an EMPTY file,
// and every "the gate refused it" assertion below would hold for the wrong
// reason — an empty file fails the checksum too.
func installStubCurl(t *testing.T, payload []byte) {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in -o) shift; out=\"$1\";; esac\n" +
		"  shift\n" +
		"done\n" +
		"cat > \"$out\" <<'STUB_PAYLOAD_EOF'\n" + string(payload) + "STUB_PAYLOAD_EOF\n" +
		"exit 0\n"
	stub := filepath.Join(dir, "curl")
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatalf("write curl stub: %v", err)
	}
	path := dir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)

	// Positive control: the stub must really deliver payload to the -o target.
	probe := filepath.Join(t.TempDir(), "probe-payload")
	cmd := exec.Command(stub, "-fsSL", "-o", probe, "https://example.invalid/probe")
	cmd.Env = []string{"PATH=" + path}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the curl stub failed: %v (output: %s)", err, out)
	}
	got, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("the curl stub wrote nothing: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("the curl stub delivered %q, want %q", got, payload)
	}
}

// TestDownloadRootlessInstaller_RejectsBytesThatDoNotMatchThePin drives the
// PRODUCTION download with the production (default) verifier and a "successful"
// curl that returned bytes which are not the pinned installer. The download
// must fail with the named checksum error and must not leave the file behind:
// a later chmod/chown/execute must have nothing to act on.
func TestDownloadRootlessInstaller_RejectsBytesThatDoNotMatchThePin(t *testing.T) {
	if rootlessInstallerVerifier == nil {
		t.Fatal("the production verifier seam is unset")
	}
	payload := []byte("#!/bin/sh\n# NOT the pinned Docker rootless installer\nexit 0\n")
	installStubCurl(t, payload)
	path := filepath.Join(t.TempDir(), "rootless-install.sh")

	out, err := downloadRootlessInstaller(context.Background(), path)
	if err == nil {
		t.Fatalf("a download that does not match the pin succeeded (output: %s)", out)
	}
	if !errors.Is(err, supplychain.ErrChecksumMismatch) {
		t.Fatalf("error %v does not unwrap to ErrChecksumMismatch", err)
	}
	var mm *supplychain.MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("error %v is not a *supplychain.MismatchError", err)
	}
	if mm.Name != supplychain.ArtifactRootlessInstaller {
		t.Errorf("mismatch names artifact %q, want %q", mm.Name, supplychain.ArtifactRootlessInstaller)
	}
	// The gate hashed the bytes the download actually delivered — not, say, an
	// empty file left by a broken stub, which would carry a different actual
	// digest and make this test pass for the wrong reason.
	sum := sha256.Sum256(payload)
	if want := hex.EncodeToString(sum[:]); mm.Actual != want {
		t.Errorf("mismatch hashed %q, want the sha256 of the delivered payload (%s)", mm.Actual, want)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("an unverified installer was left on disk at %s (stat err = %v)", path, statErr)
	}
}

// TestDownloadRootlessInstaller_VerifierFailureStopsTheDownload proves the gate
// is WIRED into the download (not merely available): a verifier that fails
// aborts the download, is called with the pinned artifact identity and the
// downloaded path, and the bytes are removed.
func TestDownloadRootlessInstaller_VerifierFailureStopsTheDownload(t *testing.T) {
	installStubCurl(t, []byte("#!/bin/sh\n# whatever the network returned\nexit 0\n"))
	path := filepath.Join(t.TempDir(), "rootless-install.sh")

	sentinel := errors.New("fixture gate says no")
	var gotName, gotArch, gotPath string
	calls := 0
	prev := rootlessInstallerVerifier
	rootlessInstallerVerifier = func(name, arch, p string) error {
		calls++
		gotName, gotArch, gotPath = name, arch, p
		return sentinel
	}
	t.Cleanup(func() { rootlessInstallerVerifier = prev })

	_, err := downloadRootlessInstaller(context.Background(), path)
	if calls != 1 {
		t.Fatalf("the gate was called %d times, want exactly 1", calls)
	}
	if gotName != supplychain.ArtifactRootlessInstaller || gotArch != supplychain.ArchAny || gotPath != path {
		t.Errorf("gate called with (%q, %q, %q), want (%q, %q, %q)",
			gotName, gotArch, gotPath, supplychain.ArtifactRootlessInstaller, supplychain.ArchAny, path)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error %v does not wrap the gate's failure", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a download the gate refused was left on disk at %s (stat err = %v)", path, statErr)
	}
}

// TestCachedRootlessInstallerDownload_CacheEntryMustMatchThePin covers the
// cache read, which is the OTHER path that produces a to-be-executed installer.
// Both arms use the real gate logic over a fixture pin:
//
//   - the fixture's own bytes are served from the cache with no download;
//   - one flipped byte is refused with the named error and is NOT silently
//     refetched (a cache that no longer holds the pinned artifact is an event
//     an operator has to see).
func TestCachedRootlessInstallerDownload_CacheEntryMustMatchThePin(t *testing.T) {
	payload := []byte("#!/bin/sh\n# fixture rootless installer\nset -e\necho fixture\nexit 0\n")

	cacheDir := t.TempDir()
	prevDir := rootlessInstallerCacheDir
	SetRootlessInstallerCacheDir(cacheDir)
	t.Cleanup(func() { rootlessInstallerCacheDir = prevDir })

	// The download seam counts calls: a cache hit must not reach it, and a
	// refused cache entry must not silently fall through to it either.
	downloads := 0
	prevDownload := rootlessInstallerDownload
	rootlessInstallerDownload = func(_ context.Context, p string) ([]byte, error) {
		downloads++
		return nil, os.WriteFile(p, payload, 0o644)
	}
	t.Cleanup(func() { rootlessInstallerDownload = prevDownload })

	prevVerify := rootlessInstallerVerifier
	rootlessInstallerVerifier = fixturePinVerifier(t, payload)
	t.Cleanup(func() { rootlessInstallerVerifier = prevVerify })

	cachePath := filepath.Join(cacheDir, rootlessInstallerCacheKey())

	t.Run("matching cache entry is served without a download", func(t *testing.T) {
		downloads = 0
		if err := os.WriteFile(cachePath, payload, 0o644); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
		dst := filepath.Join(t.TempDir(), "installer.sh")
		got, err := cachedRootlessInstallerDownload(context.Background(), dst)
		if err != nil {
			t.Fatalf("cache hit failed: %v", err)
		}
		if downloads != 0 {
			t.Errorf("cache hit reached the network (%d downloads)", downloads)
		}
		if string(got) != string(payload) {
			t.Errorf("cache hit returned %q, want the cached bytes", got)
		}
		if installed, err := os.ReadFile(dst); err != nil || string(installed) != string(payload) {
			t.Errorf("installed bytes = %q (err %v), want the cached bytes", installed, err)
		}
	})

	t.Run("tampered cache entry is refused, not refetched", func(t *testing.T) {
		downloads = 0
		tampered := append([]byte(nil), payload...)
		tampered[len(tampered)-2] = 'X'
		if err := os.WriteFile(cachePath, tampered, 0o644); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
		dst := filepath.Join(t.TempDir(), "installer.sh")
		_, err := cachedRootlessInstallerDownload(context.Background(), dst)
		if err == nil {
			t.Fatal("a cache entry that does not match the pin was accepted")
		}
		if !errors.Is(err, supplychain.ErrChecksumMismatch) {
			t.Fatalf("error %v does not unwrap to ErrChecksumMismatch", err)
		}
		if !strings.Contains(err.Error(), cachePath) {
			t.Errorf("the refusal must name the cache file an operator has to remove: %v", err)
		}
		if downloads != 0 {
			t.Errorf("the refusal silently refetched (%d downloads) instead of surfacing", downloads)
		}
		if _, statErr := os.Stat(dst); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("a refused installer was written to %s (stat err = %v)", dst, statErr)
		}
	})
}

// TestRootlessInstallerCacheKeyCarriesThePin pins the cache-key shape: the
// filename still derives from the artifact's host (so a URL change is a new
// entry) AND carries the pinned digest prefix, so a re-pin cannot be served the
// artifact the previous pin downloaded.
func TestRootlessInstallerCacheKeyCarriesThePin(t *testing.T) {
	key := rootlessInstallerCacheKey()
	tag := rootlessInstallerPinTag()
	if tag == "unpinned" {
		t.Fatal("the embedded manifest pins no installer digest; the cache key cannot track a pin")
	}
	if !strings.Contains(key, tag) {
		t.Errorf("cache key %q does not carry the pin tag %q", key, tag)
	}
	if !strings.Contains(key, hostCacheSuffix(rootlessInstallURL)) {
		t.Errorf("cache key %q no longer derives from the artifact host", key)
	}
	if !strings.HasSuffix(key, ".sh") {
		t.Errorf("cache key %q is not a .sh file name", key)
	}
}
