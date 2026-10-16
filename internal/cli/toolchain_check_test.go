package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/deployBunker/bunker/internal/provisioning"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// The SURF-009 flow tests drive `bunker toolchain-check` end to end through
// a scripted ExecAgent fake (the connect tests' shape: no network, the fake
// plays the daemon). The declared set's probe and the per-tool stages are
// asserted by script IDENTITY, not by count: an exec that is not the
// registered probe/delivery program fails the test.

const toolchainAllPresentProbe = "python3\tpresent\tPython 3.11.15\n" +
	"go\tpresent\tgo version go1.25.4 linux/amd64\n" +
	"gcc\tpresent\tgcc (Ubuntu 13.2.0-23ubuntu4) 13.2.0\n" +
	"make\tpresent\tGNU Make 4.3\n" +
	"node\tpresent\tv20.20.2\n" +
	"cargo\tpresent\tcargo 1.89.0\n"

const toolchainAllAbsentProbe = "python3\tabsent\t\n" +
	"go\tabsent\t\n" +
	"gcc\tabsent\t\n" +
	"make\tabsent\t\n" +
	"node\tabsent\t\n" +
	"cargo\tabsent\t\n"

// toolchainGoClassProbe is the shape the live proof measured: a box that can
// build Go (and has the baseline interpreter) but has no C, JS or Rust chain.
const toolchainGoClassProbe = "python3\tpresent\tPython 3.11.15\n" +
	"go\tpresent\tgo version go1.25.4 linux/amd64\n" +
	"gcc\tabsent\t\n" +
	"make\tpresent\tGNU Make 4.3\n" +
	"node\tabsent\t\n" +
	"cargo\tabsent\t\n"

// toolchainFlowFake answers the execs the command sends, in order, and
// records every script for the wiring assertions.
type toolchainFlowFake struct {
	mockBunkerdServer
	scripts  []string
	responds []func(script string) (string, int32, error)
	execErr  error
}

func (m *toolchainFlowFake) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse]) error {

	args := req.Msg.GetArgs()
	script := ""
	if n := len(args); n > 0 {
		script = args[n-1]
	}
	m.scripts = append(m.scripts, script)
	if m.execErr != nil {
		return m.execErr
	}
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

// runToolchainCommand drives the cobra command with a saved single-server
// config pointing at the fake, and returns both output streams.
func runToolchainCommand(t *testing.T, fake *toolchainFlowFake, args ...string) (string, string, error) {
	t.Helper()
	srv := newTestServer(t, fake)
	t.Cleanup(srv.Close)

	home := useTempHome(t)
	_ = home
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"default": {Name: "default", URL: srv.URL, Token: "tok-test"},
		},
		ActiveServer: "default",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewToolchainCheckCommand()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestToolchainCheckReportsAllPresentByName is the acceptance shape: one
// command, every declared name, explicit verdict.
func TestToolchainCheckReportsAllPresentByName(t *testing.T) {
	fake := &toolchainFlowFake{responds: []func(string) (string, int32, error){
		func(script string) (string, int32, error) {
			if script != provisioning.ProbeScript() {
				t.Fatalf("exec is not the declared-set probe: %.80q", script)
			}
			return toolchainAllPresentProbe, 0, nil
		},
	}}
	out, _, err := runToolchainCommand(t, fake, "agt-check", "--json")
	if err != nil {
		t.Fatalf("all-present check must exit 0: %v", err)
	}
	var report provisioning.Report
	if jerr := json.Unmarshal([]byte(out), &report); jerr != nil {
		t.Fatalf("json report: %v\n%s", jerr, out)
	}
	if !report.AllPresent {
		t.Errorf("all_present = false, want true (%v)", report.Missing)
	}
	if report.Class != provisioning.ClassFull {
		t.Errorf("class = %q, want the default %q", report.Class, provisioning.ClassFull)
	}
	var names []string
	for _, f := range report.Tools {
		names = append(names, f.Name)
		if f.Status != provisioning.StatusPresent {
			t.Errorf("%s status = %q, want %q", f.Name, f.Status, provisioning.StatusPresent)
		}
	}
	if got := strings.Join(names, ","); got != "python3,go,gcc,make,node,cargo" {
		t.Errorf("report names = %q, want the declared set", got)
	}
}

// TestToolchainCheckHumanOutputNamesEveryTool: the human form is one line
// per tool with PRESENT or ABSENT, exactly as the row asks.
func TestToolchainCheckHumanOutputNamesEveryTool(t *testing.T) {
	fake := &toolchainFlowFake{responds: []func(string) (string, int32, error){
		func(string) (string, int32, error) { return toolchainGoClassProbe, 0, nil },
	}}
	out, _, err := runToolchainCommand(t, fake, "agt-check", "--class", provisioning.ClassGo)
	if err != nil {
		t.Fatalf("class go on a Go-capable box must exit 0: %v", err)
	}
	for _, want := range []string{
		"python3  PRESENT  yes", "go       PRESENT  yes", "make     PRESENT  yes",
		"gcc      ABSENT   no", "node     ABSENT   no", "cargo    ABSENT   no",
		"class go: every required toolchain is PRESENT (python3, go, make)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

// TestToolchainCheckIncompleteIsTheGate pins the acceptance exit semantics:
// the default is a GATE — an incomplete class exits 1 and names what is
// missing.
func TestToolchainCheckIncompleteIsTheGate(t *testing.T) {
	fake := &toolchainFlowFake{responds: []func(string) (string, int32, error){
		func(string) (string, int32, error) { return toolchainAllAbsentProbe, 0, nil },
	}}
	out, _, err := runToolchainCommand(t, fake, "agt-check")
	if err == nil {
		t.Fatal("an incomplete class must exit non-zero (the gate), got nil")
	}
	for _, want := range []string{"cargo", "gcc", "go", "make", "node", "python3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the gate error must name %s: %v", want, err)
		}
	}
	if !strings.Contains(out, "MISSING on the agent: cargo, gcc, go, make, node, python3") {
		t.Errorf("the report must still list every missing name:\n%s", out)
	}
	if !strings.Contains(out, "--deliver --class full") {
		t.Errorf("the report must carry the remediation line:\n%s", out)
	}
}

// TestToolchainCheckReportOnlyIsData: --report-only keeps the reporting
// contract for callers that want the answer as data.
func TestToolchainCheckReportOnlyIsData(t *testing.T) {
	fake := &toolchainFlowFake{responds: []func(string) (string, int32, error){
		func(string) (string, int32, error) { return toolchainAllAbsentProbe, 0, nil },
	}}
	out, _, err := runToolchainCommand(t, fake, "agt-check", "--report-only")
	if err != nil {
		t.Fatalf("--report-only must never fail on a finding: %v", err)
	}
	if !strings.Contains(out, "MISSING on the agent:") {
		t.Errorf("the data form must still name the missing set:\n%s", out)
	}
}

// TestToolchainCheckUnknownClassFailsLocally: a class that is not declared is
// a usage error before any RPC — never a silent fallback to a default.
func TestToolchainCheckUnknownClassFailsLocally(t *testing.T) {
	fake := &toolchainFlowFake{}
	_, _, err := runToolchainCommand(t, fake, "agt-check", "--class", "everything")
	if err == nil || !strings.Contains(err.Error(), "unknown toolchain class") {
		t.Fatalf("unknown class must fail locally, got %v", err)
	}
	for _, want := range provisioning.ClassNames() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name the declared class %q: %v", want, err)
		}
	}
	if len(fake.scripts) != 0 {
		t.Errorf("no RPC may be sent for an unknown class, got %d execs", len(fake.scripts))
	}
}

// TestToolchainCheckUnreachableAgentFails: only a probe that could not RUN
// exits non-zero whatever the mode.
func TestToolchainCheckUnreachableAgentFails(t *testing.T) {
	fake := &toolchainFlowFake{execErr: connect.NewError(connect.CodeUnavailable, nil)}
	for _, mode := range [][]string{{}, {"--report-only"}} {
		_, _, err := runToolchainCommand(t, fake, append([]string{"agt-check"}, mode...)...)
		if err == nil {
			t.Fatalf("a probe that could not run must fail (mode %v)", mode)
		}
		if !strings.Contains(err.Error(), "toolchain") {
			t.Errorf("failure must name the surface: %v", err)
		}
	}
}

// TestToolchainCheckRejectsBadAgentID: local validation fires before any RPC.
func TestToolchainCheckRejectsBadAgentID(t *testing.T) {
	fake := &toolchainFlowFake{}
	_, _, err := runToolchainCommand(t, fake, "NOT-a-valid-id")
	if err == nil || !strings.Contains(err.Error(), "invalid agent id") {
		t.Fatalf("bad agent id must fail locally, got %v", err)
	}
	if len(fake.scripts) != 0 {
		t.Errorf("no RPC may be sent for a locally-invalid id, got %d execs", len(fake.scripts))
	}
}

// deliverableNames are the catalog entries with a delivery stage — the only
// ones --deliver may exec.
func deliverableNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, tc := range provisioning.Catalog() {
		if tc.Script == nil {
			continue
		}
		names = append(names, tc.Name)
	}
	return names
}

// deliverResponders builds the responder chain for --deliver: probe(absent),
// one stage per deliverable missing name (script asserted identical),
// probe(present).
func deliverResponders(t *testing.T, probeBefore, probeAfter string) []func(string) (string, int32, error) {
	t.Helper()
	responders := []func(string) (string, int32, error){
		func(script string) (string, int32, error) {
			if script != provisioning.ProbeScript() {
				t.Fatalf("exec #1 must be the declared-set probe, got %.80q", script)
			}
			return probeBefore, 0, nil
		},
	}
	for _, name := range deliverableNames(t) {
		stageScript, serr := provisioning.StageScript(name)
		if serr != nil {
			t.Fatalf("stage for %s: %v", name, serr)
		}
		nm := name
		responders = append(responders, func(script string) (string, int32, error) {
			if script != stageScript {
				t.Fatalf("stage exec for %s is not the registered delivery script", nm)
			}
			return nm + "\tINSTALLED\talready-present\n", 0, nil
		})
	}
	responders = append(responders,
		func(script string) (string, int32, error) {
			if script != provisioning.ProbeScript() {
				t.Fatalf("verdict exec must be the probe, got %.80q", script)
			}
			return probeAfter, 0, nil
		},
		func(string) (string, int32, error) { return probeAfter, 0, nil },
	)
	return responders
}

// TestToolchainDeliverFlowVerifiesByReprobe drives the full delivery: the
// verdict must come from the RE-PROBE (the final probe responder), not from
// the stages' own words.
func TestToolchainDeliverFlowVerifiesByReprobe(t *testing.T) {
	fake := &toolchainFlowFake{responds: deliverResponders(t, toolchainAllAbsentProbe, toolchainAllPresentProbe)}
	_, errOut, err := runToolchainCommand(t, fake, "agt-check", "--deliver")
	if err != nil {
		t.Fatalf("delivery with a clean re-probe must succeed: %v\n%s", err, errOut)
	}
	// 1 probe + 5 stages (python3 has none) + 1 verdict probe + 1 final
	// report probe = 8 execs.
	if len(fake.scripts) != 8 {
		t.Errorf("exec count = %d, want 8 (probe, 5 stages, verdict probe, report probe)", len(fake.scripts))
	}
	if !strings.Contains(errOut, "delivered (already-present)") {
		t.Errorf("progress lines must name each delivered tool:\n%s", errOut)
	}
}

// TestToolchainDeliverCustomClassRunsOnlyThatClass: --deliver for the go
// class must touch go and make only — no C, JS or Rust download.
func TestToolchainDeliverCustomClassRunsOnlyThatClass(t *testing.T) {
	// Before: go missing (python3 present, make present); after: all green.
	// Only the go stage may run.
	goStage, err := provisioning.StageScript("go")
	if err != nil {
		t.Fatalf("go stage: %v", err)
	}
	before := "python3\tpresent\tPython 3.11.15\n" +
		"go\tabsent\t\n" +
		"gcc\tabsent\t\n" +
		"make\tpresent\tGNU Make 4.3\n" +
		"node\tabsent\t\n" +
		"cargo\tabsent\t\n"
	fake := &toolchainFlowFake{responds: []func(string) (string, int32, error){
		func(script string) (string, int32, error) {
			if script != provisioning.ProbeScript() {
				t.Fatalf("exec #1 must be the probe, got %.80q", script)
			}
			return before, 0, nil
		},
		func(script string) (string, int32, error) {
			if script != goStage {
				t.Fatalf("class go must run the go stage only, got %.80q", script)
			}
			return "go\tINSTALLED\tgo version go1.25.4 linux/amd64\n", 0, nil
		},
		func(script string) (string, int32, error) {
			if script != provisioning.ProbeScript() {
				t.Fatalf("verdict exec must be the probe, got %.80q", script)
			}
			return toolchainGoClassProbe, 0, nil
		},
		func(string) (string, int32, error) { return toolchainGoClassProbe, 0, nil },
	}}
	_, errOut, err := runToolchainCommand(t, fake, "agt-check", "--deliver", "--class", provisioning.ClassGo)
	if err != nil {
		t.Fatalf("go-class delivery must succeed: %v\n%s", err, errOut)
	}
	if len(fake.scripts) != 4 {
		t.Errorf("exec count = %d, want 4 (probe, go stage, verdict probe, report probe)", len(fake.scripts))
	}
	for _, s := range fake.scripts {
		for _, name := range []string{"gcc", "node", "cargo"} {
			stage, _ := provisioning.StageScript(name)
			if s == stage {
				t.Errorf("class go must not run the %s stage", name)
			}
		}
	}
}

// TestToolchainDeliverIncompleteFails: stages may claim INSTALLED, but a
// still-absent re-probe fails the command with the names.
func TestToolchainDeliverIncompleteFails(t *testing.T) {
	fake := &toolchainFlowFake{responds: deliverResponders(t, toolchainAllAbsentProbe, toolchainAllAbsentProbe)}
	_, _, err := runToolchainCommand(t, fake, "agt-check", "--deliver")
	if err == nil {
		t.Fatal("a still-absent verdict must fail")
	}
	if !strings.Contains(err.Error(), "still absent after delivery") {
		t.Errorf("failure must name the still-absent set: %v", err)
	}
	if !strings.Contains(err.Error(), "class full") {
		t.Errorf("failure must name the class it verified: %v", err)
	}
}

// TestToolchainDeliverStageFailureNamed: the flow does not stop at the first
// bad stage (the agent-tools shape) — it keeps delivering, and the final
// re-probe verdict carries every stage's own words as the causes.
func TestToolchainDeliverStageFailureNamed(t *testing.T) {
	responders := []func(string) (string, int32, error){
		func(script string) (string, int32, error) {
			if script != provisioning.ProbeScript() {
				t.Fatalf("exec #1 must be the probe")
			}
			return toolchainAllAbsentProbe, 0, nil
		},
	}
	for _, name := range deliverableNames(t) {
		stage, _ := provisioning.StageScript(name)
		if name == "go" {
			responders = append(responders, func(script string) (string, int32, error) {
				if script != stage {
					t.Fatalf("expected the go stage, got %.80q", script)
				}
				return "go\tFAILED\ttoolchain checksum mismatch (expected 9fa5...)\n", 1, nil
			})
			continue
		}
		nm := name
		responders = append(responders, func(script string) (string, int32, error) {
			if script != stage {
				t.Fatalf("expected the %s stage, got %.80q", nm, script)
			}
			return nm + "\tINSTALLED\talready-present\n", 0, nil
		})
	}
	// The verdict re-probe: go is still absent (its stage failed).
	responders = append(responders, func(script string) (string, int32, error) {
		if script != provisioning.ProbeScript() {
			t.Fatalf("verdict exec must be the probe")
		}
		return toolchainAllAbsentProbe, 0, nil
	})
	fake := &toolchainFlowFake{responds: responders}
	_, _, err := runToolchainCommand(t, fake, "agt-check", "--deliver")
	if err == nil {
		t.Fatal("a failed stage must fail the delivery")
	}
	if !strings.Contains(err.Error(), "still absent after delivery") {
		t.Errorf("the re-probe verdict must gate the result: %v", err)
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("the stage's own words must reach the verdict as a cause: %v", err)
	}
}

// TestToolchainDeliverAlreadyPresentNoStages: an agent with the full set
// must not run a single delivery stage.
func TestToolchainDeliverAlreadyPresentNoStages(t *testing.T) {
	fake := &toolchainFlowFake{responds: []func(string) (string, int32, error){
		func(script string) (string, int32, error) { return toolchainAllPresentProbe, 0, nil },
		func(string) (string, int32, error) { return toolchainAllPresentProbe, 0, nil },
	}}
	_, errOut, err := runToolchainCommand(t, fake, "agt-check", "--deliver", "--class", provisioning.ClassFull)
	if err != nil {
		t.Fatalf("already-present delivery must succeed: %v", err)
	}
	for _, s := range fake.scripts[1:] {
		if _, isStage := hasStageScript(s); isStage {
			t.Errorf("no stage may run when the probe is clean, got one: %.60q", s)
		}
	}
	if !strings.Contains(errOut, "already present") {
		t.Errorf("must say nothing to deliver:\n%s", errOut)
	}
}

// hasStageScript reports whether s is one of the registered stage scripts.
func hasStageScript(s string) (string, bool) {
	for _, name := range provisioning.CatalogNames() {
		stage, err := provisioning.StageScript(name)
		if err == nil && s == stage {
			return name, true
		}
	}
	return "", false
}

// TestSpawnHasToolchainsFlag pins the spawn-path wiring: the flag exists,
// carries a CLASS, and defaults to no provisioning.
func TestSpawnHasToolchainsFlag(t *testing.T) {
	cmd := NewSpawnCommand()
	f := cmd.Flags().Lookup("toolchains")
	if f == nil {
		t.Fatal("spawn must carry --toolchains")
	}
	for _, want := range []string{"minimal", "go", "gcc", "make", "node", "cargo", "full"} {
		if !strings.Contains(f.Usage, want) {
			t.Errorf("flag help must name %q: %s", want, f.Usage)
		}
	}
	if f.DefValue != "" {
		t.Errorf("--toolchains must default to OFF (no provisioning; spawn compatibility), got %q", f.DefValue)
	}
	if f.NoOptDefVal != provisioning.ClassFull {
		t.Errorf("bare --toolchains must mean the full class, got NoOptDefVal %q", f.NoOptDefVal)
	}
}

// TestSpawnRejectsUnknownToolchainClass: a bogus class must fail the command
// before the spawn RPC (no agent created for a class nothing can build on).
func TestSpawnRejectsUnknownToolchainClass(t *testing.T) {
	cmd := NewSpawnCommand()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--toolchains=bogus", "--server", "nowhere"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown toolchain class") {
		t.Fatalf("spawn --toolchains=bogus must fail on the class, got %v", err)
	}
}
