package imagespec

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/supplychain"
)

// This file pins GAP-135's image half: the FROM line the builder writes must
// carry the base image's DIGEST, not its tag, and every base in the allowed set
// must be pinned (an allowed-but-unpinned base would pull a mutable tag, which
// is the defect — `ubuntu:24.04` resolved to whatever the tag pointed at on the
// day of the build, so two builds of one spec were not the same image).
//
// SPLIT OF RESPONSIBILITY, deliberately: the goldens in this package
// (dockerfile_golden_test.go, go_toolchain_test.go, stock_toolchain_test.go,
// imagespec_test.go, registry_test.go, delivered_tools_test.go) all build their
// expected FROM line through pinnedFrom below. They pin the RENDER — line
// order, layer content, quoting — and must not have to be edited every time a
// digest is re-pinned. The pin VALUE itself is pinned by the tests in THIS
// file, against internal/supplychain, so a re-pin that is not a deliberate,
// reviewed edit still fails the suite.

// pinnedFrom is the FROM line the render produces for a base:
// "FROM <pinned reference>\n". A re-pin changes it everywhere at once, which is
// the point — the goldens must not silently keep asserting the old digest.
func pinnedFrom(base string) string { return "FROM " + pinnedBaseRef(base) + "\n" }

// TestEveryAllowedBaseIsDigestPinned is the gate that makes the parsed-spec
// guarantee real: parseFromWire refuses a base the manifest does not pin, so
// this test is what keeps that refusal from ever firing on a legitimate base.
// It also fails the build if the allow-list and the manifest drift apart.
func TestEveryAllowedBaseIsDigestPinned(t *testing.T) {
	bases := AllowedBases()
	if len(bases) == 0 {
		t.Fatal("the allowed-base set is empty")
	}
	for _, base := range bases {
		ref, ok := supplychain.PinnedImageRef(base)
		if !ok {
			t.Errorf("allowed base %q is not pinned in the supply-chain manifest — Parse would refuse it, and the builder would have no immutable reference", base)
			continue
		}
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("pinned reference for %q is %q, want the name@sha256:<digest> form", base, ref)
		}
		// The reference must still name the repository the spec named: the
		// pin replaces the tag, never the image.
		repo := strings.SplitN(base, ":", 2)[0]
		if !strings.HasPrefix(ref, repo+"@") {
			t.Errorf("pinned reference for %q is %q, want it to address %q", base, ref, repo)
		}
	}
}

// TestDockerfileFromIsDigestPinned asserts the rendered FROM line, per allowed
// base: it carries the digest, it is exactly the manifest's reference, and the
// mutable tag-only form is NOT what docker is handed.
func TestDockerfileFromIsDigestPinned(t *testing.T) {
	for _, base := range AllowedBases() {
		t.Run(base, func(t *testing.T) {
			raw := `{"base":"` + base + `"}`
			spec, err := Parse([]byte(raw))
			if err != nil {
				t.Fatalf("Parse(%s): %v", raw, err)
			}
			if spec.Base != base {
				t.Fatalf("spec.Base = %q, want the named tag %q (the tag is the operator's handle)", spec.Base, base)
			}

			from, _, _ := strings.Cut(spec.Dockerfile(), "\n")
			wantRef, ok := supplychain.PinnedImageRef(base)
			if !ok {
				t.Fatalf("no pin for %q", base)
			}
			if from != "FROM "+wantRef {
				t.Fatalf("FROM line = %q, want %q", from, "FROM "+wantRef)
			}
			digest := strings.TrimPrefix(wantRef, strings.SplitN(base, ":", 2)[0]+"@")
			if len(digest) != len("sha256:")+64 {
				t.Errorf("digest %q from the FROM line is not sha256:<64 hex>", digest)
			}
			// The tag must not survive as the thing docker resolves.
			if from == "FROM "+spec.Base {
				t.Errorf("FROM line still hands docker the mutable tag %q", spec.Base)
			}
			if !strings.Contains(from, "@"+supplychain.DigestPrefix) {
				t.Errorf("FROM line %q carries no digest pin", from)
			}
		})
	}
}

// TestCacheKeyTracksThePinnedDigest binds the build identity to the PIN, not to
// the tag: two builds of one spec must produce one image, and a re-pin must
// produce a new cache key — otherwise a re-pinned spec would be served the
// image built from the previous digest and the upgrade would silently not
// happen.
func TestCacheKeyTracksThePinnedDigest(t *testing.T) {
	spec := &Spec{Base: DefaultBaseImage, Packages: []PackageAdd{
		{Manager: ManagerAPT, Packages: []string{"ripgrep"}},
	}}

	canonical := "base=" + pinnedBaseRef(DefaultBaseImage) + "\n" +
		string(ManagerAPT) + "=ripgrep,\n"
	sum := sha256.Sum256([]byte(canonical))
	if got, want := spec.CacheKey(), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("CacheKey() = %s, want the digest over the PINNED base (%s)", got, want)
	}

	// The pre-GAP-135 key hashed the tag. Re-derive it and assert the key moved,
	// so this test fails if someone reverts CacheKey to s.Base.
	tagOnly := sha256.Sum256([]byte("base=" + DefaultBaseImage + "\n" + string(ManagerAPT) + "=ripgrep,\n"))
	if hex.EncodeToString(tagOnly[:]) == spec.CacheKey() {
		t.Error("CacheKey() still hashes the tag — a re-pin would reuse the previous build")
	}
}

// TestPinnedBaseRefFallbackIsUnreachableThroughParse documents the one
// non-pinned path: pinnedBaseRef returns the bare reference for a base the
// manifest does not know, which is reachable only for a hand-constructed Spec
// (tests, or a value built before Parse). TestEveryAllowedBaseIsDigestPinned is
// what keeps every base Parse accepts out of that branch.
func TestPinnedBaseRefFallbackIsUnreachableThroughParse(t *testing.T) {
	const unknown = "docker.io/library/ubuntu:26.10"
	if got := pinnedBaseRef(unknown); got != unknown {
		t.Errorf("pinnedBaseRef(%q) = %q, want the bare reference", unknown, got)
	}
	if _, err := Parse([]byte(`{"base":"` + unknown + `"}`)); err == nil {
		t.Error("Parse accepted a base outside the allowed set; the fallback would then be reachable")
	}
}
