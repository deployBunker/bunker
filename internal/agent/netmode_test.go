package agent

// NET-BUNKER-010 + NET-BUNKER-002 tests (specs/network-isolation.md is the
// design authority). Named for what each proves:
//
//   - shared/zero-value argv is BYTE-IDENTICAL to the pre-surface spawn
//     (§5.1 zero-delta law — the regression guard protecting the default);
//   - systemd adds --property=PrivateNetwork=yes exactly ONCE and nothing
//     else (NET-BUNKER-002);
//   - an unknown mode refuses BEFORE any systemd-run invocation (§5.2
//     refuse-loudly — proved at the stage level, pre-launch);
//   - the reported mode + boundary are correct per mode, and an ABSENT mode
//     never renders as safe (§5.2 three-state vocabulary).

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/netmode"
	"github.com/deployBunker/bunker/internal/resource"
)

// netmodeTestUnitArgs is the full fixture used by the argv tests (same shape
// as the GAP-075 pin).
func netmodeTestUnitArgs(mode string) dockerdUnitArgs {
	return dockerdUnitArgs{
		AgentID:        "abc123",
		UnitName:       "bunker-docker-abc123",
		UID:            "1001",
		GID:            "1002",
		UserHome:       "/home/bunker-abc123",
		RuntimeDir:     "/run/bunker/abc123/run",
		DockerSockPath: "/run/bunker/abc123/docker.sock",
		RootlessBin:    "/home/bunker-abc123/bin/dockerd-rootless.sh",
		CPUQuota:       2.0,
		MemoryMax:      4294967296,
		DiskMax:        21474836480,
		MaxProcesses:   4096,
		MaxOpenFiles:   65536,
		NetworkMode:    mode,
	}
}

// TestBuildRootlessDockerdArgs_SharedIsByteIdenticalToPreSurfaceMode is THE
// regression guard (§5.1: today's spawn behavior is byte-identical until an
// operator asks for a mode). Both the explicit "shared" and the zero value
// must produce the exact pre-NET-BUNKER-002 argv — PrivateTmp rides position
// 5, the limit block follows immediately, and NO PrivateNetwork property
// appears anywhere. If the implementation is reverted this test still passes
// (it pins unchanged behavior); the SYSTEMD test below is the one that
// reddens on revert of the mode.
func TestBuildRootlessDockerdArgs_SharedIsByteIdenticalToPreSurfaceMode(t *testing.T) {
	preSurface := netmodeTestUnitArgs("")
	preSurface.NetworkMode = "" // explicit zero value
	shared := netmodeTestUnitArgs(netmode.ModeShared)

	for _, tc := range []struct {
		name string
		args dockerdUnitArgs
	}{
		{name: "zero-value NetworkMode", args: preSurface},
		{name: "explicit shared", args: shared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, env := buildRootlessDockerdArgs(tc.args)
			want := []string{
				"--system",
				"--unit=bunker-docker-abc123",
				"--uid=1001",
				"--gid=1002",
				"--property=PAMName=login",
				// GAP-075 boundary, same position as pre-surface.
				"--property=PrivateTmp=yes",
				// The GAP-116 limit block follows IMMEDIATELY — no mode
				// property slipped between.
				"--property=CPUQuota=200%",
				"--property=MemoryMax=4294967296",
				"--property=LimitFSIZE=21474836480",
				"--property=TasksMax=4096",
				"--property=LimitNOFILE=65536:65536",
			}
			if len(got) < len(want) {
				t.Fatalf("argv too short: %v", got)
			}
			for i, w := range want {
				if got[i] != w {
					t.Fatalf("argv[%d] = %q, want %q\nfull argv: %v", i, got[i], w, got)
				}
			}
			if strings.Join(got, "\x00") != strings.Join(append(append([]string{}, want...),
				"--setenv=PATH=/home/bunker-abc123/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
				"--setenv=HOME=/home/bunker-abc123",
				"--setenv=USER=bunker-abc123",
				"--setenv=XDG_RUNTIME_DIR=/run/bunker/abc123/run",
				"--setenv=DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns",
				"--setenv=DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=builtin",
				"--setenv=DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false",
				"--setenv=DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
				"--setenv=TMPDIR="+config.IsolationTmpDir,
				"/home/bunker-abc123/bin/dockerd-rootless.sh",
				"--host=unix:///run/bunker/abc123/docker.sock",
			), "\x00") {
				t.Fatalf("full argv drifted from the pre-surface shape:\n got: %v", got)
			}
			for _, e := range env {
				if !strings.Contains(strings.Join(got, " "), "--setenv="+e) {
					t.Errorf("env entry %q missing from argv", e)
				}
			}
			if n := countArg(got, "--property=PrivateNetwork=yes"); n != 0 {
				t.Errorf("shared argv carries %d PrivateNetwork properties, want 0 — the default must stay byte-identical", n)
			}
		})
	}
}

// TestBuildRootlessDockerdArgs_SystemdAddsPrivateNetworkExactlyOnce pins
// NET-BUNKER-002: the systemd mode adds --property=PrivateNetwork=yes exactly
// once, positioned immediately after the GAP-075 PrivateTmp boundary, and
// adds NOTHING else — every other element stays byte-identical to the shared
// argv. Reverting the mode implementation reddens this test (the property
// disappears); corrupting it to add extra properties also reddens it.
func TestBuildRootlessDockerdArgs_SystemdAddsPrivateNetworkExactlyOnce(t *testing.T) {
	sharedArgs, sharedEnv := buildRootlessDockerdArgs(netmodeTestUnitArgs(netmode.ModeShared))
	systemdArgs, systemdEnv := buildRootlessDockerdArgs(netmodeTestUnitArgs(netmode.ModeSystemd))

	if n := countArg(systemdArgs, "--property=PrivateNetwork=yes"); n != 1 {
		t.Fatalf("PrivateNetwork=yes appears %d times, want exactly 1: %v", n, systemdArgs)
	}
	// Exactly one MORE element than shared, and it IS the PrivateNetwork
	// property (nothing else added, nothing removed).
	if len(systemdArgs) != len(sharedArgs)+1 {
		t.Fatalf("systemd argv length %d, want shared length %d + exactly 1\nsystemd: %v\nshared:  %v",
			len(systemdArgs), len(sharedArgs), systemdArgs, sharedArgs)
	}
	// Same env: the mode is a property, not an env change — the rootlesskit
	// container-networking plumbing is untouched.
	if strings.Join(systemdEnv, "\x00") != strings.Join(sharedEnv, "\x00") {
		t.Fatalf("systemd env drifted from shared env:\nsystemd: %v\nshared:  %v", systemdEnv, sharedEnv)
	}
	// Position: the ONLY divergence from the shared argv is the mode property
	// inserted immediately after the GAP-075 PrivateTmp boundary. Construct
	// the expected argv directly and require byte equality — the inserted
	// element is the only difference, position included.
	wantSystemd := append(append([]string{}, sharedArgs[:6]...), "--property=PrivateNetwork=yes")
	wantSystemd = append(wantSystemd, sharedArgs[6:]...)
	if strings.Join(systemdArgs, "\x00") != strings.Join(wantSystemd, "\x00") {
		t.Fatalf("systemd argv drifted from shared+PrivateNetwork insertion:\n got: %v\nwant: %v", systemdArgs, wantSystemd)
	}
	if sharedArgs[5] != "--property=PrivateTmp=yes" {
		t.Fatal("fixture drift: PrivateTmp is not at the expected position")
	}
	// The exec/socket contract is untouched: DOCKER_HOST stays the unix
	// socket path (spec §3, composition rule 1 — the property that makes the
	// mode viable).
	if !containsArg(systemdArgs, "--setenv=DOCKER_HOST=unix:///run/bunker/abc123/docker.sock") {
		t.Errorf("systemd argv lost the unix-socket DOCKER_HOST contract: %v", systemdArgs)
	}
}

// TestSpawnRefusesUnknownNetworkModeBeforeLaunch proves the §5.2
// refuse-loudly law END TO END: an unknown mode fails the spawn at the
// VALIDATE stage (StageValidate — the pre-launch stage, before systemd-run
// can create any unit), with a named error carrying the offending value and
// the valid set. A rollback breadcrumb exists (the machinery saw the
// refusal), the refusal NEVER resolves to shared, and — the half-created-
// agent bug class this repo guards against — no rollback of a partially
// spawned agent is even needed because nothing was created.
func TestSpawnRefusesUnknownNetworkModeBeforeLaunch(t *testing.T) {
	// networkModeProbe points systemd-run at a sentinel binary; if the spawn
	// ever reached the launch stage the probe file would appear. The refusal
	// must happen long before.
	probe := mkNetmodeLaunchProbe(t)
	t.Setenv("BUNKER_NETWORKMODE_LAUNCH_PROBE", probe)

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.SSHDir = t.TempDir()
	logger := newNetmodeQuietLogger()
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()

	// Note: whitespace-only input follows the repo's GAP-116 resolver
	// convention (config.ResolveNetworkMode trims, so "   " behaves as unset
	// → the declared default). The vocabulary-level whitespace-only refusal
	// is pinned in internal/netmode's own tests.
	for _, mode := range []string{"pasta", "rootlesskit", "Systemd"} {
		t.Run("mode="+mode, func(t *testing.T) {
			_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{
				AgentId:     uniqueAgentID("netmode"),
				Ttl:         "1h",
				NetworkMode: mode,
			})
			if err == nil {
				t.Fatalf("Spawn with unknown mode %q succeeded — the §5.2 silent-fallback violation", mode)
			}
			// Named error: offending value AND the valid set.
			for _, want := range []string{mode, netmode.ModeShared, netmode.ModeSystemd} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q missing %q", err, want)
				}
			}
			// Pre-launch stage: the refusal is StageValidate (spawn Step 1d —
			// before ANY side effect, in particular before systemd-run).
			if !strings.Contains(err.Error(), StageValidate) {
				t.Errorf("refusal %q is not the pre-launch stage %s", err, StageValidate)
			}
			// The refusal never reports a shared fallback as success — err
			// non-nil IS the contract, but also assert the mode never
			// resolved: the error must not be a later-stage failure (which
			// would mean the mode silently became shared and the spawn
			// proceeded).
			for _, stage := range []string{StageUserCreate, StageDockerdStart, StageRootlessInstall} {
				if strings.Contains(err.Error(), stage) {
					t.Errorf("refusal %q names stage %s — the spawn proceeded past validate with mode %q", err, stage, mode)
				}
			}
		})
	}
	// No unit was created: the launch probe never fired.
	if netmodeProbeFired(t, probe) {
		t.Error("launch probe fired — a systemd-run invocation happened despite the pre-launch refusal")
	}
}

// TestNetworkIsolationReportingPerMode pins the §5.2 reporting payload per
// mode: each enforced mode carries its mode name AND the matching boundary
// string; an ABSENT mode yields NIL (never a fabricated "shared").
func TestNetworkIsolationReportingPerMode(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		wantNil      bool
		wantMode     string
		wantBoundary string
	}{
		{name: "shared reports its honest boundary", mode: netmode.ModeShared, wantMode: netmode.ModeShared, wantBoundary: netmode.BoundaryFor(netmode.ModeShared)},
		{name: "systemd reports its honest boundary", mode: netmode.ModeSystemd, wantMode: netmode.ModeSystemd, wantBoundary: netmode.BoundaryFor(netmode.ModeSystemd)},
		{name: "absent mode is nil, never shared", mode: "", wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ni := networkIsolationForSummary(tt.mode)
			if tt.wantNil {
				if ni != nil {
					t.Fatalf("absent mode produced %v, want nil (absence must never render as safe or as shared)", ni)
				}
				return
			}
			if ni == nil {
				t.Fatalf("mode %q produced nil reporting", tt.mode)
			}
			if ni.GetMode() != tt.wantMode {
				t.Errorf("mode = %q, want %q", ni.GetMode(), tt.wantMode)
			}
			if ni.GetBoundary() != tt.wantBoundary {
				t.Errorf("boundary = %q, want %q", ni.GetBoundary(), tt.wantBoundary)
			}
			if ni.GetBoundary() == "" {
				t.Errorf("mode %q reports an empty boundary — a bound that is not reported is not a bound", tt.mode)
			}
		})
	}
}

// TestAgentSummaryCarriesNetworkIsolation pins the wire path: a record with
// a mode exposes it on AgentSummary.network_isolation; a pre-surface record
// leaves the field nil.
func TestAgentSummaryCarriesNetworkIsolation(t *testing.T) {
	rec := &resource.AgentRecord{AgentID: "abc123", NetworkMode: netmode.ModeSystemd, NetworkIsolation: networkIsolationForSummary(netmode.ModeSystemd)}
	sum := rec.ToAgentSummary()
	if sum.GetNetworkIsolation() == nil {
		t.Fatal("summary lost the network isolation payload")
	}
	if got := sum.GetNetworkIsolation().GetMode(); got != netmode.ModeSystemd {
		t.Errorf("summary mode = %q, want systemd", got)
	}
	if got := sum.GetNetworkIsolation().GetBoundary(); got != netmode.BoundaryFor(netmode.ModeSystemd) {
		t.Errorf("summary boundary = %q, want the systemd boundary", got)
	}

	pre := (&resource.AgentRecord{AgentID: "old"}).ToAgentSummary()
	if pre.GetNetworkIsolation() != nil {
		t.Errorf("pre-surface summary reports %v, want nil (empty must never render as safe)", pre.GetNetworkIsolation())
	}
}

// TestInBandMarkerPerMode pins the in-band marker against the three-state
// law: explicit modes name the mode; empty/unknown render the UNKNOWN marker
// and never a shared claim.
func TestInBandMarkerPerMode(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantSubstr []string
	}{
		{name: "shared", mode: netmode.ModeShared, wantSubstr: []string{"mode shared"}},
		{name: "systemd", mode: netmode.ModeSystemd, wantSubstr: []string{"mode systemd", "NO outbound"}},
		{name: "absent is UNKNOWN, never shared", mode: "", wantSubstr: []string{"unknown", "NOT verified"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InBandNetworkIsolationMarker(tt.mode)
			for _, s := range tt.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("marker(%q) = %q, missing %q", tt.mode, got, s)
				}
			}
		})
	}
}
