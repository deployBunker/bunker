package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// SURF-006: `bunker surface` makes the agent tool surface a SUPPORTED step.
// Install writes the toolsd socket + per-connection service units through the
// agent's own exec context (the agent's identity owns %t and %h, so the same
// unit text is correct for every agent), enables + starts the socket, and
// verifies the result — a fresh agent becomes socket-ready with one command
// and no hand-written files. Remove stops, disables, deletes and proves the
// absence. Both verbs are idempotent and refuse, with a NAMED error, to run
// against a dead systemd user manager: writing units into a manager that will
// never load them is the silent partial state this command exists to prevent.

// surfaceOperationTimeout is the client-side budget for ONE agent operation
// (preflight, install, verify, remove, probe). systemctl on a live manager is
// milliseconds; the budget exists for the SSH hop, not for systemd.
const surfaceOperationTimeout = 30 * time.Second

// runSurfacePreflight asks the agent whether its user manager is alive, and
// fails with a NAMED cause when it is not. Every mutating step runs only after
// this passes, so a dead manager can never produce a half-installed surface.
func runSurfacePreflight(ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string) error {

	pctx, cancel := context.WithTimeout(ctx, surfaceOperationTimeout)
	defer cancel()
	out, _, err := execOnAgentScript(pctx, client, entry, agentID, surfacePreflightScript, 0)
	if err != nil {
		return fmt.Errorf("preflight on agent %q failed: %w", agentID, err)
	}
	line := parseSurfaceIsRunning(out)
	if isUserManagerUnavailable(line) || line == "" {
		return classifySurfaceManagerFailure(agentID, out)
	}
	return nil
}

// runSurfaceInstall installs the units through the agent, verifies the result,
// probes the toolsd dependency, and reports. The verify step is the evidence,
// not a formality: its exit code and RESULT line decide the command's verdict.
func runSurfaceInstall(cmd *cobra.Command, ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string, asJSON bool) error {

	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	if err := runSurfacePreflight(ctx, client, entry, agentID); err != nil {
		return err
	}

	ictx, cancel := context.WithTimeout(ctx, surfaceOperationTimeout)
	defer cancel()
	installOut, exitCode, err := execOnAgentScript(ictx, client, entry, agentID, buildSurfaceInstallScript(), 0)
	if err != nil {
		return fmt.Errorf("install units on agent %q: %w", agentID, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("install units on agent %q failed (exit %d): %s",
			agentID, exitCode, tailSurfaceOutput(installOut, 500))
	}
	changed := surfaceInstallWrote(installOut)

	vctx, cancelV := context.WithTimeout(ctx, surfaceOperationTimeout)
	defer cancelV()
	verifyOut, vExit, err := execOnAgentScript(vctx, client, entry, agentID, surfaceVerifyScript, 0)
	if err != nil {
		return fmt.Errorf("verify on agent %q: %w", agentID, err)
	}
	state, ok := parseSurfaceVerifyOutput(verifyOut)
	if !ok {
		return fmt.Errorf("verify on agent %q produced no parsable state (exit %d, output: %s) — "+
			"reporting UNKNOWN rather than success", agentID, vExit, tailSurfaceOutput(verifyOut, 500))
	}
	if vExit != 0 {
		return fmt.Errorf("verify on agent %q failed (exit %d): unit files absent after install — "+
			"the units were not written; refusing to report success (agent output: %s)",
			agentID, vExit, tailSurfaceOutput(verifyOut, 500))
	}
	state.Agent = agentID
	state.Changed = changed

	// Honest dependency probe: the units are management, the binary is not.
	// A socket whose ExecStart target is absent activates nothing — say so
	// loudly instead of reporting a provisioned-looking agent.
	pctx, cancelP := context.WithTimeout(ctx, surfaceOperationTimeout)
	defer cancelP()
	probeOut, _, err := execOnAgentScript(pctx, client, entry, agentID, surfaceToolsdProbeScript, 0)
	state.UnixReady = parseSurfaceToolsdProbe(probeOut)
	if err != nil {
		state.UnixReady = "unknown"
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(state)
	}

	action := "unchanged (already current)"
	if changed {
		action = "written"
	}
	fmt.Fprintf(out, "surface installed for agent %s (units %s)\n", agentID, action)
	fmt.Fprintf(out, "  socket : %s (%s)\n", state.SocketPath, state.State)
	fmt.Fprintf(out, "  owner  : %s, mode %s\n", state.Owner, state.Mode)
	fmt.Fprintf(out, "  units  : %s/%s + %s/%s\n", "$HOME", surfaceUnitSubdir, "$HOME", surfaceUnitSubdir)
	switch state.UnixReady {
	case "present":
		fmt.Fprintln(out, "  toolsd : present at $HOME/bin/toolsd")
	default:
		fmt.Fprintf(errOut, "bunker: WARNING toolsd is %s at the agent's $HOME/bin/toolsd — "+
			"the socket is up but will activate nothing until the binary is delivered: "+
			"bunker agent-tools %s --install\n", state.UnixReady, agentID)
	}
	return nil
}

// runSurfaceRemove tears the surface down through the agent and proves the
// absence: no socket, no enabled unit files. A tolerant teardown (a down
// manager is fine) with a STRICT result check.
func runSurfaceRemove(cmd *cobra.Command, ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string, asJSON bool) error {

	out := cmd.OutOrStdout()

	rctx, cancel := context.WithTimeout(ctx, surfaceOperationTimeout)
	defer cancel()
	remOut, exitCode, err := execOnAgentScript(rctx, client, entry, agentID, buildSurfaceRemoveScript(), 0)
	if err != nil {
		return fmt.Errorf("remove units on agent %q: %w", agentID, err)
	}
	socketPath, state, ok := parseSurfaceRemoveOutput(remOut)
	if !ok {
		return fmt.Errorf("remove on agent %q produced no parsable result (exit %d, output: %s) — "+
			"reporting failure rather than assuming success", agentID, exitCode, tailSurfaceOutput(remOut, 500))
	}
	switch {
	case exitCode == 0 && state == "removed":
		if asJSON {
			return json.NewEncoder(out).Encode(map[string]string{
				"agent": agentID, "socket_path": socketPath, "state": state,
			})
		}
		fmt.Fprintf(out, "surface removed for agent %s\n", agentID)
		fmt.Fprintf(out, "  socket %s: gone\n", socketPath)
		fmt.Fprintf(out, "  units  : deleted (%s + %s)\n", surfaceSocketUnitName, surfaceServiceUnitName)
		return nil
	case state == "still-present":
		return fmt.Errorf("remove on agent %q left the socket present at %s (exit %d) — "+
			"the units are deleted but the socket file survived; inspect the user manager on the agent host",
			agentID, socketPath, exitCode)
	case state == "remove-incomplete":
		return fmt.Errorf("remove on agent %q could not delete the unit files (exit %d) — "+
			"nothing was claimed removed; inspect permissions on the agent's %s directory",
			agentID, exitCode, surfaceUnitSubdir)
	default:
		return fmt.Errorf("remove on agent %q ended in state %q (exit %d, output: %s)",
			agentID, state, exitCode, tailSurfaceOutput(remOut, 500))
	}
}

// NewSurfaceCommand builds `bunker surface` with its install/remove
// subcommands. Shared flag set: --server, --json, --timeout.
func NewSurfaceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "surface",
		Short: "Manage the agent's toolsd socket surface (systemd user units)",
		Long: `Install or remove the toolsd tool surface on an agent: a systemd --user
socket unit plus a per-connection service template, written THROUGH the
agent's own exec context so the agent's identity owns the socket (%t) and
home (%h) specifiers.

  bunker surface install <agent-id>   make the agent socket-ready
  bunker surface remove <agent-id>    remove the units and prove absence

Install is idempotent: unit files already identical to the wanted content are
skipped untouched. Both verbs refuse — with a named cause — to act when the
agent's systemd user manager is not running (linger off / manager down):
writing units into a manager that will never load them is a silent partial
state.

The BINARY the socket activates ($HOME/bin/toolsd) is a separate concern:
` + "`bunker agent-tools <agent-id> --install`" + ` delivers it. Install warns loudly
when the binary is absent rather than claiming a working surface.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newSurfaceInstallCommand())
	cmd.AddCommand(newSurfaceRemoveCommand())
	return cmd
}

// surfaceVerbContext resolves config/server/client and returns a client with a
// deadline — the same resolution chain every agent-addressing command runs.
func surfaceVerbContext(cmd *cobra.Command, serverName string, timeout uint32) (
	context.Context, bunkerv1connect.BunkerdClient, ServerEntry, error) {

	cfg, err := LoadCLIConfig()
	if err != nil {
		return nil, nil, ServerEntry{}, fmt.Errorf("load config: %w", err)
	}
	// Mutating command: never fall back to the shared active_server default
	// without being told (GAP-093 fail-closed binding).
	resolved, berr := SessionScopedTarget(serverName, cfg.ActiveServer)
	if berr != nil {
		return nil, nil, ServerEntry{}, berr
	}
	entry, ok := cfg.Servers[resolved]
	if !ok {
		return nil, nil, ServerEntry{}, fmt.Errorf("server %q not found in config", resolved)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "bunker: using server %q\n", resolved)
	client := newBunkerdClient(entry)
	ctxTimeout := time.Duration(timeout) * time.Second
	if ctxTimeout <= 0 {
		ctxTimeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	_ = cancel // owned by the returned ctx's caller via the command lifetime
	return ctx, client, entry, nil
}

// newSurfaceInstallCommand implements `bunker surface install <agent-id>`.
func newSurfaceInstallCommand() *cobra.Command {
	var (
		serverName string
		asJSON     bool
		timeout    uint32
	)
	cmd := &cobra.Command{
		Use:   "install <agent-id>",
		Short: "Install the toolsd socket + service units on an agent (idempotent)",
		Long: `Install the toolsd tool surface on an agent and leave it socket-ready.

Writes two unit files through the agent's own exec context into the agent's
$HOME/.config/systemd/user:

  toolsd.socket     ListenStream=%t/toolsd.sock, SocketMode=0600, Accept=yes,
                    WantedBy=sockets.target
  toolsd@.service   ExecStart=%h/bin/toolsd mcp, StandardInput/Output=socket

then runs systemctl --user daemon-reload, enable and start. systemd spawns
one ` + "`toolsd mcp`" + ` process per accepted connection; zero before the first.

Idempotent: a unit file already identical to the wanted content is skipped
untouched, so re-running changes nothing. After installing, the command
verifies the live state (socket path, owner, mode) and probes whether
$HOME/bin/toolsd exists — a missing binary is a loud WARNING naming
` + "`bunker agent-tools <agent-id> --install`" + `, not an install failure.

If the agent's systemd user manager is not running (linger off / manager
down), the command fails with that cause NAMED and installs nothing.

Examples:
  bunker surface install abc12345
  bunker surface install abc12345 --server staging
  bunker surface install abc12345 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			ctx, client, entry, err := surfaceVerbContext(cmd, serverName, timeout)
			if err != nil {
				return err
			}
			return runSurfaceInstall(cmd, ctx, client, entry, agentID, asJSON)
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the resulting state as JSON")
	cmd.Flags().Uint32Var(&timeout, "timeout", 60, "Overall timeout in seconds")
	return cmd
}

// newSurfaceRemoveCommand implements `bunker surface remove <agent-id>`.
func newSurfaceRemoveCommand() *cobra.Command {
	var (
		serverName string
		asJSON     bool
		timeout    uint32
	)
	cmd := &cobra.Command{
		Use:   "remove <agent-id>",
		Short: "Remove the toolsd units from an agent and prove absence",
		Long: `Remove the toolsd tool surface from an agent.

Stops and disables the socket unit, deletes BOTH unit files
(toolsd.socket + toolsd@.service) from the agent's
$HOME/.config/systemd/user, and reloads the user manager. The result is
verified on the agent: the socket path must be GONE and both unit files
deleted, and the command reports each step — a removal that leaves residue
fails instead of claiming success.

Tolerant of a down user manager (the units can be uninstalled without one)
but strict about the outcome: any surviving socket or unit file is an error
naming what survived.

Examples:
  bunker surface remove abc12345
  bunker surface remove abc12345 --server staging
  bunker surface remove abc12345 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			ctx, client, entry, err := surfaceVerbContext(cmd, serverName, timeout)
			if err != nil {
				return err
			}
			return runSurfaceRemove(cmd, ctx, client, entry, agentID, asJSON)
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the removal result as JSON")
	cmd.Flags().Uint32Var(&timeout, "timeout", 60, "Overall timeout in seconds")
	return cmd
}
