package supplychain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixturePayload is the synthetic artifact every table-driven verification case
// hashes. Nothing here downloads: the "valid" rows pin the sha256 of these
// bytes, computed in-test, and the mismatch rows offer different bytes.
var fixturePayload = []byte("#!/bin/sh\n# fixture artifact payload\nexit 0\n")

// fixtureSHA is the sha256 of fixturePayload, as the manifest spells it.
func fixtureSHA() string {
	sum := sha256.Sum256(fixturePayload)
	return hex.EncodeToString(sum[:])
}

// fixtureManifest builds a minimal, valid manifest pinning fixturePayload under
// name for the given archs. It is a real *Manifest (parsed through
// ParseManifest, not hand-built) so every test exercises the same validation
// production runs.
func fixtureManifest(t *testing.T, name string, archs ...string) *Manifest {
	t.Helper()
	if len(archs) == 0 {
		archs = []string{ArchAny}
	}
	var variants []string
	for _, a := range archs {
		variants = append(variants, `{"arch":"`+a+`","url":"https://example.invalid/v1/`+a+`","sha256":"`+fixtureSHA()+`"}`)
	}
	raw := `{"schema_version":1,"artifacts":[{"name":"` + name + `","version":"v9.9.9","note":"fixture","variants":[` +
		strings.Join(variants, ",") + `]}],"images":[]}`
	m, err := ParseManifest([]byte(raw))
	if err != nil {
		t.Fatalf("fixture manifest does not parse: %v", err)
	}
	return m
}

// ─────────────────────────────────────────────────────────────────────────────
// Checksum verification (GAP-135 criterion 4): valid, mismatch, missing entry
// ─────────────────────────────────────────────────────────────────────────────

// TestVerifyChecksumPath_Table is the required table for the verification path:
// pinned bytes pass, tampered bytes fail with the named mismatch error, an
// artifact the manifest does not pin fails with the named not-pinned error, and
// so does an architecture the manifest does not pin. Every row reads the bytes
// through BOTH entry points (file and in-memory) so the two paths cannot drift.
func TestVerifyChecksumPath_Table(t *testing.T) {
	const name = "fixture-artifact"

	// tampered flips one byte of the payload: same length, different digest.
	tampered := append([]byte(nil), fixturePayload...)
	tampered[len(tampered)-2] = 'X'

	tests := []struct {
		name string
		// manifest is the pin table under test (nil = the caller builds none).
		manifest *Manifest
		// arch is the architecture asked for.
		arch string
		// payload is the artifact name the caller verifies.
		artifact string
		// data is the offered payload.
		data []byte
		// wantErr is the sentinel the failure must unwrap to (nil = expect success).
		wantErr error
	}{
		{
			name:     "valid: exact pinned bytes in memory",
			manifest: fixtureManifest(t, name),
			arch:     ArchAny,
			artifact: name,
			data:     fixturePayload,
		},
		{
			name:     "valid: arch asked for is served by the any-variant",
			manifest: fixtureManifest(t, name),
			arch:     "x86_64",
			artifact: name,
			data:     fixturePayload,
		},
		{
			name:     "valid: exact-arch variant wins over any",
			manifest: fixtureManifest(t, name, "x86_64", ArchAny),
			arch:     "x86_64",
			artifact: name,
			data:     fixturePayload,
		},
		{
			name:     "mismatch: one byte changed",
			manifest: fixtureManifest(t, name),
			arch:     ArchAny,
			artifact: name,
			data:     tampered,
			wantErr:  ErrChecksumMismatch,
		},
		{
			name:     "mismatch: truncated payload",
			manifest: fixtureManifest(t, name),
			arch:     ArchAny,
			artifact: name,
			data:     fixturePayload[:len(fixturePayload)-1],
			wantErr:  ErrChecksumMismatch,
		},
		{
			name:     "mismatch: empty payload",
			manifest: fixtureManifest(t, name),
			arch:     ArchAny,
			artifact: name,
			data:     nil,
			wantErr:  ErrChecksumMismatch,
		},
		{
			name:     "missing manifest entry: unknown artifact name",
			manifest: fixtureManifest(t, name),
			arch:     ArchAny,
			artifact: "not-in-the-manifest",
			data:     fixturePayload,
			wantErr:  ErrNotPinned,
		},
		{
			name:     "missing manifest entry: pinned name, unpinned architecture",
			manifest: fixtureManifest(t, name, "x86_64"),
			arch:     "aarch64",
			artifact: name,
			data:     fixturePayload,
			wantErr:  ErrNotPinned,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// In-memory path.
			errBytes := VerifyBytesWith(tt.manifest, tt.artifact, tt.arch, tt.data)
			checkVerifyErr(t, "VerifyBytesWith", errBytes, tt.wantErr)

			// File path: the same bytes, read from disk.
			path := filepath.Join(t.TempDir(), "payload")
			if err := os.WriteFile(path, tt.data, 0o600); err != nil {
				t.Fatalf("write payload: %v", err)
			}
			errFile := VerifyFileWith(tt.manifest, tt.artifact, tt.arch, path)
			checkVerifyErr(t, "VerifyFileWith", errFile, tt.wantErr)

			if tt.wantErr == nil {
				return
			}
			// The named error must carry the facts an operator needs, and the
			// failure must read as the same class through both entry points.
			if tt.wantErr == ErrChecksumMismatch {
				var mm *MismatchError
				if !errors.As(errBytes, &mm) {
					t.Fatalf("mismatch is not a *MismatchError: %#v", errBytes)
				}
				if mm.Expected != fixtureSHA() {
					t.Errorf("MismatchError.Expected = %q, want the pinned sha256 %q", mm.Expected, fixtureSHA())
				}
				if mm.Actual == mm.Expected {
					t.Errorf("MismatchError carried no actual digest: %+v", mm)
				}
				if mm.Version != "v9.9.9" {
					t.Errorf("MismatchError.Version = %q, want the pinned version", mm.Version)
				}
				if !strings.Contains(mm.Error(), "refusing to use it") {
					t.Errorf("mismatch text must refuse the bytes: %q", mm.Error())
				}
				// The FILE path must name the file; the in-memory path has no
				// file to name.
				var mmFile *MismatchError
				if !errors.As(errFile, &mmFile) {
					t.Fatalf("file-path mismatch is not a *MismatchError: %#v", errFile)
				}
				if mmFile.Path != path {
					t.Errorf("file-path mismatch did not name the file: Path = %q, want %q", mmFile.Path, path)
				}
				if mmFile.Expected != mm.Expected || mmFile.Actual != mm.Actual {
					t.Errorf("file and byte paths disagree on the digests:\n file:  %+v\n bytes: %+v", mmFile, mm)
				}
			}
			var np *NotPinnedError
			if tt.wantErr == ErrNotPinned {
				if !errors.As(errBytes, &np) {
					t.Fatalf("not-pinned is not a *NotPinnedError: %#v", errBytes)
				}
				if len(np.Known) == 0 || np.Known[0] != name {
					t.Errorf("NotPinnedError should list the pinned names, got %v", np.Known)
				}
			}
		})
	}
}

// checkVerifyErr asserts the expected class, and that a success carries no
// error at all.
func checkVerifyErr(t *testing.T, via string, err, wantErr error) {
	t.Helper()
	if wantErr == nil {
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", via, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("%s: expected %v, got nil", via, wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("%s: error %v does not unwrap to %v", via, err, wantErr)
	}
}

// TestVerifyFile_MissingFileIsNotAMismatch pins the distinction the install
// paths depend on: a file that is not there is an I/O failure, not a tamper
// report — but it still fails closed.
func TestVerifyFile_MissingFileIsNotAMismatch(t *testing.T) {
	m := fixtureManifest(t, "fixture-artifact")
	err := VerifyFileWith(m, "fixture-artifact", ArchAny, filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if errors.Is(err, ErrChecksumMismatch) || errors.Is(err, ErrNotPinned) {
		t.Fatalf("missing file should not be reported as a pin failure: %v", err)
	}
}

// TestVerify_NilManifestFailsClosed guards the "no manifest" path: it must be an
// error, never a pass.
func TestVerify_NilManifestFailsClosed(t *testing.T) {
	if err := VerifyBytesWith(nil, "fixture-artifact", ArchAny, fixturePayload); err == nil {
		t.Fatal("a nil manifest must fail verification, not accept the bytes")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Manifest parsing / validation
// ─────────────────────────────────────────────────────────────────────────────

func TestParseManifest_RejectsBadShapes(t *testing.T) {
	valid := `{"schema_version":1,"artifacts":[{"name":"a","version":"v1","note":"n","variants":[{"arch":"any","url":"https://example.invalid/v1/a","sha256":"` + fixtureSHA() + `"}]}],"images":[{"reference":"docker.io/library/ubuntu:24.04","digest":"sha256:` + fixtureSHA() + `","note":"n"}]}`

	tests := []struct {
		name string
		raw  string
		// want is a substring the refusal must name, so the error is actionable.
		want string
	}{
		{name: "valid fixture parses", raw: valid},
		{
			name: "unknown field is refused",
			raw:  strings.Replace(valid, `"version":"v1"`, `"sha265":"`+fixtureSHA()+`","version":"v1"`, 1),
			want: "unknown field",
		},
		{
			name: "schema version mismatch",
			raw:  strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1),
			want: "schema_version",
		},
		{
			name: "no artifacts",
			raw:  `{"schema_version":1,"artifacts":[]}`,
			want: "pins no artifacts",
		},
		{
			name: "artifact without a name",
			raw:  strings.Replace(valid, `"name":"a",`, ``, 1),
			want: "no name",
		},
		{
			name: "duplicate artifact name",
			raw: strings.Replace(valid, `],"images"`,
				`,{"name":"a","version":"v2","note":"n","variants":[{"arch":"any","url":"https://example.invalid/v2/a","sha256":"`+fixtureSHA()+`"}]}],"images"`, 1),
			want: "twice",
		},
		{
			name: "artifact without a version",
			raw:  strings.Replace(valid, `"version":"v1",`, ``, 1),
			want: "no version",
		},
		{
			name: "artifact without variants",
			raw:  `{"schema_version":1,"artifacts":[{"name":"a","version":"v1","variants":[]}]}`,
			want: "no variants",
		},
		{
			name: "variant without an arch",
			raw:  strings.Replace(valid, `"arch":"any",`, ``, 1),
			want: "no arch",
		},
		{
			name: "duplicate arch",
			raw: strings.Replace(valid, `"sha256":"`+fixtureSHA()+`"}]}`,
				`"sha256":"`+fixtureSHA()+`"},{"arch":"any","url":"https://example.invalid/v2/a","sha256":"`+fixtureSHA()+`"}]}`, 1),
			want: "twice",
		},
		{
			name: "url is not https",
			raw:  strings.Replace(valid, "https://example.invalid", "http://example.invalid", 1),
			want: "not https",
		},
		{
			name: "url resolves latest",
			raw:  strings.Replace(valid, "https://example.invalid/v1/a", "https://example.invalid/releases/latest/download/a", 1),
			want: "resolves latest",
		},
		{
			name: "sha256 too short",
			raw:  strings.Replace(valid, `"sha256":"`+fixtureSHA()+`"`, `"sha256":"deadbeef"`, 1),
			want: "64 lowercase hex",
		},
		{
			name: "sha256 uppercase is refused (digests are compared as hex)",
			raw:  strings.Replace(valid, `"sha256":"`+fixtureSHA()+`"`, `"sha256":"`+strings.ToUpper(fixtureSHA())+`"`, 1),
			want: "64 lowercase hex",
		},
		{
			name: "image digest without the algorithm prefix",
			raw:  strings.Replace(valid, `"digest":"sha256:`, `"digest":"`, 1),
			want: "digest",
		},
		{
			name: "duplicate image reference",
			raw: strings.Replace(valid, `"images":[`,
				`"images":[{"reference":"docker.io/library/ubuntu:24.04","digest":"sha256:`+fixtureSHA()+`","note":"n"},`, 1),
			want: "twice",
		},
		{
			name: "trailing data",
			raw:  valid + `{}`,
			want: "trailing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tt.raw))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ParseManifest(%s) = %v, want success", tt.raw, err)
				}
				if m.SchemaVersion != ManifestSchemaVersion || len(m.Artifacts) != 1 {
					t.Fatalf("parsed manifest = %+v, want one artifact at schema %d", m, ManifestSchemaVersion)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseManifest(%s) = nil error, want a refusal naming %q", tt.raw, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("refusal %q does not name %q", err.Error(), tt.want)
			}
			if m != nil {
				t.Errorf("a refused manifest must not be returned: %+v", m)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The repository's own pins
// ─────────────────────────────────────────────────────────────────────────────

// TestEmbeddedManifest_IsValidAndCoversProductionLookups is the gate that makes
// every production lookup a typo-proof compile-time-ish check: the embedded
// manifest must parse, and every artifact constant the install paths resolve
// must be pinned for the architectures those paths ask for.
func TestEmbeddedManifest_IsValidAndCoversProductionLookups(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatalf("the embedded manifest does not parse: %v", err)
	}

	for _, name := range []string{ArtifactRootlessInstaller, ArtifactSlirp4netns, ArtifactFuseOverlayfs} {
		if _, ok := m.Artifact(name); !ok {
			t.Errorf("embedded manifest pins no artifact %q (known: %v)", name, m.ArtifactNames())
		}
	}

	// The install script is architecture-independent: it must be reachable
	// from EVERY architecture the prereq path asks for.
	for _, arch := range []string{ArchAny, "x86_64", "aarch64"} {
		v, ok := m.Variant(ArtifactRootlessInstaller, arch)
		if !ok {
			t.Errorf("rootless installer is not pinned for arch %s", arch)
			continue
		}
		if v.Arch != ArchAny {
			t.Errorf("rootless installer variant for %s resolved to arch %q, want %q", arch, v.Arch, ArchAny)
		}
		if !hex64.MatchString(v.SHA256) {
			t.Errorf("rootless installer pin for %s is %q, want 64 lowercase hex", arch, v.SHA256)
		}
	}

	// The prebuilt binaries are architecture-specific: each must carry both
	// architectures the installer requests, with DIFFERENT digests (a shared
	// digest across architectures would mean one of them is the wrong file).
	for _, name := range []string{ArtifactSlirp4netns, ArtifactFuseOverlayfs} {
		x86, okX := m.Variant(name, "x86_64")
		arm, okA := m.Variant(name, "aarch64")
		if !okX || !okA {
			t.Errorf("%s is not pinned for both x86_64 and aarch64", name)
			continue
		}
		if x86.SHA256 == arm.SHA256 {
			t.Errorf("%s pins the same digest for x86_64 and aarch64 (%s)", name, x86.SHA256)
		}
		for _, v := range []Variant{x86, arm} {
			if !strings.Contains(v.URL, "/download/") {
				t.Errorf("%s url %q is not a pinned release asset", name, v.URL)
			}
		}
	}

	// Ordering of ArtifactNames is part of the operator-facing error text.
	names := m.ArtifactNames()
	if !sort.StringsAreSorted(names) {
		t.Errorf("ArtifactNames() must be sorted, got %v", names)
	}
	if len(m.Images) == 0 {
		t.Fatal("embedded manifest pins no images")
	}
}

// TestPinnedImageRef pins the FROM-line resolution and its fail-closed half: an
// unknown reference resolves to nothing (the caller then refuses), never to the
// bare tag silently.
func TestPinnedImageRef(t *testing.T) {
	ref, ok := PinnedImageRef("docker.io/library/ubuntu:24.04")
	if !ok {
		t.Fatal("docker.io/library/ubuntu:24.04 is not pinned")
	}
	wantPrefix := "docker.io/library/ubuntu@sha256:"
	if !strings.HasPrefix(ref, wantPrefix) {
		t.Errorf("PinnedImageRef = %q, want the %q form", ref, wantPrefix)
	}
	if strings.Contains(ref, ":24.04@") {
		t.Errorf("pinned reference must be the digest form, got %q", ref)
	}

	if _, ok := PinnedImageRef("docker.io/library/ubuntu:26.10"); ok {
		t.Error("an unpinned reference resolved; it must fail closed")
	}
}

// TestArtifactLookupsFailClosed pins the two named lookups the install paths
// call: pinned names resolve to the manifest's URL/version, unknown ones return
// the named not-pinned error.
func TestArtifactLookupsFailClosed(t *testing.T) {
	url, err := ArtifactURL(ArtifactSlirp4netns, "x86_64")
	if err != nil {
		t.Fatalf("ArtifactURL(slirp4netns, x86_64) = %v", err)
	}
	if !strings.Contains(url, "/v1.3.6/") {
		t.Errorf("slirp4netns url %q does not name the pinned version", url)
	}
	if strings.Contains(url, "/latest/") {
		t.Errorf("slirp4netns url %q still resolves latest", url)
	}
	sum, err := ArtifactDigest(ArtifactSlirp4netns, "x86_64")
	if err != nil {
		t.Fatalf("ArtifactDigest(slirp4netns, x86_64) = %v", err)
	}
	if !hex64.MatchString(sum) {
		t.Errorf("ArtifactDigest = %q, want 64 lowercase hex", sum)
	}

	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{"ArtifactURL", func() error { _, err := ArtifactURL("no-such-artifact", "x86_64"); return err }},
		{"ArtifactDigest", func() error { _, err := ArtifactDigest(ArtifactSlirpnetnsTypo, "x86_64"); return err }},
		{"ArtifactURL bad arch", func() error { _, err := ArtifactURL(ArtifactFuseOverlayfs, "riscv64"); return err }},
	} {
		err := call.fn()
		if err == nil {
			t.Errorf("%s: expected the not-pinned error", call.name)
			continue
		}
		if !errors.Is(err, ErrNotPinned) {
			t.Errorf("%s: %v does not unwrap to ErrNotPinned", call.name, err)
		}
	}
}

// ArtifactSlirpnetnsTypo is a deliberate misspelling of ArtifactSlirp4netns,
// used to prove a lookup typo fails loudly instead of resolving to "".
const ArtifactSlirpnetnsTypo = "slirp4netn"

// TestLiveArtifactPins_MatchUpstream re-resolves every pinned artifact from its
// recorded URL and compares the bytes to the pin. It is the UPGRADE-PATH PROBE:
// it is skipped by default (no test-time downloads) and is the exact check to
// run after re-pinning, or as a periodic tamper audit:
//
//	BUNKER_LIVE_PIN_PROBE=1 go test ./internal/supplychain/ -run LiveArtifact -v
//
// liveness: a pin that no longer matches upstream is a FAILURE here, not a
// silent re-pin — the manifest is the contract, and re-pinning is a reviewed
// edit (docs/supply-chain-pinning.md).
func TestLiveArtifactPins_MatchUpstream(t *testing.T) {
	if os.Getenv("BUNKER_LIVE_PIN_PROBE") == "" {
		t.Skip("live probe: set BUNKER_LIVE_PIN_PROBE=1 to re-resolve the pins against upstream")
	}
	m, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	for _, a := range m.Artifacts {
		for _, v := range a.Variants {
			t.Run(a.Name+"/"+v.Arch, func(t *testing.T) {
				resp, err := client.Get(v.URL)
				if err != nil {
					t.Fatalf("GET %s: %v", v.URL, err)
				}
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("GET %s: HTTP %d", v.URL, resp.StatusCode)
				}
				h := sha256.New()
				n, err := io.Copy(h, resp.Body)
				if err != nil {
					t.Fatalf("read %s: %v", v.URL, err)
				}
				if got := hex.EncodeToString(h.Sum(nil)); got != v.SHA256 {
					t.Errorf("%s (arch %s): upstream sha256 %s != pinned %s (%d bytes)",
						a.Name, v.Arch, got, v.SHA256, n)
				}
			})
		}
	}
}
