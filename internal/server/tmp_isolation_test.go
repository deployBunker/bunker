package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/hostsetup"
	"github.com/deployBunker/bunker/internal/resource"
)

// activeTmpNamespaceState builds a TmpNamespaceState whose every observed
// property passes, i.e. the state evaluateActive would call Active. It is
// built by hand (no I/O, no root) so tmpIsolationFromState's branches can be
// pinned by flipping exactly one property at a time.
func activeTmpNamespaceState() hostsetup.TmpNamespaceState {
	return hostsetup.TmpNamespaceState{
		ModulePresent:              true,
		GuardModulePresent:         true,
		GuardModuleSupportsPattern: true,
		ExecModulePresent:          true,
		ConfPresent:                true,
		ConfRuleOK:                 true,
		ConfOwnerOK:                true,
		ConfModeOK:                 true,
		HelperPresent:              true,
		HelperIntegrityOK:          true,
		HelperOwnerOK:              true,
		HelperModeOK:               true,
		HelperManifestPresent:      true,
		HelperManifestOwnerOK:      true,
		HelperManifestModeOK:       true,
		HelperDirPresent:           true,
		HelperDirOwnerOK:           true,
		HelperDirModeOK:            true,
		PAMBlockPresent:            true,
		AgentGroupPresent:          true,
		AgentGroup:                 "bunker-agents",
		DeployedVerifyGroup:        "bunker-agents",
		InstanceRootPresent:        true,
		InstanceRootOwnerOK:        true,
		InstanceRootModeOK:         true,
		OwnershipVerifiable:        true,
		Active:                     true,
	}
}

// TestTmpIsolationFromState pins the mapping from an observed
// hostsetup.TmpNamespaceState to the (level, detail) pair ServerInfo reports
// (DF-BUNKER-9). All four branches — private, host-shared, unknown-by-error
// and unknown-unprivileged — run as the non-root user running the tests: the
// mapping is pure, so no branch depends on the test process being root.
func TestTmpIsolationFromState(t *testing.T) {
	active := activeTmpNamespaceState()
	inactiveMissingModule := active
	inactiveMissingModule.ModulePresent = false
	inactiveMissingModule.Active = false
	inactiveMissingPAMBlock := active
	inactiveMissingPAMBlock.PAMBlockPresent = false
	inactiveMissingPAMBlock.Active = false
	inactiveNoIdentifiableReason := active
	inactiveNoIdentifiableReason.Active = false
	unprivileged := active
	unprivileged.OwnershipVerifiable = false
	unprivileged.Active = false

	tests := []struct {
		name       string
		state      hostsetup.TmpNamespaceState
		err        error
		wantLevel  string
		wantDetail string
	}{
		{
			name:       "active provisioning reports private",
			state:      active,
			wantLevel:  "private",
			wantDetail: "per-session pam_namespace instance is provisioned and enforced",
		},
		{
			name:       "inactive with pam_namespace missing reports host-shared with the reason",
			state:      inactiveMissingModule,
			wantLevel:  "host-shared",
			wantDetail: "pam_namespace.so is not installed on this host",
		},
		{
			name:       "inactive with broken PAM block reports host-shared with the reason",
			state:      inactiveMissingPAMBlock,
			wantLevel:  "host-shared",
			wantDetail: "the agent-scoped sshd PAM session block is missing or not intact",
		},
		{
			name:       "inactive with no identifiable reason falls back to the generic message",
			state:      inactiveNoIdentifiableReason,
			wantLevel:  "host-shared",
			wantDetail: "pam_namespace private-/tmp provisioning is not active on this host",
		},
		{
			name:       "probe error reports unknown with the error text",
			state:      activeTmpNamespaceState(),
			err:        errors.New("getent: exit status 2"),
			wantLevel:  "unknown",
			wantDetail: "getent: exit status 2",
		},
		{
			name:       "non-root daemon reports unverifiable unknown",
			state:      unprivileged,
			wantLevel:  "unknown",
			wantDetail: "daemon is not running as root — /tmp isolation state cannot be verified",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, detail := tmpIsolationFromState(tt.state, tt.err)
			if level != tt.wantLevel {
				t.Errorf("level = %q, want %q", level, tt.wantLevel)
			}
			if detail != tt.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tt.wantDetail)
			}
		})
	}
}

// TestServerInfo_TmpIsolationWithNilConfig verifies the nil-cfg guard: a
// service constructed without configuration (as several tests do) must still
// answer ServerInfo — degrading to ("unknown", …) — and must not panic.
func TestServerInfo_TmpIsolationWithNilConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{logger: logger, tracker: tracker}

	req := connect.NewRequest(&v1.ServerInfoRequest{})
	resp, err := svc.ServerInfo(context.Background(), req)
	if err != nil {
		t.Fatalf("ServerInfo() error: %v", err)
	}
	msg := resp.Msg
	if msg.GetTmpIsolation() != "unknown" {
		t.Errorf("TmpIsolation = %q, want %q (nil config degrades, never errors)", msg.GetTmpIsolation(), "unknown")
	}
	if msg.GetTmpIsolationDetail() == "" {
		t.Error("TmpIsolationDetail is empty, want the nil-config reason")
	}
	// The cached probe must answer identically on a second call (sync.Once).
	resp2, err := svc.ServerInfo(context.Background(), req)
	if err != nil {
		t.Fatalf("second ServerInfo() error: %v", err)
	}
	if resp2.Msg.GetTmpIsolation() != msg.GetTmpIsolation() ||
		resp2.Msg.GetTmpIsolationDetail() != msg.GetTmpIsolationDetail() {
		t.Errorf("second ServerInfo() = (%q, %q), want stable (%q, %q)",
			resp2.Msg.GetTmpIsolation(), resp2.Msg.GetTmpIsolationDetail(),
			msg.GetTmpIsolation(), msg.GetTmpIsolationDetail())
	}
}

// TestServerInfo_TmpIsolationWithConfig proves a configured service reports
// the observed host state through the RPC surface (on this non-root test
// host: "unknown", because the ownership half cannot be verified without
// root) and never fails the RPC because of the probe.
func TestServerInfo_TmpIsolationWithConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.ServerInfoRequest{})
	resp, err := svc.ServerInfo(context.Background(), req)
	if err != nil {
		t.Fatalf("ServerInfo() error: %v", err)
	}
	msg := resp.Msg
	if msg.GetTmpIsolation() != "private" && msg.GetTmpIsolation() != "host-shared" && msg.GetTmpIsolation() != "unknown" {
		t.Errorf("TmpIsolation = %q, want one of private/host-shared/unknown", msg.GetTmpIsolation())
	}
	// As non-root the probe MUST report unknown/unverifiable, never a
	// confident "private".
	if msg.GetTmpIsolation() == "private" {
		t.Error("non-root probe reported private — isolation state cannot be verified without root")
	}
	if msg.GetTmpIsolation() != "unknown" {
		t.Errorf("TmpIsolation = %q, want %q for a non-root daemon", msg.GetTmpIsolation(), "unknown")
	}
}
