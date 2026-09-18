package agent

import (
	"testing"

	"github.com/deployBunker/bunker/internal/hostsetup"
)

// TestIsolationGrantCapabilityMatchesHostsetup pins the TWO copies of the
// capability token together (GAP-082). internal/agent owns the grant
// (provisionIsolation, StageIsolationProvision) and advertises the token in
// `bunkerd --version`; internal/hostsetup requires it — but it cannot import
// this package (internal/agent imports internal/hostsetup, so the reverse would
// be an import cycle), so hostsetup keeps its own literal.
//
// This is the legal direction of the check: the importer verifies against the
// imported. Without it the two literals could drift and the probe would refuse
// every healthy daemon — two copies of a string that must never disagree.
func TestIsolationGrantCapabilityMatchesHostsetup(t *testing.T) {
	if IsolationGrantCapability != hostsetup.GrantCapability {
		t.Fatalf("capability tokens drifted: agent.IsolationGrantCapability = %q, hostsetup.GrantCapability = %q",
			IsolationGrantCapability, hostsetup.GrantCapability)
	}
}

// TestSpawnCapabilitiesIncludeTheGrant pins what `bunkerd --version` advertises:
// the token the installer requires must be in the list printVersion renders, or
// the probe refuses a daemon that does carry the grant.
func TestSpawnCapabilitiesIncludeTheGrant(t *testing.T) {
	caps := SpawnCapabilities()
	if len(caps) == 0 {
		t.Fatalf("SpawnCapabilities() = %v, want at least %q", caps, IsolationGrantCapability)
	}
	found := false
	for _, c := range caps {
		if c == IsolationGrantCapability {
			found = true
		}
		if c == "" {
			t.Errorf("SpawnCapabilities() contains an empty token: %v", caps)
		}
	}
	if !found {
		t.Errorf("SpawnCapabilities() = %v, want it to include %q", caps, IsolationGrantCapability)
	}
	// The advertised list must survive the parser the probe uses on it: a token
	// with a comma or whitespace would be split into two and never match.
	build, err := hostsetup.ParseDaemonVersionOutput([]byte("bunkerd 0.1.4\n  caps: " + caps[0] + "\n"))
	if err != nil {
		t.Fatalf("the advertised capability does not round-trip through the probe parser: %v", err)
	}
	if !build.HasCapability(IsolationGrantCapability) {
		t.Errorf("advertised capability %q does not survive ParseDaemonVersionOutput: %+v", IsolationGrantCapability, build)
	}
}
