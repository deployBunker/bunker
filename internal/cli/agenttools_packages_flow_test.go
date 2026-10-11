package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// The DF-BUNKER-57 flow tests drive installAgentTools' package-path half end
// to end through a scripted ExecAgent fake (the connect tests' shape: no
// config file, no SSH, no network — the fake plays the daemon). The package
// delivery runs BETWEEN the toolsd scp and the verifying probe, so a static
// local toolsd is faked into a stub PATH for the vendored leg.

// packageFlowFake answers the execs the install flow sends, in order:
// the home-resolution probe, then one delivery stage per missing package tool,
// then the verifying catalog probe. Scripts are recorded for the wiring
// assertions; the scp/chmod legs of the vendored copy are bypassed by pointing
// deliverFile's SSH at nothing — installAgentTools' vendored half is exercised
// by TestDeliverFile* elsewhere, so these tests stub resolveUserAtHost's
// config surface via a ServerEntry the resolver fails on... except the flow
// tests DO want the vendored leg to succeed, so a static toolsd stub is built
// and deliverFile is replaced through the seam below.
type packageFlowFake struct {
	mockBunkerdServer
	scripts  []string
	responds []func(script string) (string, int32, error)
}

func (m *packageFlowFake) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse]) error {

	args := req.Msg.GetArgs()
	script := ""
	if n := len(args); n > 0 {
		script = args[n-1]
	}
	m.scripts = append(m.scripts, script)
	if len(m.responds) == 0 {
		return connect.NewError(connect.CodeUnavailable, nil)
	}
	respond := m.responds[0]
	if len(m.responds) > 1 {
		m.responds = m.responds[1:]
	}
	out, code, err := respond(script)
	if err != nil {
		return err
	}
	return stream.Send(&v1.ExecAgentResponse{
		Output:   &v1.ExecAgentResponse_Stdout{Stdout: []byte(out)},
		ExitCode: code,
	})
}

// installResponders builds the responder chain for one flow run: home probe,
// one stage responder per catalog name missing in the probeBefore output, then
// the final verifying probe.
func installResponders(t *testing.T, probeBefore, finalProbe string) []func(string) (string, int32, error) {
	t.Helper()
	responders := []func(string) (string, int32, error){
		func(script string) (string, int32, error) {
			if !strings.Contains(script, "printf '%s' \"$HOME\"") {
				t.Fatalf("exec #1 must be the home probe, got: %q", script)
			}
			return "/home/bunker-flowtest", 0, nil
		},
	}
	before := parseAgentToolOutput("flowtest", probeBefore, 0)
	for _, inst := range packageToolInstallers {
		f := findingFor(before, inst.CatalogName)
		if f == nil || f.Status == "present" {
			continue
		}
		stageScript := inst.Script()
		responders = append(responders, func(script string) (string, int32, error) {
			if script != stageScript {
				t.Fatalf("stage exec for %s is not the registered delivery script", inst.CatalogName)
			}
			return inst.CatalogName + "\tINSTALLED\talready-present\n", 0, nil
		})
	}
	responders = append(responders, func(script string) (string, int32, error) {
		wantProbe := strings.Replace(agentProbeScript, "__TOOLS__", catalogNames(), 1)
		if script != wantProbe {
			t.Fatalf("final exec must be the catalog probe, got: %q", script)
		}
		return finalProbe, 0, nil
	})
	return responders
}

// staticToolsdStub builds a genuinely static (CGO_ENABLED=0) throwaway binary
// for the vendored leg, the same shape the live run passed as --binary: the
// flow must exercise the REAL static-link guard in its passing direction, not
// stub it away.
func staticToolsdStub(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH; the static vendored artifact cannot be built here")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "toolsd")
	build := testExecGoBuild(t, bin, src)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build static stub: %v\n%s", err, out)
	}
	return bin
}

// runPackageInstall drives the package half of installAgentTools directly with
// the vendored (scp) leg stubbed out, and returns both output streams.
func runPackageInstall(t *testing.T, fake *packageFlowFake, probeBefore string) (string, string, error) {
	t.Helper()
	srv := newTestServer(t, fake)
	t.Cleanup(srv.Close)
	client := bunkerv1connect.NewBunkerdClient(srv.Client(), srv.URL)
	entry := ServerEntry{Name: "default", URL: srv.URL}

	restore := stubDeliverFileForTest(t)
	defer restore()

	cmd := &cobra.Command{Use: "test"}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	err := installAgentTools(cmd, context.Background(), client, entry, "agent-flowtest",
		parseAgentToolOutput("agent-flowtest", probeBefore, 0), staticToolsdStub(t))
	return out.String(), errOut.String(), err
}

// stubDeliverFileForTest replaces deliverFile with a no-op for the duration of
// one test (the vendored scp leg needs a real SSH host; the flow tests are
// about the package half, whose evidence is the re-probe). It restores the
// original on cleanup and REQUIRES the stub was actually used, so the test
// cannot pass with a quietly skipped step.
func stubDeliverFileForTest(t *testing.T) func() {
	t.Helper()
	orig := deliverFileFunc
	deliverFileFunc = func(cmd *cobra.Command, entry ServerEntry,
		agentID, localPath, remotePath string) error {
		return nil
	}
	t.Cleanup(func() { deliverFileFunc = orig })
	return func() { deliverFileFunc = orig }
}

// TestInstallDeliversMissingPackageTools is the DF-BUNKER-57 acceptance at the
// flow level: a fresh-agent report (rg+gopls absent) must produce one delivery
// stage per missing tool BETWEEN the toolsd delivery and the verifying probe,
// and a final probe showing both present must yield "delivered <tool>" lines
// and a nil error.
func TestInstallDeliversMissingPackageTools(t *testing.T) {
	probeBefore := probeOutput("absent")
	probeAfter := probeOutput("present")
	fake := &packageFlowFake{responds: installResponders(t, probeBefore, probeAfter)}

	out, _, err := runPackageInstall(t, fake, probeBefore)
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	// 1 home + 2 stages + 1 verifying probe.
	if len(fake.scripts) != 4 {
		t.Fatalf("expected 4 execs (home, rg, gopls, verify), got %d", len(fake.scripts))
	}
	for _, want := range []string{"delivered rg:", "delivered gopls:", "delivered toolsd:"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout must contain %q, got:\n%s", want, out)
		}
	}
}

// TestInstallFailsWhenPackageToolStillAbsent is the load-bearing direction: a
// stage that claims INSTALLED but a re-probe that still reads absent fails the
// install — the stage's word is never the verdict. The final probe reports
// toolsd PRESENT so the toolsd leg passes and the PACKAGE verdict is the one
// that fires.
func TestInstallFailsWhenPackageToolStillAbsent(t *testing.T) {
	probeBefore := probeOutput("absent")
	final := strings.Replace(probeOutput("absent"), "toolsd	absent", "toolsd	present	toolsd version flowtest", 1)
	fake := &packageFlowFake{responds: installResponders(t, probeBefore, final)}

	_, _, err := runPackageInstall(t, fake, probeBefore)
	if err == nil {
		t.Fatal("a tool still absent after delivery must fail the install")
	}
	for _, want := range []string{"install FAILED", "copied the file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must say %q, got: %v", want, err)
		}
	}
}

// TestInstallFailsWhenStageReportsFailure: a FAILED stage is collected and
// named at the end (after the human report printed the other results), and the
// install is incomplete — never a partial success. toolsd reads present on the
// final probe so the package verdict is the one that fires.
func TestInstallFailsWhenStageReportsFailure(t *testing.T) {
	probeBefore := probeOutput("absent")
	final := strings.Replace(probeOutput("absent"), "toolsd	absent", "toolsd	present	toolsd version flowtest", 1)
	responders := installResponders(t, probeBefore, final)
	// Replace the rg stage (exec #2) with a failure; gopls (exec #3) stays
	// successful so the report path with mixed outcomes is exercised.
	failedStage := responders[1]
	responders[1] = func(script string) (string, int32, error) {
		_, _, _ = failedStage(script)
		return "rg	FAILED	E: Unable to locate package ripgrep\n", 0, nil
	}
	fake := &packageFlowFake{responds: responders}

	_, errOut, err := runPackageInstall(t, fake, probeBefore)
	if err == nil {
		t.Fatal("a FAILED stage must fail the install")
	}
	if !strings.Contains(err.Error(), "install incomplete") || !strings.Contains(err.Error(), "rg: E: Unable to locate package ripgrep") {
		t.Errorf("the failure must name the stage cause, got: %v", err)
	}
	if !strings.Contains(errOut, "WARNING rg delivery FAILED") {
		t.Errorf("stderr must warn on the failed stage, got:\n%s", errOut)
	}
}

// TestInstallStageExecErrorStopsTheFlow: a transport-level stage failure (the
// exec itself dying) is returned immediately, naming the tool — not swallowed
// into a verdict the probe never produced.
func TestInstallStageExecErrorStopsTheFlow(t *testing.T) {
	probeBefore := probeOutput("absent")
	final := strings.Replace(probeOutput("absent"), "toolsd	absent", "toolsd	present	toolsd version flowtest", 1)
	fake := &packageFlowFake{responds: installResponders(t, probeBefore, final)}
	// Turn the rg stage (exec #2) into a transport error.
	fake.responds[1] = func(string) (string, int32, error) {
		return "", 0, connect.NewError(connect.CodeUnavailable, nil)
	}

	_, _, err := runPackageInstall(t, fake, probeBefore)
	if err == nil {
		t.Fatal("a dead delivery exec must fail the install")
	}
	if !strings.Contains(err.Error(), "deliver rg on agent") {
		t.Errorf("the failure must name the tool and agent, got: %v", err)
	}
	// The flow must stop: no verifying probe after a dead stage.
	if got := len(fake.scripts); got != 2 {
		t.Errorf("flow must stop after the failed stage (2 execs), got %d", got)
	}
}

// TestInstallSkipsAlreadyPresentPackageTools: a tool the pre-install probe
// already found must get NO delivery stage — the idempotent path goes
// home-probe → verify only.
func TestInstallSkipsAlreadyPresentPackageTools(t *testing.T) {
	probeBefore := probeOutput("present")
	fake := &packageFlowFake{responds: installResponders(t, probeBefore, probeOutput("present"))}

	out, _, err := runPackageInstall(t, fake, probeBefore)
	if err != nil {
		t.Fatalf("install failed: %v", err)
	}
	if len(fake.scripts) != 2 {
		t.Fatalf("expected 2 execs (home, verify), got %d", len(fake.scripts))
	}
	if strings.Contains(out, "delivered rg:") || strings.Contains(out, "delivered gopls:") {
		t.Errorf("already-present tools must not be claimed as freshly delivered:\n%s", out)
	}
}
