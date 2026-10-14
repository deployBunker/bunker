// Package supplychain holds the repository's supply-chain pins: the exact
// version and sha256 of every artifact the installers DOWNLOAD, and the digest
// of every container base image the platform builds FROM.
//
// WHY THIS EXISTS (GAP-135). The rootless install path used to fetch its
// inputs by MOVING URLs and then execute them:
//
//   - https://get.docker.com/rootless — the official install script, fetched
//     and then run as the agent user (it writes executables into ~/bin);
//   - https://github.com/<project>/releases/latest/download/<binary> — prebuilt
//     slirp4netns / fuse-overlayfs binaries, downloaded and installed to
//     /usr/local/bin.
//
// releases/latest/download is not a version: whatever upstream published most
// recently is what the host installed, unverified, and there was no record of
// what that was. The base images had the same shape — a tag is a mutable
// pointer, so `docker.io/library/ubuntu:24.04` resolved to whatever the tag
// pointed at on the day of the build.
//
// The manifest (manifest.json, embedded so a `go install` binary carries it) is
// the CONTRACT: one row per artifact with its version, its URL and its sha256;
// one row per base image with its digest. Every consumer verifies before it
// executes or extracts, and a mismatch is a NAMED error
// (MismatchError / ErrChecksumMismatch) — never a warning, never a fallback to
// the unverified bytes.
//
// Drift is reported rather than absorbed: this package only answers "what is
// pinned" and "do these bytes match"; it never re-resolves a moving URL.
// Re-pinning is a deliberate, reviewed edit, documented in
// docs/supply-chain-pinning.md.
package supplychain

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

//go:embed manifest.json
var embeddedManifest []byte

// ManifestSchemaVersion is the only manifest schema this package understands.
// A manifest declaring another version is refused: guessing at a newer shape
// would mean verifying with rules nobody wrote.
const ManifestSchemaVersion = 1

// ArchAny marks a variant that is architecture-independent (a shell script).
// A lookup for any architecture accepts an ArchAny variant.
const ArchAny = "any"

// Artifact names for the pins the Go code looks up. They are constants so a
// caller cannot misspell a manifest name into an "unpinned" runtime failure —
// the pin-coverage test walks these against the embedded manifest.
const (
	// ArtifactRootlessInstaller is the official Docker rootless install
	// script. Its bytes decide the docker-ce-rootless-extras archive that
	// then supplies dockerd-rootless.sh, dockerd-rootless-setuptool.sh and
	// rootlesskit.
	ArtifactRootlessInstaller = "rootless-installer"
	// ArtifactSlirp4netns is the static slirp4netns binary used when the
	// host's distribution has no package for it.
	ArtifactSlirp4netns = "slirp4netns"
	// ArtifactFuseOverlayfs is the static fuse-overlayfs binary used when the
	// host's distribution has no package for it.
	ArtifactFuseOverlayfs = "fuse-overlayfs"
)

// DigestPrefix is the algorithm prefix every image digest carries.
const DigestPrefix = "sha256:"

// ErrNotPinned is the sentinel for "the manifest has no pin for this
// artifact/architecture". Callers test it with errors.Is; the concrete
// NotPinnedError names what WAS pinned so the operator can fix the typo or add
// the row.
var ErrNotPinned = errors.New("supply-chain manifest has no pin for this artifact")

// ErrChecksumMismatch is the sentinel for "these bytes are not the pinned
// bytes". The concrete MismatchError carries both digests. Nothing may be
// executed, extracted or installed after this error.
var ErrChecksumMismatch = errors.New("supply-chain checksum mismatch")

// Artifact is one downloadable artifact: a name, the upstream version the pin
// was resolved from, and one variant per architecture.
type Artifact struct {
	// Name is the lookup key (see the Artifact* constants).
	Name string `json:"name"`
	// Version is the upstream version the pin was resolved from. For the
	// rootless install script it is the docker-install commit the published
	// bytes were built from, because the script itself has no version.
	Version string `json:"version"`
	// Note is operator-facing context: provenance, coverage, why this pin.
	Note string `json:"note"`
	// Variants holds one entry per architecture, or a single ArchAny entry
	// for an architecture-independent artifact.
	Variants []Variant `json:"variants"`
}

// Variant is one architecture's download of an artifact.
type Variant struct {
	// Arch is the architecture token the install path asks for (x86_64,
	// aarch64), or ArchAny.
	Arch string `json:"arch"`
	// URL is the exact, immutable download URL. A URL that resolves latest
	// is refused by validation (see validateURL).
	URL string `json:"url"`
	// SHA256 is the lowercase hex sha256 of the bytes at URL.
	SHA256 string `json:"sha256"`
}

// Image is one pinned container base image.
type Image struct {
	// Reference is the mutable human-facing name:tag the spec names.
	Reference string `json:"reference"`
	// Digest is the immutable sha256 digest of the image manifest (list) the
	// tag resolved to at pin time. FROM lines render Reference@Digest.
	Digest string `json:"digest"`
	// Note is operator-facing context.
	Note string `json:"note"`
}

// Manifest is the parsed manifest.
type Manifest struct {
	SchemaVersion int        `json:"schema_version"`
	Artifacts     []Artifact `json:"artifacts"`
	Images        []Image    `json:"images"`
}

// NotPinnedError reports a lookup for an artifact (or architecture) the
// manifest does not pin. It is never a soft failure: an unpinned artifact
// cannot be verified, so it cannot be used.
type NotPinnedError struct {
	// Name is the artifact name that was asked for.
	Name string
	// Arch is the architecture that was asked for.
	Arch string
	// Known lists the artifact names the manifest DOES pin, so the error text
	// is actionable without reading the file.
	Known []string
}

func (e *NotPinnedError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("%s: artifact %q (arch %s)", ErrNotPinned, e.Name, e.Arch)
	}
	return fmt.Sprintf("%s: artifact %q (arch %s): pinned artifacts are %s",
		ErrNotPinned, e.Name, e.Arch, strings.Join(e.Known, ", "))
}

// Unwrap makes errors.Is(err, ErrNotPinned) true.
func (e *NotPinnedError) Unwrap() error { return ErrNotPinned }

// MismatchError reports bytes that are not the pinned bytes.
type MismatchError struct {
	// Name is the artifact name.
	Name string
	// Version is the pinned upstream version.
	Version string
	// Arch is the architecture the pin was resolved for.
	Arch string
	// Path is the file that failed verification ("" for byte-slice checks).
	Path string
	// Expected is the pinned sha256 (lowercase hex).
	Expected string
	// Actual is the sha256 of the bytes that were offered (lowercase hex).
	Actual string
}

func (e *MismatchError) Error() string {
	at := "for " + e.Name + " " + e.Version
	if e.Path != "" {
		at = e.Path + " (" + at + ")"
	}
	return fmt.Sprintf("%s %s: expected sha256 %s (arch %s), got %s — refusing to use it",
		ErrChecksumMismatch, at, e.Expected, e.Arch, e.Actual)
}

// Unwrap makes errors.Is(err, ErrChecksumMismatch) true.
func (e *MismatchError) Unwrap() error { return ErrChecksumMismatch }

// VerifyFn checks a downloaded file against a pin. It is the shape the install
// paths inject, so a test can pin a fixture without touching the embedded
// manifest.
type VerifyFn func(name, arch, path string) error

// hex64 matches a lowercase sha256 digest value.
var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Load parses the embedded manifest. A manifest that does not parse makes every
// verification FAIL (callers surface the error); it never silently becomes "no
// pins" or "all bytes accepted".
func Load() (*Manifest, error) {
	return ParseManifest(embeddedManifest)
}

// ParseManifest validates raw manifest bytes. It is total: any error means no
// part of the input was accepted.
//
// Validation is deliberately strict, because a lenient parse is a silent
// supply-chain downgrade: unknown fields are refused (a typo'd "sha265" must
// not read as "no digest required"), every artifact must carry a version and at
// least one variant, every variant must carry an https URL and a 64-hex sha256,
// no artifact may declare the same architecture twice, and no two images may
// share a reference.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse supply-chain manifest: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse supply-chain manifest: unexpected trailing data")
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		return nil, fmt.Errorf("supply-chain manifest schema_version is %d, this build understands %d",
			m.SchemaVersion, ManifestSchemaVersion)
	}
	if len(m.Artifacts) == 0 {
		return nil, errors.New("supply-chain manifest pins no artifacts")
	}

	seenArtifact := make(map[string]bool, len(m.Artifacts))
	for i, a := range m.Artifacts {
		if a.Name == "" {
			return nil, fmt.Errorf("supply-chain manifest artifact #%d has no name", i+1)
		}
		if seenArtifact[a.Name] {
			return nil, fmt.Errorf("supply-chain manifest pins artifact %q twice", a.Name)
		}
		seenArtifact[a.Name] = true
		if a.Version == "" {
			return nil, fmt.Errorf("supply-chain manifest artifact %q pins no version", a.Name)
		}
		if len(a.Variants) == 0 {
			return nil, fmt.Errorf("supply-chain manifest artifact %q pins no variants", a.Name)
		}
		seenArch := make(map[string]bool, len(a.Variants))
		for _, v := range a.Variants {
			if v.Arch == "" {
				return nil, fmt.Errorf("supply-chain manifest artifact %q has a variant with no arch", a.Name)
			}
			if seenArch[v.Arch] {
				return nil, fmt.Errorf("supply-chain manifest artifact %q pins arch %q twice", a.Name, v.Arch)
			}
			seenArch[v.Arch] = true
			if err := validateURL(a.Name, v.Arch, v.URL); err != nil {
				return nil, err
			}
			if !hex64.MatchString(v.SHA256) {
				return nil, fmt.Errorf("supply-chain manifest artifact %q (arch %s) has sha256 %q, want 64 lowercase hex characters",
					a.Name, v.Arch, v.SHA256)
			}
		}
	}

	seenImage := make(map[string]bool, len(m.Images))
	for i, img := range m.Images {
		if img.Reference == "" {
			return nil, fmt.Errorf("supply-chain manifest image #%d has no reference", i+1)
		}
		if seenImage[img.Reference] {
			return nil, fmt.Errorf("supply-chain manifest pins image %q twice", img.Reference)
		}
		seenImage[img.Reference] = true
		if !strings.HasPrefix(img.Digest, DigestPrefix) ||
			!hex64.MatchString(strings.TrimPrefix(img.Digest, DigestPrefix)) {
			return nil, fmt.Errorf("supply-chain manifest image %q has digest %q, want %s followed by 64 lowercase hex characters",
				img.Reference, img.Digest, DigestPrefix)
		}
	}
	return &m, nil
}

// validateURL refuses anything that is not an immutable https download. `latest`
// paths are refused by name because they are the exact class of moving URL this
// package exists to eliminate.
func validateURL(name, arch, url string) error {
	switch {
	case url == "":
		return fmt.Errorf("supply-chain manifest artifact %q (arch %s) has no url", name, arch)
	case !strings.HasPrefix(url, "https://"):
		return fmt.Errorf("supply-chain manifest artifact %q (arch %s) url %q is not https", name, arch, url)
	case strings.Contains(url, "/latest/"):
		return fmt.Errorf("supply-chain manifest artifact %q (arch %s) url %q resolves latest; pin a release instead", name, arch, url)
	}
	return nil
}

// ArtifactNames returns the sorted artifact names the embedded manifest pins.
func (m *Manifest) ArtifactNames() []string {
	out := make([]string, 0, len(m.Artifacts))
	for _, a := range m.Artifacts {
		out = append(out, a.Name)
	}
	sort.Strings(out)
	return out
}

// Artifact returns the named artifact.
func (m *Manifest) Artifact(name string) (Artifact, bool) {
	for _, a := range m.Artifacts {
		if a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// Variant returns the pin for name on arch: the exact-architecture variant when
// one exists, otherwise an ArchAny variant (the artifact is the same bytes
// everywhere).
func (m *Manifest) Variant(name, arch string) (Variant, bool) {
	a, ok := m.Artifact(name)
	if !ok {
		return Variant{}, false
	}
	for _, v := range a.Variants {
		if v.Arch == arch {
			return v, true
		}
	}
	for _, v := range a.Variants {
		if v.Arch == ArchAny {
			return v, true
		}
	}
	return Variant{}, false
}

// ImageDigest returns the pinned digest for a base-image reference.
func (m *Manifest) ImageDigest(reference string) (string, bool) {
	for _, img := range m.Images {
		if img.Reference == reference {
			return img.Digest, true
		}
	}
	return "", false
}

// Embedded exposes the pinned lookups for production callers that have no
// manifest of their own. Every helper below fails closed: an unparseable
// embedded manifest is an error, never an "unpinned so allow" path.

// ArtifactURL returns the pinned download URL for name on arch.
func ArtifactURL(name, arch string) (string, error) {
	m, err := Load()
	if err != nil {
		return "", err
	}
	v, ok := m.Variant(name, arch)
	if !ok {
		return "", &NotPinnedError{Name: name, Arch: arch, Known: m.ArtifactNames()}
	}
	return v.URL, nil
}

// ArtifactDigest returns the pinned sha256 for name on arch.
func ArtifactDigest(name, arch string) (string, error) {
	m, err := Load()
	if err != nil {
		return "", err
	}
	v, ok := m.Variant(name, arch)
	if !ok {
		return "", &NotPinnedError{Name: name, Arch: arch, Known: m.ArtifactNames()}
	}
	return v.SHA256, nil
}

// PinnedImageRef resolves a base-image reference (name:tag) to the immutable
// form a FROM line must carry: name@sha256:<digest>.
func PinnedImageRef(reference string) (string, bool) {
	m, err := Load()
	if err != nil {
		return "", false
	}
	digest, ok := m.ImageDigest(reference)
	if !ok {
		return "", false
	}
	name := strings.SplitN(reference, ":", 2)[0]
	return name + "@" + digest, true
}

// VerifyFile checks the file at path against the embedded pin for name/arch.
func VerifyFile(name, arch, path string) error {
	m, err := Load()
	if err != nil {
		return err
	}
	return VerifyFileWith(m, name, arch, path)
}

// VerifyFileWith checks the file at path against the given manifest. It streams
// the file: the artifacts here are megabytes, and hashing must not depend on
// reading them into memory.
func VerifyFileWith(m *Manifest, name, arch, path string) error {
	v, version, err := pinned(m, name, arch)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("verify %s: open %s: %w", name, path, err)
	}
	defer func() { _ = f.Close() }()
	return matchStream(name, version, path, v, f)
}

// VerifyBytesWith checks in-memory bytes against the given manifest. The
// install path uses it for fixtures and small payloads.
func VerifyBytesWith(m *Manifest, name, arch string, data []byte) error {
	v, version, err := pinned(m, name, arch)
	if err != nil {
		return err
	}
	return matchStream(name, version, "", v, bytes.NewReader(data))
}

// pinned resolves the (variant, version) pair for name/arch.
func pinned(m *Manifest, name, arch string) (Variant, string, error) {
	if m == nil {
		return Variant{}, "", errors.New("verify: no supply-chain manifest")
	}
	v, ok := m.Variant(name, arch)
	if !ok {
		return Variant{}, "", &NotPinnedError{Name: name, Arch: arch, Known: m.ArtifactNames()}
	}
	a, _ := m.Artifact(name)
	return v, a.Version, nil
}

// matchStream hashes r and compares it to the pin.
func matchStream(name, version, path string, v Variant, r io.Reader) error {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("verify %s: hash %s: %w", name, path, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != v.SHA256 {
		return &MismatchError{
			Name:     name,
			Version:  version,
			Arch:     v.Arch,
			Path:     path,
			Expected: v.SHA256,
			Actual:   got,
		}
	}
	return nil
}
