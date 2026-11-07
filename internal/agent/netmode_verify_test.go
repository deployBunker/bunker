package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/netmode"
)

// TestRequiredUnitProperty pins the mode→property mapping the read-back probe
// uses. It is derived from the SAME constants the unit argv builder emits
// (netmode.PropertyPrivateNetworkYes / PropertyProtectProcInvisible), so a mode
// whose argv element changes cannot silently stop being verified — the read-back
// and the request cannot drift apart.
func TestRequiredUnitProperty(t *testing.T) {
	tests := []struct {
		mode     string
		wantName string
		wantVal  string
		wantOK   bool
	}{
		{mode: netmode.ModeSystemd, wantName: "PrivateNetwork", wantVal: "yes", wantOK: true},
		{mode: netmode.ModeProcVis, wantName: "ProtectProc", wantVal: "invisible", wantOK: true},
		// Modes that enforce nothing have NOTHING to read back: claiming a
		// verified boundary for them would be a manufactured bound.
		{mode: netmode.ModeShared, wantOK: false},
		{mode: "", wantOK: false},
		{mode: "unknown", wantOK: false},
	}
	for _, tt := range tests {
		name, val, ok := requiredUnitProperty(tt.mode)
		if ok != tt.wantOK {
			t.Fatalf("requiredUnitProperty(%q) ok = %v, want %v", tt.mode, ok, tt.wantOK)
		}
		if !ok {
			continue
		}
		if name != tt.wantName || val != tt.wantVal {
			t.Errorf("requiredUnitProperty(%q) = (%q, %q), want (%q, %q)", tt.mode, name, val, tt.wantName, tt.wantVal)
		}
	}
}

// stubUnitProperties swaps the probe seam for the duration of a test and
// records every (unit, property) it was asked for.
func stubUnitProperties(t *testing.T, value string, err error) *[]string {
	t.Helper()
	var asked []string
	orig := unitPropertyReader
	unitPropertyReader = func(_ context.Context, unitName, property string) (string, error) {
		asked = append(asked, unitName+":"+property)
		return value, err
	}
	t.Cleanup(func() { unitPropertyReader = orig })
	return &asked
}

// TestVerifyUnitNetworkMode_Table drives the daemon-side read-back (INT-CI-020).
//
// The pre-fix spawn path had NO read-back at all: it sent
// --property=PrivateNetwork=yes to systemd-run and treated a green exit status
// as containment. Every arm below is therefore new behaviour, and the three
// failing arms are the defect being closed:
//
//   - systemd requested, the running unit reports PrivateNetwork=no  → REFUSE
//     (the mode is not applied; recording it would be a false cut);
//   - systemd requested, the probe cannot read the property at all    → REFUSE
//     (an unverifiable boundary is not a boundary);
//   - shared/empty                                                    → verified
//     false with NO probe (nothing is enforced, so nothing is claimed).
func TestVerifyUnitNetworkMode_Table(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		value        string
		readErr      error
		wantVerified bool
		wantErr      []string
		wantProbed   bool
	}{
		{
			name:         "systemd_private_network_yes_verifies",
			mode:         netmode.ModeSystemd,
			value:        "yes",
			wantVerified: true,
			wantProbed:   true,
		},
		{
			name:       "systemd_private_network_no_refuses",
			mode:       netmode.ModeSystemd,
			value:      "no",
			wantErr:    []string{"systemd", "PrivateNetwork", `"no"`, "NOT applied", "refused"},
			wantProbed: true,
		},
		{
			name:       "systemd_unreadable_property_refuses",
			mode:       netmode.ModeSystemd,
			readErr:    errors.New("Failed to connect to bus: No such file or directory"),
			wantErr:    []string{"could not be read back", "cannot verify"},
			wantProbed: true,
		},
		{
			name:         "procvis_protectproc_invisible_verifies",
			mode:         netmode.ModeProcVis,
			value:        "invisible",
			wantVerified: true,
			wantProbed:   true,
		},
		{
			name:         "shared_enforces_nothing_never_probes",
			mode:         netmode.ModeShared,
			wantVerified: false,
			wantProbed:   false,
		},
		{
			name:         "empty_mode_enforces_nothing_never_probes",
			mode:         "",
			wantVerified: false,
			wantProbed:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := stubUnitProperties(t, tt.value, tt.readErr)

			verified, err := verifyUnitNetworkMode(context.Background(), "bunker-docker-abc123", tt.mode)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("verifyUnitNetworkMode(%q) err = nil, want a refusal", tt.mode)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q missing %q", err.Error(), want)
					}
				}
			} else if err != nil {
				t.Fatalf("verifyUnitNetworkMode(%q) unexpected error: %v", tt.mode, err)
			}
			if verified != tt.wantVerified {
				t.Errorf("verifyUnitNetworkMode(%q) verified = %v, want %v", tt.mode, verified, tt.wantVerified)
			}
			probed := len(*asked) > 0
			if probed != tt.wantProbed {
				t.Errorf("verifyUnitNetworkMode(%q) probed = %v (asked %v), want %v", tt.mode, probed, *asked, tt.wantProbed)
			}
			if tt.wantProbed && len(*asked) == 1 && !strings.HasPrefix((*asked)[0], "bunker-docker-abc123:") {
				t.Errorf("probe targeted %q, want the agent's own unit", (*asked)[0])
			}
		})
	}
}

// TestVerifyUnitNetworkMode_DetachedFromRequestContext pins the rollback-budget
// rule (DF-BUNKER-21) applied to this probe: a cancelled spawn request must not
// turn the read-back into an instant "context canceled" — that would be an
// unverified boundary blamed on the client instead of a real measurement.
func TestVerifyUnitNetworkMode_DetachedFromRequestContext(t *testing.T) {
	var sawLiveContext bool
	orig := unitPropertyReader
	unitPropertyReader = func(ctx context.Context, _, _ string) (string, error) {
		sawLiveContext = ctx.Err() == nil
		return "yes", nil
	}
	t.Cleanup(func() { unitPropertyReader = orig })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client already gave up

	verified, err := verifyUnitNetworkMode(ctx, "bunker-docker-abc123", netmode.ModeSystemd)
	if err != nil {
		t.Fatalf("verifyUnitNetworkMode on a cancelled request context: %v", err)
	}
	if !verified {
		t.Error("verified = false, want true")
	}
	if !sawLiveContext {
		t.Error("the probe inherited the dead request context — it must run detached")
	}
}
