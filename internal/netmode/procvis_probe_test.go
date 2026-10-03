package netmode

// Tests for the procvis mode's host verifier (NET-BUNKER-011).
//
// HONESTY NOTE (spec §6.3, and the brief): the visibility property is proved
// by the LIVE kernel experiment, not by a hermetic stub. The hermetic tests
// below pin the script's contract and the refusal classification; the live
// tests (the two that need mount namespaces) prove the kernel actually
// honors and reports hidepid=2 and that the control arm refuses a torn
// experiment. On a host without mount-namespace capability the live tests
// SKIP with the named reason — they never pass vacuously.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// procvisProbeEnvOK reports whether this environment can run the experiment
// AT ALL: a private mount namespace whose shell is root (the same wrapper
// shape runProcVisProbe builds). It deliberately does NOT test hidepid —
// that is what the live tests below prove (a kernel without hidepid support
// must FAIL the mode's live tests, not skip them, because the daemon must
// refuse the mode there).
func procvisProbeEnvOK(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	defer os.RemoveAll(dir)
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command("unshare", "--mount", "--propagation", "private", "sh", "-c",
			`mount -t proc proc "$1" && grep -q ' proc ' "$1/self/mounts"`, "envgate", dir)
	} else {
		cmd = exec.Command("unshare", "--map-root-user", "--mount", "--propagation", "private", "sh", "-c",
			`mount -t proc proc "$1" && grep -q ' proc ' "$1/self/mounts"`, "envgate", dir)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("procvis probe environment unavailable: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return err == nil
}

// TestProcVisHostExperimentProvesHidepidVisibility is THE visibility test:
// it runs the REAL experiment on the REAL kernel. Passing means this host's
// kernel honored hidepid=2 on a private procfs instance, REPORTED the option
// back through /proc/<pid>/mounts read through the new instance, and actually
// HID a foreign-uid process from a readdir while the control arm still saw
// it — i.e. the mechanism the procvis mode's units rely on is real here.
// A kernel that silently ignores hidepid reddens this test (exit 1), which
// is correct: procvis must be refused on such a host.
func TestProcVisHostExperimentProvesHidepidVisibility(t *testing.T) {
	if !procvisProbeEnvOK(t) {
		t.Skip("mount-namespace experiment unavailable in this environment (no unshare/mount capability for this uid) — the live visibility proof is skipped, not passed vacuously")
	}
	err := VerifyProcVisHost()
	if err != nil {
		t.Fatalf("the live kernel experiment failed — procvis must be refused on this host: %v", err)
	}
}

// TestProcVisProbeRejectsDeadVictimExperiment proves the control arm is
// load-bearing. The classic false positive in hidepid testing: the foreign
// victim process DIES (here: a PATH-stubbed `sleep 5` that exits instantly),
// so the hidden arm "passes" for the wrong reason (the process is absent
// because it is dead, not because it is hidden). The pinned experiment must
// REFUSE that: its control arm requires the victim to be visible without
// hiding, and maps its absence to exit 2, which classifies as the
// control-arm refusal — the torn experiment can never read as a pass.
func TestProcVisProbeRejectsDeadVictimExperiment(t *testing.T) {
	if !procvisProbeEnvOK(t) {
		t.Skip("mount-namespace experiment unavailable in this environment — skipped, not passed vacuously")
	}
	binDir := t.TempDir()
	// A sleep that dies instantly when asked for 5 seconds (the victim's
	// duration) and behaves normally otherwise (the script's pacing waits).
	fake := "#!/bin/sh\nif [ \"$1\" = \"5\" ]; then exit 0; fi\nexec /bin/sleep \"$@\"\n"
	if err := os.WriteFile(binDir+"/sleep", []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake sleep: %v", err)
	}
	dir := t.TempDir()
	defer os.RemoveAll(dir)

	cmd := exec.Command("unshare", "--map-root-user", "--mount", "--propagation", "private", "sh", "-c",
		ProcVisProbeScript(), "procvis-probe", dir)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an experiment whose victim died (control arm cannot see it) PASSED — the control arm is not load-bearing and a dead-victim run would read as isolation; output: %s", out)
	}
	classified := classifyProcVisProbe(err, out)
	if !strings.Contains(classified.Error(), "control arm") {
		t.Fatalf("dead-victim experiment was not classified as the control-arm refusal: %v (output: %s)", classified, out)
	}
}

// TestProcVisProbeScriptContractPins pins the experiment's load-bearing
// structure in text, so a maintenance edit cannot silently drop the
// kernel's ack (the hidepid=2 read-back), the foreign-uid victim, or the
// control arm (the dead-victim defense).
func TestProcVisProbeScriptContractPins(t *testing.T) {
	s := ProcVisProbeScript()
	for _, want := range []string{
		"hidepid=2",                // the hidden arm's mount option
		`hidepid=(2|invisible)`,    // the ack accepts BOTH reported spellings (Ubuntu 6.8 normalizes to the level name)
		`"$P/self/mounts"`,         // the ack is read THROUGH the new instance
		"exit 1",                   // unreported/ignored hidepid => refusal
		"--reuid=1",                // the victim is a FOREIGN uid
		"--reuid=2",                // the observer is foreign too (never root)
		"hidepid=0",                // the control arm re-mounts without hiding
		`"$FOUND" = 1 ] || exit 2`, // a control that cannot see the victim fails the experiment
	} {
		if !strings.Contains(s, want) {
			t.Errorf("probe script lost its %q contract:\n%s", want, s)
		}
	}
	// The control arm must be REACHED only after the hidden arm passed:
	// control-arm text comes after hidden-arm text.
	hiddenIdx := strings.Index(s, "HIDDEN")
	controlIdx := strings.Index(s, "FOUND")
	if hiddenIdx < 0 || controlIdx < 0 || controlIdx < hiddenIdx {
		t.Errorf("probe script control arm is not positioned after the hidden arm (hidden=%d control=%d)", hiddenIdx, controlIdx)
	}
}

// TestProcVisProbeClassificationNamesTheFailure pins the refusal texts: a
// kernel that ignores hidepid must be named as a would-be SILENT NO-OP (the
// manufactured-bound class), a control-arm failure must name the control
// arm, and an environment failure must say the probe could not run.
func TestProcVisProbeClassificationNamesTheFailure(t *testing.T) {
	// mkExit builds a REAL *exec.ExitError with the given exit code by
	// running a shell that exits with it.
	mkExit := func(code int) *exec.ExitError {
		cmd := exec.Command("sh", "-c", "exit "+itoa(code))
		_ = cmd.Run()
		return &exec.ExitError{ProcessState: cmd.ProcessState}
	}

	e1 := classifyProcVisProbe(mkExit(1), []byte("grep output"))
	if e1 == nil || !strings.Contains(e1.Error(), "silent no-op") {
		t.Errorf("exit 1 classified as %v, want the silent-no-op hidepid refusal", e1)
	}
	e2 := classifyProcVisProbe(mkExit(2), nil)
	if e2 == nil || !strings.Contains(e2.Error(), "control arm") {
		t.Errorf("exit 2 classified as %v, want the control-arm refusal", e2)
	}
	e3 := classifyProcVisProbe(mkExit(9), nil)
	if e3 == nil || !strings.Contains(e3.Error(), "probe environment failed") {
		t.Errorf("exit 9 classified as %v, want the environment-failure refusal", e3)
	}
	// A non-exit failure (e.g. binary missing) is an environment failure too.
	e4 := classifyProcVisProbe(exec.Command("/definitely/not/a/binary").Run(), nil)
	if e4 == nil || !strings.Contains(e4.Error(), "probe") {
		t.Errorf("wrapper failure classified as %v, want a probe failure", e4)
	}
}

// itoa avoids importing strconv for three digits.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// TestVerifyProcVisHostReturnsTheProbesVerdict pins the wiring: the exported
// verifier surfaces the runner's verdict (a refusal is a refusal, never nil).
func TestVerifyProcVisHostReturnsTheProbesVerdict(t *testing.T) {
	orig := procvisProbeRunner
	defer func() { procvisProbeRunner = orig }()
	want := &ProcVisProbeError{Detail: "stub refusal"}
	procvisProbeRunner = func() error { return want }
	got := VerifyProcVisHost()
	if got != error(want) {
		t.Fatalf("VerifyProcVisHost() = %v, want the runner's own refusal %v", got, want)
	}
}
