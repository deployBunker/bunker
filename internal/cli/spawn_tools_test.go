package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// TOOLS-003 piece (a): the spawn-time tool delivery. These tests drive
// deliverSpawnTools and the spawn command's --tools-at-spawn wiring through the
// scripted ExecAgent fake (the same shape the uninstall/flow tests use): no
// config file, no SSH, no real toolsd artifact required — the fake plays the
// daemon and the vendored leg is stubbed through the seams.

// probeOutputWith renders a probe report with per-tool states (the uninstall
// tests' probeOutput renders ONE status for every tool; the spawn delivery
// verdict needs per-tool control, so it has its own renderer).
func probeOutputWith(states map[string]string) string {
	lines := make([]string, 0, len(agentToolCatalog))
	for _, dep := range agentToolCatalog {
		status := states[dep.Name]
		if status == "" {
			status = "absent"
		}
		v := ""
		if status == "present" {
			v = dep.Name + " version 1.0.0"
		}
		lines = append(lines, dep.Name+"\t"+status+"\t"+v)
	}
	return strings.Join(lines, "\n")
}

// runSpawnToolsDelivery drives deliverSpawnTools directly, with the vendored
// (copy) leg stubbed away, so the registry stages + re-probe verdict are
// exercised against the fake alone.
func runSpawnToolsDelivery(t *testing.T, fake *fakeExecBunkerd) (string, string, error) {
	t.Helper()
	srv := newTestServer(t, fake)
	t.Cleanup(srv.Close)

	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	entry := ServerEntry{Name: "default", URL: srv.URL}

	var out, errOut bytes.Buffer
	err := deliverSpawnTools(context.Background(), client, entry, "agent-spawn-tools-test", &errOut, &out)
	return out.String(), errOut.String(), err
}

// isCatalogProbe reports whether an exec'd script is the catalog probe. The
// probe's distinctive token is `command -v "$t"` (the delivery stages also
// contain `command -v`, but against apt-get/go), so stage scripts never
// classify as probes.
func isCatalogProbe(script string) bool {
	return strings.Contains(script, `command -v "$t"`)
}

// isRgStage / isGoplsStage classify the two registry delivery stages by the
// tokens their builders embed: the apt stage names its package in pkg='ripgrep',
// the go stage assigns gpkg=. Match THOSE rather than the fetcher invocation
// (the script reads "$fetcher" download — the literal "apt-get download" never
// appears).
func isRgStage(script string) bool {
	return strings.Contains(script, `pkg='ripgrep'`)
}

func isGoplsStage(script string) bool {
	return strings.Contains(script, "gpkg=")
}

// stubVendoredLegAbsent points the vendored resolver at the absent-artifact
// signature (no local toolsd to copy) and restores it on cleanup.
func stubVendoredLegAbsent(t *testing.T) {
	t.Helper()
	orig := spawnLocalBinaryResolver
	spawnLocalBinaryResolver = func(name, override string) (string, error) {
		return "", &resolverAbsentError{}
	}
	t.Cleanup(func() { spawnLocalBinaryResolver = orig })
}

// resolverAbsentError mimics resolveDeliverableBinary's PATH-miss error (the
// "--binary ... make dist" wording) so the warning-classification branch is
// exercisable without a real PATH.
type resolverAbsentError struct{}

func (e *resolverAbsentError) Error() string {
	return "no toolsd on PATH to deliver; build the shippable artifact (`make dist` in the toolkit repo) and pass --binary <path>"
}

// installedLine / failedLine render one stage verdict line for the fakes.
func installedLine(name, version string) string {
	return name + "\tINSTALLED\t" + version + "\n"
}

func failedLine(name, detail string) string {
	return name + "\tFAILED\t" + detail + "\n"
}

// TestSpawnToolsDeliveryProbesFirst pins the delivery's shape: the FIRST exec
// is the pre-delivery probe (the stages read it for idempotency).
func TestSpawnToolsDeliveryProbesFirst(t *testing.T) {
	var scripts []string
	fake := &fakeExecBunkerd{
		respond: func(script string) (string, int32, error) {
			scripts = append(scripts, script)
			return "", 1, context.DeadlineExceeded
		},
	}

	_, _, err := runSpawnToolsDelivery(t, fake)
	if err == nil {
		t.Fatal("a probe that cannot run must fail the delivery")
	}
	if len(scripts) == 0 || !isCatalogProbe(scripts[0]) {
		t.Fatalf("the first exec must be the pre-delivery probe, got: %d scripts", len(scripts))
	}
}

// TestSpawnToolsDeliveryLoudOnUnknownResolverError pins the vendored-leg
// classification: a resolver error that is NOT the absent-artifact signature
// must fail the delivery LOUDLY (never degrade to the named warning).
func TestSpawnToolsDeliveryLoudOnUnknownResolverError(t *testing.T) {
	sawProbe := false
	fake := &fakeExecBunkerd{
		respond: func(script string) (string, int32, error) {
			if isCatalogProbe(script) {
				sawProbe = true
			}
			return probeOutputWith(map[string]string{"toolsd": "present", "git": "present", "jq": "present"}), 0, nil
		},
	}
	orig := spawnLocalBinaryResolver
	spawnLocalBinaryResolver = func(name, override string) (string, error) {
		return "", filepath.ErrBadPattern // not the absent signature → loud
	}
	t.Cleanup(func() { spawnLocalBinaryResolver = orig })

	_, _, err := runSpawnToolsDelivery(t, fake)
	if err == nil {
		t.Fatal("a resolver error that is NOT the absent-artifact signature must fail the delivery loudly")
	}
	if !strings.Contains(err.Error(), "resolve toolsd artifact") {
		t.Errorf("the failure must name the vendored-leg resolution, got: %v", err)
	}
	if !sawProbe {
		t.Error("the pre-delivery probe must still have run before the vendored leg resolved")
	}
}

// TestSpawnToolsDeliveryWarnsNamedWhenNoToolsdArtifact pins the vendored
// absence path: with no local toolsd artifact, the delivery NAMES the warning
// (never silent), still delivers the registry tools, and succeeds when the
// re-probe proves them present.
func TestSpawnToolsDeliveryWarnsNamedWhenNoToolsdArtifact(t *testing.T) {
	calls := 0
	fake := &fakeExecBunkerd{
		respond: func(script string) (string, int32, error) {
			calls++
			switch {
			case isCatalogProbe(script) && calls == 1:
				return probeOutputWith(map[string]string{"toolsd": "present", "git": "present", "jq": "present"}), 0, nil
			case isRgStage(script):
				return installedLine("rg", "ripgrep 14.1.0"), 0, nil
			case isGoplsStage(script):
				return installedLine("gopls", "gopls v0.23.0"), 0, nil
			default:
				return probeOutputWith(map[string]string{"toolsd": "present", "git": "present", "jq": "present", "rg": "present", "gopls": "present"}), 0, nil
			}
		},
	}
	stubVendoredLegAbsent(t)

	_, errOut, err := runSpawnToolsDelivery(t, fake)
	if err != nil {
		t.Fatalf("registry-only delivery must succeed when the re-probe proves presence, got: %v", err)
	}
	if !strings.Contains(errOut, "WARNING no local toolsd artifact") {
		t.Errorf("the no-artifact case must print the NAMED warning, got stderr:\n%s", errOut)
	}
	if !strings.Contains(errOut, "agent-tools agent-spawn-tools-test --install") {
		t.Errorf("the warning must name the in-place repair command, got stderr:\n%s", errOut)
	}
}

// TestSpawnToolsDeliveryFailsWhenReProbeStillMissing is the row's loud rule:
// every stage printed INSTALLED, but the final probe still reports rg absent —
// the delivery must FAIL with the survivor named. "The stages printed
// INSTALLED" is never "the tools work".
func TestSpawnToolsDeliveryFailsWhenReProbeStillMissing(t *testing.T) {
	calls := 0
	fake := &fakeExecBunkerd{
		respond: func(script string) (string, int32, error) {
			calls++
			switch {
			case isCatalogProbe(script) && calls == 1:
				return probeOutputWith(map[string]string{"toolsd": "present", "git": "present", "jq": "present"}), 0, nil
			case isRgStage(script):
				return installedLine("rg", "ripgrep 14.1.0"), 0, nil
			case isGoplsStage(script):
				return installedLine("gopls", "gopls v0.23.0"), 0, nil
			default:
				// The lie: rg still absent after the "successful" stage.
				return probeOutputWith(map[string]string{"toolsd": "present", "git": "present", "jq": "present", "gopls": "present"}), 0, nil
			}
		},
	}
	stubVendoredLegAbsent(t)

	_, _, err := runSpawnToolsDelivery(t, fake)
	if err == nil {
		t.Fatal("a re-probe that still reports a delivered-set tool absent must fail the delivery")
	}
	if !strings.Contains(err.Error(), "still missing") || !strings.Contains(err.Error(), "rg") {
		t.Errorf("the failure must name the survivor, got: %v", err)
	}
}

// TestSpawnToolsDeliveryFailsWhenAStageFails pins the stage verdict handling:
// a FAILED stage line is collected and the delivery fails naming the tool and
// the agent's own words — no partial stage is ever read as installed.
func TestSpawnToolsDeliveryFailsWhenAStageFails(t *testing.T) {
	calls := 0
	fake := &fakeExecBunkerd{
		respond: func(script string) (string, int32, error) {
			calls++
			switch {
			case isCatalogProbe(script) && calls == 1:
				return probeOutputWith(map[string]string{"toolsd": "present", "git": "present", "jq": "present"}), 0, nil
			case isRgStage(script):
				return failedLine("rg", "apt-get download: stage-failed"), 0, nil
			case isGoplsStage(script):
				return installedLine("gopls", "gopls v0.23.0"), 0, nil
			default:
				return probeOutputWith(map[string]string{"toolsd": "present", "git": "present", "jq": "present", "gopls": "present"}), 0, nil
			}
		},
	}
	stubVendoredLegAbsent(t)

	_, _, err := runSpawnToolsDelivery(t, fake)
	if err == nil {
		t.Fatal("a FAILED stage must fail the delivery")
	}
	if !strings.Contains(err.Error(), "rg") || !strings.Contains(err.Error(), "apt-get download") {
		t.Errorf("the failure must name the tool and the agent's own words, got: %v", err)
	}
}

// TestSpawnToolsDeliveryIsIdempotent: when the probe reports everything
// already present (the image-spec carrier delivered rg+gopls at spawn), NO
// delivery stage may be exec'd and the delivery succeeds — a re-run (renew,
// retry) is network-free.
func TestSpawnToolsDeliveryIsIdempotent(t *testing.T) {
	probes := 0
	fake := &fakeExecBunkerd{
		respond: func(script string) (string, int32, error) {
			if !isCatalogProbe(script) {
				t.Fatalf("no stage must run when the probe reports everything present, got a non-probe exec: %.80s", script)
			}
			probes++
			return probeOutputWith(map[string]string{
				"toolsd": "present", "git": "present", "jq": "present", "rg": "present", "gopls": "present",
			}), 0, nil
		},
	}
	stubVendoredLegAbsent(t)

	if _, _, err := runSpawnToolsDelivery(t, fake); err != nil {
		t.Fatalf("an already-provisioned agent must be a cheap success, got: %v", err)
	}
	if probes != 2 {
		t.Errorf("delivery ran %d probes, want exactly 2 (before + verifying re-probe)", probes)
	}
}

// TestApplySpawnToolsDeliveryFailsLoud pins the spawn-surface wrapper: a
// delivery failure writes the row's loud verdict (the agent EXISTS but is NOT
// provisioned), names the in-place repair, and returns the error so the spawn
// command exits non-zero.
func TestApplySpawnToolsDeliveryFailsLoud(t *testing.T) {
	orig := deliverSpawnToolsFunc
	deliverSpawnToolsFunc = func(ctx context.Context, client bunkerv1connect.BunkerdClient,
		entry ServerEntry, agentID string, errOut, out io.Writer) error {
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { deliverSpawnToolsFunc = orig })

	cmd := &cobra.Command{Use: "test"}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	// The client is never dialed when the seam is stubbed; a client pointed at
	// nothing keeps the signature honest without a server.
	client := bunkerv1connect.NewBunkerdClient(http.DefaultClient, "http://127.0.0.1:1")
	err := applySpawnToolsDelivery(cmd, context.Background(), client, ServerEntry{Name: "default"}, "agt-loud")
	if err == nil {
		t.Fatal("a failed spawn-time delivery must return an error (the spawn command must exit non-zero)")
	}
	if !strings.Contains(errOut.String(), "EXISTS but is NOT provisioned") {
		t.Errorf("the loud verdict must state the agent is not provisioned, got:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "agent-tools agt-loud --install") {
		t.Errorf("the verdict must name the in-place repair, got:\n%s", errOut.String())
	}
	_ = v1.ExecAgentRequest{} // keep the wire-type import tied to the surface under test
}
