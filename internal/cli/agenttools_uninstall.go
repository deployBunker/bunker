package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// GAP-096 criterion 3: uninstall/rollback. Delivering toolsd onto an agent is a
// supported step; removing it has to be one too, or an operator who wants to
// roll back reaches for an undocumented `ssh + rm` that no probe ever verifies.
//
// The shape mirrors `bunker surface remove`: a tolerant teardown (removing an
// already-absent file is a named outcome, not an error) with a STRICT result
// check — after the removal the agent is re-probed through the same audited
// exec path the install trusts, and "toolsd is still reachable" is a failure
// naming the surviving path. The probe is the evidence, not the rm's exit
// code, because "result is evidence" is the contract in both directions.
//
// Only files this command family DELIVERS are removed ($HOME/bin/toolsd); tools
// that arrived through the image-spec package-add path (rg, gopls) belong to
// their package managers and are deliberately out of scope.

// agentToolsRemoveScript removes the vendored binaries from the agent's
// $HOME/bin and reports each file as one tab-separated line:
//
//	<name>	REMOVED	<was-present|already-absent>
//	<name>	FAILED	<rm's own words>
//
// It never aborts on the first failure (no `set -e`): one stubborn file must
// not stop the removal of the others, and the re-probe afterwards is what
// decides the verdict anyway. The directory itself is left in place — the
// server puts it on every exec PATH and other tooling may hold files there.
func agentToolsRemoveScript() string {
	var b strings.Builder
	b.WriteString("for t in")
	for _, name := range deliverableVendoredTools {
		b.WriteString(" " + name)
	}
	b.WriteString(`; do
  p="$HOME/` + agentToolInstallDir + `/$t"
  if [ -e "$p" ]; then
    if out=$(rm -f "$p" 2>&1); then
      printf '%s\tREMOVED\twas-present\n' "$t"
    else
      printf '%s\tFAILED\t%s\n' "$t" "$out"
    fi
  else
    printf '%s\tREMOVED\talready-absent\n' "$t"
  fi
done
`)
	return b.String()
}

// agentToolsRemoval is the parsed outcome for ONE delivered file.
type agentToolsRemoval struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "REMOVED" or "FAILED"
	Detail string `json:"detail"` // "was-present", "already-absent", or rm's error
}

// parseAgentToolsRemovalOutput parses the removal script's lines. A line it
// cannot parse is reported as a failed removal with the line itself as the
// detail — silent tolerance is how a half-removal would read as a clean one.
func parseAgentToolsRemovalOutput(output string) []agentToolsRemoval {
	removals := []agentToolsRemoval{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" {
			removals = append(removals, agentToolsRemoval{
				Name: "?", Status: "FAILED", Detail: "unparsable output line: " + line,
			})
			continue
		}
		removals = append(removals, agentToolsRemoval{
			Name:   strings.TrimSpace(parts[0]),
			Status: strings.TrimSpace(parts[1]),
			Detail: strings.TrimSpace(parts[2]),
		})
	}
	return removals
}

// uninstallAgentTools removes the delivered tools from the agent, then re-probes
// to PROVE absence. The re-probe is the same probeAgentTools the install path
// trusts, so "uninstalled" and "installed" are verified by the same evidence.
func uninstallAgentTools(cmd *cobra.Command, ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string) error {

	errOut := cmd.ErrOrStderr()
	out := cmd.OutOrStdout()

	// 1. Remove the delivered files, through the agent's own exec context.
	//    The script is tolerant; the probe below is what decides the verdict.
	removeOut, exitCode, err := execOnAgentScript(ctx, client, entry, agentID, agentToolsRemoveScript(), 0)
	if err != nil {
		return fmt.Errorf("remove delivered tools on agent %q: %w", agentID, err)
	}
	removals := parseAgentToolsRemovalOutput(removeOut)

	// 2. Re-probe: the verdict is the agent's PATH, not the rm's exit code.
	report, err := probeAgentTools(ctx, client, entry, agentID)
	if err != nil {
		return fmt.Errorf("removal ran, but the verifying probe failed: %w", err)
	}
	toolsd := findingFor(report, "toolsd")
	if toolsd != nil && toolsd.Status == "present" {
		return fmt.Errorf("uninstall FAILED: toolsd is still reachable on the agent's PATH "+
			"(probe reports version %q after removing $HOME/%s/toolsd) — the removal did not "+
			"take effect; inspect the agent's $HOME/%s directory",
			toolsd.Version, agentToolInstallDir, agentToolInstallDir)
	}

	// 3. Report each file by name, tolerantly: already-absent is data, not an
	//    error. A FAILED removal is a warning here and an error below, so the
	//    human output still names everything that happened before the exit.
	failed := 0
	for _, r := range removals {
		switch r.Status {
		case "REMOVED":
			// The report writers below are best-effort: a closed stdout must
			// not mask the removal verdict, which the re-probe already
			// decided. Explicit blank assignment (the lint gate wants the
			// choice visible, not implicit).
			_, _ = fmt.Fprintf(out, "removed %s from the agent ($HOME/%s/%s): %s\n",
				r.Name, agentToolInstallDir, r.Name, r.Detail)
		case "FAILED":
			failed++
			_, _ = fmt.Fprintf(errOut, "bunker: WARNING removal of %s reported a failure: %s\n", r.Name, r.Detail)
		default:
			failed++
			_, _ = fmt.Fprintf(errOut, "bunker: WARNING removal of %s ended in state %q (%s)\n", r.Name, r.Status, r.Detail)
		}
	}
	if exitCode != 0 {
		failed++
		_, _ = fmt.Fprintf(errOut, "bunker: WARNING removal script exited %d\n", exitCode)
	}
	if failed > 0 {
		return fmt.Errorf("uninstall on agent %q incomplete: %d removal step(s) not confirmed — "+
			"nothing is claimed uninstalled beyond what the re-probe proves absent", agentID, failed)
	}

	_, _ = fmt.Fprintf(out, "uninstalled delivered tools for agent %s; verifying re-probe reports toolsd %s\n",
		agentID, statusOf(toolsd))
	return nil
}
