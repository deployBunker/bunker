package cli

import (
	"testing"

	"github.com/deployBunker/bunker/internal/provisioning"
)

// TestToolchainGoPinMatchesAgentToolsBootstrap keeps the provisioning
// package's Go pin byte-identical to the pin internal/cli's gopls bootstrap
// already proved live (agenttools_packages.go). Both packages duplicate the
// constants deliberately (provisioning must stay a leaf); this test makes
// the sync a test-time fact — a pin bumped on one side without the other
// fails here.
func TestToolchainGoPinMatchesAgentToolsBootstrap(t *testing.T) {
	if provisioning.GoToolchainVersion != goBootstrapVersion {
		t.Errorf("Go version pin drifted: provisioning %q vs agent-tools bootstrap %q",
			provisioning.GoToolchainVersion, goBootstrapVersion)
	}
	if provisioning.GoToolchainSHA256AMD64 != goBootstrapSHA256AMD64 {
		t.Errorf("Go amd64 pin drifted: provisioning ...%s vs bootstrap ...%s",
			provisioning.GoToolchainSHA256AMD64[len(provisioning.GoToolchainSHA256AMD64)-12:],
			goBootstrapSHA256AMD64[len(goBootstrapSHA256AMD64)-12:])
	}
	if provisioning.GoToolchainSHA256ARM64 != goBootstrapSHA256ARM64 {
		t.Errorf("Go arm64 pin drifted: provisioning ...%s vs bootstrap ...%s",
			provisioning.GoToolchainSHA256ARM64[len(provisioning.GoToolchainSHA256ARM64)-12:],
			goBootstrapSHA256ARM64[len(goBootstrapSHA256ARM64)-12:])
	}
}
