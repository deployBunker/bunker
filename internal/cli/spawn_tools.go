package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// TOOLS-003 piece (a): make tool delivery a PROPERTY of an agent instead of a
// command someone remembers. `bunker spawn --tools-at-spawn` runs the SAME
// delivery the agent-tools command performs — the vendored toolsd copy (when a
// local static artifact exists) plus the registry stages (rg, gopls) — right
// after the agent is created, and FAILS THE SPAWN COMMAND LOUDLY when the
// delivery cannot prove its result. The row's load-bearing rule: a delivery
// step that fails at spawn must not produce an agent that merely looks
// provisioned — so the post-spawn failure text states the agent EXISTS but is
// NOT provisioned, and names the in-place repair command.
//
// Delivery-carrier division (docs/prd/SPEC-agent-tool-delivery.md):
//   - the registry tools ride the image-spec path when the daemon supports
//     specs: --tools-at-spawn attaches imagespec.AgentToolSpec (the canonical
//     delivered set) to the SpawnAgentRequest, so the image itself carries
//     rg+gopls. The runtime stages below are the belt to that braces on
//     spec-less daemons.
//   - toolsd exists in no registry (self-built, static-only), so it is
//     delivered by copy after the spawn — ONLY when a local static artifact
//     can be resolved. A missing local artifact is a named warning, never a
//     silent skip: the registry half still makes the agent fully provisioned
//     for everything the image-spec carrier can carry, so this is not a
//     "looks provisioned but is not" state — but it is never silent.
//
// Everything else is the row's carried constraints, inherited from the
// installer: the agent's $HOME is asked of the agent, the artifact must be
// static (the shared guard refuses otherwise), the result is verified by
// re-probing the agent, and a local/remote version difference is a named
// warning.

// deliverSpawnToolsFunc and deliverFileForSpawnFunc are the test seams
// (DF-BUNKER-27's builder-seam pattern, the same shape
// deliverMissingPackageTools/deliverFile already expose): production wires the
// real delivery; spawn tests substitute a stub so the spawn path can be driven
// end to end without SSH or a real toolsd artifact.
var (
	deliverSpawnToolsFunc     = deliverSpawnTools
	spawnLocalBinaryResolver  = resolveDeliverableBinary
	spawnStaticLinkAssertFunc = assertStaticallyLinked
)

// deliverSpawnTools is the post-spawn delivery verdict, judged by a fresh
// probe of the AGENT (the re-probe rule): every still-missing delivered-set
// tool must read present afterwards, or this returns an error naming the
// survivor.
func deliverSpawnTools(ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string, errOut io.Writer, out io.Writer) error {

	// Probe FIRST so the package stages skip already-present tools (the
	// idempotency contract install shares) and so the verdict can name what
	// THIS delivery changed.
	before, err := probeAgentTools(ctx, client, entry, agentID)
	if err != nil {
		return fmt.Errorf("probe before delivery: %w", err)
	}

	// The vendored leg: toolsd by copy, only when a local static artifact can
	// be resolved. The artifact is refused unless a static link is confirmed
	// (the same last-mile guard the install trusts — a dynamically linked
	// toolsd would die on a far-side libc difference invisible from here).
	local, localErr := spawnLocalBinaryResolver("toolsd", "")
	toolsdAttempted := false
	switch {
	case localErr == nil:
		toolsdAttempted = true
		if err := spawnStaticLinkAssertFunc(local); err != nil {
			return fmt.Errorf("toolsd artifact is not shippable: %w", err)
		}
		if err := deliverToolsdToAgent(ctx, client, entry, agentID, local, errOut); err != nil {
			return err
		}
	case isVendoredArtifactUnavailable(localErr):
		// Named warning, never a silent skip: the binary copy is the only road
		// for toolsd, and it exists only when the operator built one.
		_, _ = fmt.Fprintf(errOut, "bunker: WARNING no local toolsd artifact to copy (%v); "+
			"deliver the binary copy in place with: bunker agent-tools %s --install --binary <dist/toolsd>\n",
			localErr, agentID)
	default:
		return fmt.Errorf("resolve toolsd artifact: %w", localErr)
	}

	// The registry leg: deliver every still-missing registry tool of the
	// delivered set through its pinned stage (a no-op when the image-spec
	// carrier already installed it — the stage short-circuits on
	// already-present).
	if _, packageFailures, err := deliverMissingPackageTools(discardCommand(io.Discard, io.Discard), ctx, client, entry, agentID, before); err != nil {
		return err
	} else if len(packageFailures) > 0 {
		return fmt.Errorf("package-tool delivery failed:\n  %s", strings.Join(packageFailures, "\n  "))
	}

	// The verdict is the re-probe, not the stages' claims. Every tool this
	// delivery owed the agent must now read present on THIS fresh probe.
	report, err := probeAgentTools(ctx, client, entry, agentID)
	if err != nil {
		return fmt.Errorf("delivered, but the verifying probe failed: %w", err)
	}
	var stillMissing []string
	for _, name := range []string{"rg", "gopls"} {
		f := findingFor(report, name)
		if f == nil || f.Status != "present" {
			stillMissing = append(stillMissing, name+" ("+statusOf(f)+")")
		}
	}
	if toolsdAttempted {
		if f := findingFor(report, "toolsd"); f == nil || f.Status != "present" {
			stillMissing = append(stillMissing, "toolsd ("+statusOf(f)+")")
		}
	}
	if len(stillMissing) > 0 {
		return fmt.Errorf("delivery not proven — still missing on the agent: %s", strings.Join(stillMissing, ", "))
	}
	return nil
}

// isVendoredArtifactUnavailable reports whether the local-artifact resolver's
// error is the "nothing to copy" case (absent from PATH, no --binary) rather
// than a resolver defect. Only that case may degrade to the named warning.
func isVendoredArtifactUnavailable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "--binary") && strings.Contains(err.Error(), "make dist")
}

// deliverToolsdToAgent copies the local static toolsd into the agent's
// $HOME/bin (asked of the agent, never assumed), verifies the transfer by the
// agent's own sha256 echo, and verifies the RESULT by a fresh probe of the
// agent — "copied the file" is not "the tool works". The copy rides the same
// audited exec RPC as the probe (base64 payload through stdin), so it works on
// every transport and needs no SSH leg. A version difference between the local
// artifact and the agent's copy is a named warning, never silent.
func deliverToolsdToAgent(ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID, localPath string, errOut io.Writer) error {

	// 1. The destination: the agent's own $HOME/bin — asked of the agent.
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
	dest := home + "/" + agentToolInstallDir + "/toolsd"

	// 2. Transfer through the audited exec path: the artifact is base64'd
	//    into the script payload, the agent decodes it into place (mode 0755
	//    in the same breath), checks the sha256 it was stamped with, and
	//    echoes the installed file's own sha256 back.
	localBytes, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", localPath, err)
	}
	sum := sha256.Sum256(localBytes)
	sha := hex.EncodeToString(sum[:])
	enc := base64.StdEncoding.EncodeToString(localBytes)
	deliveryScript := `set -e
mkdir -p "$HOME/` + agentToolInstallDir + `"
base64 -d > "$HOME/` + agentToolInstallDir + `/toolsd.tmp" <<'BUNKER_TOOLS_PAYLOAD'
` + enc + `
BUNKER_TOOLS_PAYLOAD
printf '%s  %s\n' '` + sha + `' "$HOME/` + agentToolInstallDir + `/toolsd.tmp" | sha256sum -c - >/dev/null
chmod 0755 "$HOME/` + agentToolInstallDir + `/toolsd.tmp"
mv -f "$HOME/` + agentToolInstallDir + `/toolsd.tmp" "$HOME/` + agentToolInstallDir + `/toolsd"
sha256sum "$HOME/` + agentToolInstallDir + `/toolsd" | awk '{print $1}'`
	shaOut, execCode, err := execOnAgentScript(ctx, client, entry, agentID, deliveryScript, stageBudgetSeconds(ctx, 5*time.Minute))
	if err != nil {
		return fmt.Errorf("toolsd copy: %w", err)
	}
	if execCode != 0 {
		return fmt.Errorf("toolsd copy: the agent exited %d while installing the artifact", execCode)
	}
	if remoteSHA := strings.TrimSpace(shaOut); remoteSHA != sha {
		return fmt.Errorf("toolsd copy: transport corruption — local sha256 %s, agent reports %s", sha, remoteSHA)
	}

	// 3. Verify by the only evidence that counts: a fresh probe of the AGENT.
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	report, err := probeAgentTools(probeCtx, client, entry, agentID)
	if err != nil {
		return fmt.Errorf("delivered, but the verifying probe failed: %w", err)
	}
	f := findingFor(report, "toolsd")
	if f == nil || f.Status != "present" {
		return fmt.Errorf("toolsd delivery FAILED: still not reachable on the agent's PATH (probe reports %v after copying to %s)",
			statusOf(f), dest)
	}

	// 4. Named drift warning (GAP-096 criterion 2), same verdict as install.
	if localVersion := localToolsdVersion(localPath); reportVersionDrift(localVersion, f.Version) {
		_, _ = fmt.Fprintf(errOut, "bunker: WARNING version drift — local %q vs agent %q; "+
			"the agent will run the delivered build, not the one you tested\n", localVersion, f.Version)
	}
	return nil
}

// applySpawnToolsDelivery is the spawn command's single entry into this file:
// it prints the progress line, runs the delivery, and on failure emits the
// LOUD verdict the row demands — the agent EXISTS but must not be treated as
// provisioned, and the repair is named. The spawn command then fails so no
// caller (script, QA harness, operator) can misread the outcome.
func applySpawnToolsDelivery(cmd *cobra.Command, ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string) error {

	errOut := cmd.ErrOrStderr()
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, "Delivering verb tools onto the agent (--tools-at-spawn)...")
	if err := deliverSpawnToolsFunc(ctx, client, entry, agentID, errOut, out); err != nil {
		_, _ = fmt.Fprintf(errOut, "\nbunker: TOOL DELIVERY FAILED at spawn — agent %q EXISTS but is NOT provisioned; "+
			"do not use it as provisioned.\n", agentID)
		_, _ = fmt.Fprintf(errOut, "bunker: finish the delivery in place, then re-probe:\n")
		_, _ = fmt.Fprintf(errOut, "  bunker agent-tools %s --install\n", agentID)
		_, _ = fmt.Fprintf(errOut, "  bunker agent-tools %s\n", agentID)
		return fmt.Errorf("tools-at-spawn delivery failed: %w", err)
	}
	_, _ = fmt.Fprintln(out, "Tools delivered and verified by re-probe: toolsd, rg, gopls present on the agent.")
	return nil
}
