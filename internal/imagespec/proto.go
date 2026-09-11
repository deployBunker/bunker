package imagespec

import (
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// FromProto converts and validates a proto ImageSpec message into a Spec.
// A nil/empty proto message yields the default (no-op) spec, which every
// caller treats as "use the existing bootstrap image".
func FromProto(pb *v1.ImageSpec) (*Spec, error) {
	if pb == nil {
		return Default(), nil
	}
	raw := wireImageSpec{Base: pb.GetBase()}
	for _, d := range pb.GetPackages() {
		raw.Packages = append(raw.Packages, PackageAddSpec{
			Manager:  PackageManager(d.GetManager()),
			Packages: append([]string(nil), d.GetPackages()...),
		})
	}
	return fromWire(&raw)
}

// ToProto renders a validated Spec back into the proto message (used by tests
// and by any future read-side surface).
func (s *Spec) ToProto() *v1.ImageSpec {
	if s == nil {
		return nil
	}
	out := &v1.ImageSpec{Base: s.Base}
	for _, d := range s.Packages {
		out.Packages = append(out.Packages, &v1.PackageAdd{
			Manager:  string(d.Manager),
			Packages: append([]string(nil), d.Packages...),
		})
	}
	return out
}

// wireImageSpec mirrors the JSON wire form (base + packages directives); it
// is the shared shape both Parse and FromProto validate through.
type wireImageSpec struct {
	Base     string           `json:"base"`
	Packages []PackageAddSpec `json:"packages"`
}
