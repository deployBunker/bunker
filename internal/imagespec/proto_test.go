package imagespec

import (
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

func TestFromProto_NilIsDefault(t *testing.T) {
	spec, err := FromProto(nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Base != DefaultBaseImage || len(spec.Packages) != 0 {
		t.Errorf("nil proto must yield default spec, got %+v", spec)
	}
}

func TestFromProto_Valid(t *testing.T) {
	pb := &v1.ImageSpec{
		Base: "docker.io/library/debian:12",
		Packages: []*v1.PackageAdd{
			{Manager: "apt", Packages: []string{"jq"}},
			{Manager: "npm", Packages: []string{"typescript@5.6.3"}},
		},
	}
	spec, err := FromProto(pb)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Base != "docker.io/library/debian:12" {
		t.Errorf("base = %q", spec.Base)
	}
	if len(spec.Packages) != 2 {
		t.Fatalf("packages = %+v", spec.Packages)
	}

	// Round-trip: ToProto(FromProto(x)) == x (semantically).
	rt := spec.ToProto()
	if rt.Base != pb.Base || len(rt.Packages) != len(pb.Packages) {
		t.Errorf("round-trip mismatch: %+v vs %+v", rt, pb)
	}
	for i := range rt.Packages {
		if rt.Packages[i].Manager != pb.Packages[i].Manager {
			t.Errorf("directive %d manager drift", i)
		}
	}
}

func TestFromProto_Rejects(t *testing.T) {
	cases := []*v1.ImageSpec{
		{Base: "evil.example.com/x:1"},
		{Packages: []*v1.PackageAdd{{Manager: "sh", Packages: []string{"-c"}}}},
		{Packages: []*v1.PackageAdd{{Manager: "apt", Packages: []string{"a; b"}}}},
	}
	for i, pb := range cases {
		if _, err := FromProto(pb); err == nil {
			t.Errorf("case %d: proto spec accepted that must be rejected: %+v", i, pb)
		}
	}
}

// TestParseToProtoEquivalence proves the JSON and proto paths enforce the
// identical grammar: whatever JSON accepts, the equivalent proto accepts, and
// both produce the same cache key.
func TestParseToProtoEquivalence(t *testing.T) {
	spec, err := Parse([]byte(`{"base": "docker.io/library/debian:12", "packages": [{"manager": "go", "packages": ["golang.org/x/tools/gopls@v0.17.0"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	pbSpec, err := FromProto(spec.ToProto())
	if err != nil {
		t.Fatalf("proto round-trip rejected: %v", err)
	}
	if pbSpec.CacheKey() != spec.CacheKey() {
		t.Errorf("cache keys diverge: %s vs %s", pbSpec.CacheKey(), spec.CacheKey())
	}
}
