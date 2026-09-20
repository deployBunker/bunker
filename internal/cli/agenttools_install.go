package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// deliverChildBudget bounds ONE delivery step (the scp, then the chmod/chown).
// It is longer than copyChildBudget on purpose: a 10MB binary over a slow link
// legitimately takes longer than the 30s that suffices for a source file, and a
// transfer killed at the wire looks exactly like a hung agent.
const deliverChildBudget = 120 * time.Second

// deliverableVendoredTools are the tools this command SHIPS by copying a binary.
// Only tools we build ourselves belong here: there is no registry to install
// from, so a copy is the only honest mechanism.
//
// The division of labour is deliberate and stated in the output: ripgrep and the
// language servers ARE in distribution registries, so they belong to the
// image-spec package-add path (which pins versions and verifies signatures)
// rather than to a binary copy that would bypass both.
var deliverableVendoredTools = []string{"toolsd"}

// agentToolInstallDir is the destination directory INSIDE the agent, relative to
// its home. It matches the path the server already puts on every exec PATH
// (internal/server service.go: agentBinPath = $HOME/bin), so a delivered tool is
// reachable without touching shell profiles.
const agentToolInstallDir = "bin"

// installAgentTools delivers the vendored tools onto the agent and reports the
// result. It fails loudly and early at each step rather than reporting a partial
// success: a delivery that silently half-lands leaves an agent that looks
// provisioned and is not.
func installAgentTools(cmd *cobra.Command, ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string, binaryPath string) error {

	errOut := cmd.ErrOrStderr()

	// 1. Resolve the LOCAL artifact to ship.
	local, err := resolveDeliverableBinary("toolsd", binaryPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(errOut, "bunker: delivering %s from %s\n", "toolsd", local)

	// 2. Refuse to ship a dynamically linked binary. This is the SAME guard
	//    `make dist` enforces in the toolkit repo, applied at the last moment
	//    before the artifact crosses a host boundary — a dynamic link is
	//    invisible until the binary runs against a different libc, which is
	//    exactly the failure a delivery mechanism must not have.
	if err := assertStaticallyLinked(local); err != nil {
		return err
	}

	// 3. Resolve the destination: the agent's own $HOME/bin, asked of the agent
	//    rather than assumed, because the home directory is the daemon's choice.
	homeOut, exitCode, err := execOnAgentScript(ctx, client, entry, agentID,
		`mkdir -p "$HOME/`+agentToolInstallDir+`" && printf '%s' "$HOME"`, 60)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("resolve agent home: the agent exited %d", exitCode)
	}
	home := strings.TrimSpace(homeOut)
	if home == "" || !strings.HasPrefix(home, "/") {
		return fmt.Errorf("resolve agent home: agent returned %q, which is not an absolute path", home)
	}
	destDir := filepath.Join(home, agentToolInstallDir)
	fmt.Fprintf(errOut, "bunker: destination %s\n", filepath.Join(destDir, "toolsd"))

	// 4. Transfer, then fix mode and ownership — the same two steps `bunker cp`
	//    performs, and for the same reason: scp lands the file as the SSH user
	//    it authenticated as, which is not necessarily the agent user.
	if err := deliverFile(cmd, entry, agentID, local, filepath.Join(destDir, "toolsd")); err != nil {
		return err
	}

	// 5. Verify by the only evidence that counts: a fresh probe of the AGENT.
	report, err := probeAgentTools(ctx, client, entry, agentID)
	if err != nil {
		return fmt.Errorf("delivered, but the verifying probe failed: %w", err)
	}
	toolsd := findingFor(report, "toolsd")
	if toolsd == nil || toolsd.Status != "present" {
		return fmt.Errorf("delivery FAILED: toolsd is still not reachable on the agent's PATH "+
			"(probe reports %v after copying to %s)", statusOf(toolsd), filepath.Join(destDir, "toolsd"))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\ndelivered toolsd: %s\n", toolsd.Version)

	// 6. Check local/remote drift (GAP-096 criterion 2): a silent behaviour split
	//    between the CLI an operator tests and the binary an agent runs is worse
	//    than a loud version mismatch.
	if localVersion := localToolsdVersion(local); localVersion != "" && toolsd.Version != "" &&
		!strings.Contains(toolsd.Version, commandName(localVersion)) {
		fmt.Fprintf(errOut, "bunker: WARNING version drift — local %q vs agent %q; "+
			"the agent will run the delivered build, not the one you tested\n",
			localVersion, toolsd.Version)
	}

	// 7. Say what this command did NOT deliver, and which path owns it.
	remaining := []string{}
	for _, name := range report.Missing {
		if !isVendoredDeliverable(name) {
			remaining = append(remaining, name)
		}
	}
	if len(remaining) > 0 {
		fmt.Fprintf(errOut, "\nbunker: NOT delivered here (no vendored binary; install via the image-spec package-add path): %s\n",
			strings.Join(remaining, ", "))
		fmt.Fprintln(errOut, `  spawn with: {"packages":[{"manager":"apt","packages":["ripgrep"]},`+
			`{"manager":"go","packages":["golang.org/x/tools/gopls@latest"]}]}`)
	}
	return nil
}

// resolveDeliverableBinary finds the artifact to ship: an explicit --binary
// wins, otherwise the tool is looked up on PATH. A clear failure here is much
// cheaper than a mystery downstream.
func resolveDeliverableBinary(name, override string) (string, error) {
	if override != "" {
		info, err := os.Stat(override)
		if err != nil {
			return "", fmt.Errorf("--binary %q: %w", override, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("--binary %q is a directory, not a binary", override)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("--binary %q is not executable", override)
		}
		return override, nil
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("no %s on PATH to deliver; build the shippable artifact "+
			"(`make dist` in the toolkit repo) and pass --binary <path>", name)
	}
	return found, nil
}

// assertStaticallyLinked refuses a dynamically linked artifact.
//
// It asks ldd first and falls back to file(1); when NEITHER can confirm a static
// link it refuses rather than assuming the best. The check is deliberately
// conservative in that direction: the failure it prevents (a binary that dies on
// an agent because of a libc difference) is remote, delayed and confusing, so a
// false refusal is far cheaper than a false pass.
func assertStaticallyLinked(path string) error {
	if out, err := exec.Command("ldd", path).CombinedOutput(); err == nil || len(out) > 0 {
		if strings.Contains(string(out), "not a dynamic executable") {
			return nil
		}
	}
	if out, err := exec.Command("file", path).CombinedOutput(); err == nil {
		if strings.Contains(string(out), "statically linked") {
			return nil
		}
		if strings.Contains(string(out), "dynamically linked") {
			return fmt.Errorf("refusing to deliver %s: it is DYNAMICALLY linked, so it depends on "+
				"this machine's libc and may die on the agent. Build it statically "+
				"(`make dist` in the toolkit repo sets CGO_ENABLED=0)", path)
		}
	}
	return fmt.Errorf("refusing to deliver %s: cannot confirm it is statically linked "+
		"(neither ldd nor file reported a static build)", path)
}

// deliverFile copies one local file into the agent and then fixes its mode and
// ownership over SSH. Extracted from the `bunker cp` flow for the same reason
// that flow exists: scp authenticates as the SSH user, which is not necessarily
// the agent user, so the copy alone can leave a file the agent cannot execute.
func deliverFile(cmd *cobra.Command, entry ServerEntry,
	agentID, localPath, remotePath string) error {

	userAtHost, err := resolveUserAtHost(entry, agentSSHFSMount(entry, agentID), "")
	if err != nil {
		return fmt.Errorf("resolve agent host: %w", err)
	}
	keyPath, err := defaultSSHKeyPath(agentID)
	if err != nil {
		return fmt.Errorf("resolve SSH key: %w", err)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		return fmt.Errorf("SSH key not found at %q — spawn the agent first", keyPath)
	}
	const port = uint32(22)
	sshUser := strings.SplitN(userAtHost, "@", 2)[0]

	childCtx, stopChildren := newChildSignalContext()
	defer stopChildren()

	scpCtx, cancelSCP := context.WithTimeout(childCtx, deliverChildBudget)
	defer cancelSCP()
	scpCmd := newLongLivedCommand(scpCtx, "scp",
		buildSCPArgs(keyPath, port, localPath, userAtHost, remotePath, false)...)
	scpCmd.Stdout = cmd.OutOrStdout()
	scpCmd.Stderr = cmd.ErrOrStderr()
	if scpErr := runDetachedChildCommand(scpCmd); scpErr != nil {
		if childCtx.Err() != nil {
			return fmt.Errorf("scp: interrupted by a shutdown signal — the delivery was stopped")
		}
		return fmt.Errorf("scp: %w", scpErr)
	}

	// chmod AND chown in one round trip: both are required, and the agent's PATH
	// entry is useless for a file the agent user cannot execute or does not own.
	postCtx, cancelPost := context.WithTimeout(childCtx, deliverChildBudget)
	defer cancelPost()
	postCmd := newLongLivedCommand(postCtx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "IdentitiesOnly=yes",
		"-i", keyPath,
		"-p", fmt.Sprintf("%d", port),
		userAtHost,
		fmt.Sprintf("chmod 0755 %s && chown %s:%s %s", remotePath, sshUser, sshUser, remotePath),
	)
	postCmd.Stdout = cmd.OutOrStdout()
	postCmd.Stderr = cmd.ErrOrStderr()
	if postErr := runDetachedChildCommand(postCmd); postErr != nil {
		if childCtx.Err() != nil {
			return fmt.Errorf("chmod/chown after scp: interrupted by a shutdown signal")
		}
		return fmt.Errorf("chmod/chown after scp: %w", postErr)
	}
	return nil
}

func findingFor(r agentToolReport, name string) *agentToolFinding {
	for i := range r.Tools {
		if r.Tools[i].Name == name {
			return &r.Tools[i]
		}
	}
	return nil
}

func statusOf(f *agentToolFinding) string {
	if f == nil {
		return "not reported"
	}
	return f.Status
}

func isVendoredDeliverable(name string) bool {
	for _, d := range deliverableVendoredTools {
		if d == name {
			return true
		}
	}
	return false
}

// commandName picks the version token out of a `toolsd version <token>` line so
// the drift check compares builds rather than whole sentences.
func commandName(versionLine string) string {
	fields := strings.Fields(versionLine)
	if len(fields) >= 3 && fields[0] == "toolsd" && fields[1] == "version" {
		return fields[2]
	}
	return versionLine
}

// localToolsdVersion reads the version of the artifact about to be shipped.
func localToolsdVersion(path string) string {
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// agentSSHFSMount reads the stored sshfs_mount command for an agent, which is
// where the host this client actually reached lives. The `bunker cp` flow reads
// it from a GetAgent response; the delivery needs the same string, so it is
// resolved here through the same RPC rather than guessed from config.
func agentSSHFSMount(entry ServerEntry, agentID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := newBunkerdClient(entry)
	req := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
	if token := resolveToken(entry); token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	resp, err := client.GetAgent(ctx, req)
	if err != nil || resp == nil || resp.Msg.GetAgent() == nil {
		return ""
	}
	return resp.Msg.GetAgent().GetSshfsMount()
}
