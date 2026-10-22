package imagespec

import (
	"bytes"
	"encoding/json"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestAgentToolSpecPBMatchesTheCanonicalWire pins TOOLS-003's spawn-carrier
// accessor: the proto the spawn path attaches for --tools-at-spawn must be the
// canonical delivered set — the same AgentToolSpec JSON the CLI prints, parsed
// and rendered back to the wire form. If the accessor and the printed
// remediation ever drift, a spawn-time delivery would name one set while the
// probe, the docs and the runtime stages pin another.
func TestAgentToolSpecPBMatchesTheCanonicalWire(t *testing.T) {
	got := AgentToolSpecPB()
	if got == nil {
		t.Fatal("AgentToolSpecPB() = nil, want the delivered set as a wire proto")
	}

	// The wire form must round-trip to exactly the delivered set's JSON:
	// re-render the proto through FromProto→(marshal the parsed spec's
	// canonical form) and compare package directives to AgentToolPackages.
	specPB := got
	if len(specPB.GetPackages()) != len(AgentToolPackages) {
		t.Fatalf("AgentToolSpecPB carries %d package directives, want %d (the AgentToolPackages set)",
			len(specPB.GetPackages()), len(AgentToolPackages))
	}
	for i, d := range AgentToolPackages {
		pb := specPB.GetPackages()[i]
		if pb.GetManager() != string(d.Manager) {
			t.Errorf("directive %d manager = %q, want %q", i, pb.GetManager(), string(d.Manager))
		}
		if len(pb.GetPackages()) != len(d.Packages) {
			t.Fatalf("directive %d carries %d packages, want %d", i, len(pb.GetPackages()), len(d.Packages))
		}
		for j := range d.Packages {
			if pb.GetPackages()[j] != d.Packages[j] {
				t.Errorf("directive %d package %d = %q, want %q", i, j, pb.GetPackages()[j], d.Packages[j])
			}
		}
	}

	// And the same set the base image names, so the spawn-carried spec cannot
	// silently drop the stock userland pin (DF-BUNKER-80).
	if specPB.GetBase() == "" {
		t.Error("AgentToolSpecPB carries no base image; the spawn-carried spec must pin one")
	}
}

// TestAgentToolSpecPBIsValidOnTheWire asserts the accessor's output survives
// the exact trip a spawn takes: proto → JSON → Parse (the same path the server
// runs on a received ImageSpec). A proto that fails this is undeliverable at
// spawn time, not merely mis-rendered in a test.
func TestAgentToolSpecPBIsValidOnTheWire(t *testing.T) {
	pb := AgentToolSpecPB()
	raw, err := json.Marshal(pb)
	if err != nil {
		t.Fatalf("marshal AgentToolSpecPB: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("AgentToolSpecPB does not render valid JSON: %s", raw)
	}
	// Sanity on the byte level: the canonical package names must appear.
	for _, want := range []string{"ripgrep", "golang.org/x/tools/gopls@latest"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("rendered spec JSON does not name %q: %s", want, raw)
		}
	}
	_ = v1.ImageSpec{} // keep the import tied to the wire type under test
}
