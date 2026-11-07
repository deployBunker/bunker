// Network-isolation enforcement gate for `bunker spawn` (INT-CI-020).
//
// The defect this file closes: `--network-mode systemd` was validated as a MODE
// NAME and then sent as a proto field. A daemon that predates the
// network-isolation surface ignores the unknown field, answers OK and creates
// the agent in the HOST network namespace — so the caller's exit code, output
// and connection bundle all looked identical to real containment. That is a
// FALSE CUT: a battery that proves its containment from the flag value runs
// with full egress.
//
// Two gates, mirroring the DF-BUNKER-82 egress precedent:
//
//   - BEFORE the spawn: refuse when the daemon does not advertise the requested
//     mode in ServerInfoResponse.network_modes_supported. An EMPTY list means
//     the daemon predates network-isolation enforcement, and absence is never
//     an affirmative statement (the proto field's own contract).
//   - AFTER the spawn: read the daemon's record back (GetAgent) and require
//     that it reports the SAME canonical mode with a boundary and the
//     read-back verdict. Anything else and the agent is DESTROYED before the
//     command fails, so a refused spawn leaves no uncontained agent behind.
//
// Only modes that ENFORCE something on the network axis are gated ("systemd").
// "shared" (and an unset flag, which defers to the daemon's own resolution
// chain) changes nothing, so pre-existing invocations against older daemons
// keep working byte-identically — exactly the inert-mode rule the egress gate
// uses for "open".
package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/netmode"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// daemonNetworkModes fetches ServerInfo and returns the daemon's reported
// network-isolation vocabulary. An empty slice is meaningful (pre-capability
// daemon), not an error: the caller decides what absence means. Transport and
// permission failures ARE errors — a daemon we cannot interrogate must not be
// assumed either capable or incapable.
func daemonNetworkModes(entry ServerEntry) ([]string, error) {
	info, err := ConnectServerWithEntry(entry)
	if err != nil {
		return nil, fmt.Errorf("network-isolation capability probe: %w", err)
	}
	return info.GetNetworkModesSupported(), nil
}

// enforceNetworkModeRequested reports whether the spawn asks for a mode that
// ENFORCES a boundary. "shared" is explicit but isolates nothing (the argv is
// byte-identical to a pre-surface spawn), so a daemon without the network
// surface behaves exactly like a current one for it. The empty value defers to
// the daemon's own resolution chain, which is the daemon's business, not the
// client's.
func enforceNetworkModeRequested(mode string) bool {
	if strings.TrimSpace(mode) == "" {
		return false
	}
	return config.CanonicalNetworkMode(mode) != netmode.ModeShared
}

// ensureDaemonSupportsNetworkMode is the spawn-path gate (INT-CI-020). It
// refuses — client-side, BEFORE any SpawnAgent RPC — when:
//
//   - the daemon's ServerInfo carries an EMPTY network_modes_supported field
//     and the spawn requests an enforced mode: the daemon build predates
//     network-isolation enforcement (any 0.1.x bunkerd, e.g. bunker-las-03 at
//     0.1.4), the flag would be silently ignored, and the agent would run in
//     the host network namespace while the caller believes it is contained;
//   - the daemon reports its vocabulary but the requested mode is not in it.
//
// It returns nil without any RPC when the spawn requests no mode or the inert
// "shared" mode. An unresolvable ServerInfo fails the spawn loudly: a daemon we
// cannot interrogate must not be assumed capable.
func ensureDaemonSupportsNetworkMode(mode string, entry ServerEntry) error {
	if !enforceNetworkModeRequested(mode) {
		return nil
	}
	requested := config.CanonicalNetworkMode(mode)
	modes, err := daemonNetworkModes(entry)
	if err != nil {
		return fmt.Errorf(
			"refusing to spawn with --network-mode %q: network-isolation capability probe failed (%v). "+
				"The daemon's network-isolation support could not be verified, so the boundary cannot be promised; upgrade the daemon or drop the flag",
			requested, err,
		)
	}
	if len(modes) == 0 {
		return fmt.Errorf(
			"refusing to spawn with --network-mode %q: the daemon at this server predates network-isolation enforcement "+
				"(ServerInfo carries no network-mode capability) and would silently ignore the flag — the agent would be created "+
				"in the HOST network namespace with full egress while this command reported success. Upgrade bunkerd on this "+
				"server to a build that reports network_modes_supported (INT-CI-020) or drop the flag; `bunker status` shows the "+
				"daemon's reported default mode",
			requested,
		)
	}
	for _, m := range modes {
		if config.CanonicalNetworkMode(m) == requested {
			return nil
		}
	}
	return fmt.Errorf(
		"refusing to spawn with --network-mode %q: the daemon does not report support for it (supported: %s). "+
			"Choose a supported mode or upgrade the daemon",
		requested, strings.Join(modes, ", "),
	)
}

// verifySpawnedNetworkMode is the POST-spawn read-back (spec §5.2: "requested"
// and "provided" are different fields). It takes the mode the caller asked for
// and the isolation the daemon recorded for the agent, and returns an error
// unless the daemon reports the SAME canonical mode, a boundary line, and the
// read-back verdict.
//
// It is pure so the table test can drive every wire shape without a host. An
// unset request ("", the daemon's resolution chain) is not verified here: the
// caller asked for nothing, so there is nothing to hold the daemon to.
func verifySpawnedNetworkMode(requested string, ni *v1.NetworkIsolation) error {
	if !enforceNetworkModeRequested(requested) {
		return nil
	}
	want := config.CanonicalNetworkMode(requested)
	if ni == nil || ni.GetMode() == "" {
		return fmt.Errorf(
			"spawn requested network-isolation mode %q but the daemon reported no enforced mode for the agent "+
				"(the record carries no network-isolation state) — the boundary is NOT verified", want)
	}
	got := config.CanonicalNetworkMode(ni.GetMode())
	if got != want {
		return fmt.Errorf(
			"spawn requested network-isolation mode %q but the daemon reports %q — a silent fallback, so the boundary is NOT verified",
			want, got)
	}
	if strings.TrimSpace(ni.GetBoundary()) == "" {
		return fmt.Errorf(
			"spawn requested network-isolation mode %q and the daemon reports the mode with NO boundary line — "+
				"a mode without a boundary fails the reporting law (§5.2), so the boundary is NOT verified", want)
	}
	if !ni.GetVerified() {
		return fmt.Errorf(
			"spawn requested network-isolation mode %q; the daemon reports %q but did not read the enforced unit property back "+
				"from the running unit — the boundary is NOT verified", want, got)
	}
	return nil
}

// spawnNetworkModeReadback fetches the daemon's OWN record for the agent that
// was just created (GetAgent) and returns its network-isolation state. This is
// the read-back surface: the spawn response carries no agent summary, so the
// daemon's durable record is the only honest source for "what did the daemon
// actually enforce".
func spawnNetworkModeReadback(ctx context.Context, client interface {
	GetAgent(context.Context, *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error)
}, agentID, token string) (*v1.NetworkIsolation, error) {
	req := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	resp, err := client.GetAgent(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get agent for the network-isolation read-back: %w", err)
	}
	return resp.Msg.GetAgent().GetNetworkIsolation(), nil
}

// destroyUnverifiedAgent tears down an agent whose boundary did not verify, so
// a refused spawn never leaves an uncontained agent running. It is best-effort
// and BOUNDED (its own short context: the spawn's context may already be
// spent); its error is reported inside the caller's refusal, never swallowed
// into a success.
func destroyUnverifiedAgent(ctx context.Context, client interface {
	DestroyAgent(context.Context, *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error)
}, agentID, token string) error {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
	defer cancel()
	req := connect.NewRequest(&v1.DestroyAgentRequest{AgentId: agentID, Force: true})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	if _, err := client.DestroyAgent(dctx, req); err != nil {
		return fmt.Errorf("destroy agent %q: %w", agentID, err)
	}
	return nil
}
