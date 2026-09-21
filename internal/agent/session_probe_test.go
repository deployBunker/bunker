package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// ── Unit proofs: the live probe's argv, bounds, and classification ──
// (no real ssh anywhere: the binary is a PATH stub, same pattern as the
// INT-CI-005 tests).

// TestProbeAgentSession_ExecPathOptions pins that the probe runs the EXACT
// target and options the exec path builds (buildSSHBaseCommand, the tunnel
// builder minus -L/-N), with a single-token remote command. No real ssh
// runs — the stub records its argv.
func TestProbeAgentSession_ExecPathOptions(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "ssh-argv")
	// Succeed immediately; only the argv matters here.
	writeStub(t, binDir, "ssh", "printf '%s\\n' \"$*\" >> "+logPath+"\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempts, err := probeAgentSessionLive(ctx, "abc123", "bunker-abc123", keyPath)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}

	lines := readRecord(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("ssh stub ran %d times, want 1 (argv log: %v)", len(lines), lines)
	}
	argv := lines[0]
	// The exec path target/options, element-for-element (buildSSHBaseCommand):
	for _, want := range []string{
		"-o StrictHostKeyChecking=no",
		"-o UserKnownHostsFile=/dev/null",
		"-o LogLevel=ERROR",
		"-o ConnectTimeout=10",
		"-i " + keyPath,
		"bunker-abc123@localhost",
		"whoami",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("probe argv %q missing exec-path element %q", argv, want)
		}
	}
}

// TestProbeAgentSession_AttemptBoundAndBackoff is the C3 proof: a probe
// that always fails is attempted exactly sessionProbeMaxAttempts times,
// with the 1s-then-2s backoff observable between attempts, and the final
// error is the last failure. No real ssh runs.
func TestProbeAgentSession_AttemptBoundAndBackoff(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "ssh-calls")
	// Always fail like a generic ssh failure (exit 255 + stderr) so the
	// retry loop — not the classifier — is what is under test.
	writeStub(t, binDir, "ssh", "printf '%s\\n' \"$0 $*\" >> "+logPath+"\necho 'stub ssh failure' >&2\nexit 255\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	attempts, err := probeAgentSessionLive(ctx, "bound1", "bunker-bound1", keyPath)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("probe returned nil although every attempt failed")
	}
	if attempts != sessionProbeMaxAttempts {
		t.Fatalf("attempts = %d, want exactly %d (the bound must be exact, not 'at most')", attempts, sessionProbeMaxAttempts)
	}
	if calls := len(readRecord(t, logPath)); calls != sessionProbeMaxAttempts {
		t.Errorf("ssh stub ran %d times, want %d", calls, sessionProbeMaxAttempts)
	}
	if !strings.Contains(err.Error(), "255") {
		t.Errorf("final error should carry the exit status, got: %v", err)
	}
	// Backoff proof: attempt 1 + 1s backoff + attempt 2 + 2s backoff +
	// attempt 3. The stub attempts are near-instant, so the total must
	// show at least the full 3s of backoff — and stay far below anything
	// unbounded.
	if elapsed < 3*time.Second {
		t.Errorf("probe returned after %s — the 1s+2s backoff between attempts did not run", elapsed)
	}
	if elapsed >= 35*time.Second {
		t.Errorf("probe returned after %s — exceeds the documented worst case (~33s)", elapsed)
	}
}

// TestProbeAgentSession_ContextCancelEndsRetry is the C3 deadline proof:
// the per-attempt contexts are DERIVED FROM the spawn ctx, so a request ctx
// that dies during the backoff ends the probe immediately instead of
// running the remaining attempts. No real ssh runs.
func TestProbeAgentSession_ContextCancelEndsRetry(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "ssh-calls")
	writeStub(t, binDir, "ssh", "printf '%s\\n' x >> "+logPath+"\nexit 255\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}

	// One near-instant attempt + up to 1s of backoff; the ctx dies
	// mid-backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	attempts, err := probeAgentSessionLive(ctx, "cancel1", "bunker-cancel1", keyPath)
	if err == nil {
		t.Fatal("probe returned nil although the ctx expired")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error should attribute the cancellation, got: %v", err)
	}
	if attempts > 1 {
		t.Errorf("attempts = %d, want <= 1: the probe kept retrying after the request ctx died", attempts)
	}
	if calls := len(readRecord(t, logPath)); calls > 1 {
		t.Errorf("ssh stub ran %d times, want <= 1", calls)
	}
}

// TestProbeAgentSession_HangingChildKilledByContext proves the bound on a
// hung child: an ssh stub that sleeps far past any deadline is killed by a
// context deadline (nothing leaks), the probe returns well before the sleep
// ends, and the error names the bound (never a bare "signal: killed"). The
// request-ctx half fires here (500ms); the 10s per-attempt deadline is the
// same context.WithTimeout derivation with a larger constant and is
// exercised by the demo-host live battery.
func TestProbeAgentSession_HangingChildKilledByContext(t *testing.T) {
	binDir := t.TempDir()
	writeStub(t, binDir, "ssh", "sleep 57\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := probeAgentSessionLive(ctx, "slow1", "bunker-slow1", keyPath)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("probe returned nil although the ssh child hung past the deadline")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("probe took %s — the hanging child was not killed by a context deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "cancelled") && !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should name the bound that fired, got: %v", err)
	}
}

// TestProbeAgentSession_SessionDenialClassification is the C4 proof on the
// probe side: the INT-DEMO-001 signature (exit 254, empty stderr, banner on
// stdout) is classified through the shared leaf package, so the probe error
// carries the exact session-denial diagnostic — the same text the exec path
// streams. No real ssh runs.
func TestProbeAgentSession_SessionDenialClassification(t *testing.T) {
	binDir := t.TempDir()
	// Reproduce the outage signature exactly: banner to stdout, nothing on
	// stderr, exit 254.
	writeStub(t, binDir, "ssh", "echo 'Last login: demo banner'\nexit 254\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The signature is deterministic, so the first attempt already
	// surfaces it; keep the ctx generous enough for all three attempts.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := probeAgentSessionLive(ctx, "pamdenied", "bunker-pamdenied", keyPath)
	if err == nil {
		t.Fatal("probe returned nil on the session-denial signature")
	}
	if !strings.Contains(err.Error(), "session was denied before the command ran") {
		t.Errorf("probe error does not carry the session-denial diagnostic: %v", err)
	}
	if !strings.Contains(err.Error(), "PAM") {
		t.Errorf("probe error does not name PAM: %v", err)
	}
}

// ── Spawn-level proofs (C1, C2) — root-gated: the spawn pipeline writes
// under /home/<user>/.ssh and runs real useradd/keygen, exactly like the
// existing root-gated battery (TestSpawn_SystemdUnitHasUlimits et al.).

// stubbedDockerdWait makes the dockerd-wait and container-cap stages
// deterministic (same pattern as manager_dockerd_test.go): no real dockerd,
// no systemd dependence.
func stubbedDockerdWait(t *testing.T) {
	t.Helper()
	oldChecker := dockerdProcessChecker
	dockerdProcessChecker = func(ctx context.Context, username string) (bool, error) { return true, nil }
	t.Cleanup(func() { dockerdProcessChecker = oldChecker })
	oldCount := countAgentContainers
	countAgentContainers = func(ctx context.Context, dockerSockPath string) (uint32, error) { return 0, nil }
	t.Cleanup(func() { countAgentContainers = oldCount })
}

// TestSpawn_SessionProbeFailureNeverReportsReady is the C2 fail-closed
// proof: the probe fails → Spawn returns an error naming the session-probe
// stage, no tracker record, no registry persist row, the standard rollback
// ran (the created user is gone), and the breadcrumb carries the probe
// stage. The pipeline is fully real (same machinery as the success test);
// only the probe is the injectable seam (no real ssh).
func TestSpawn_SessionProbeFailureNeverReportsReady(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges (drives the real spawn stages up to the probe)")
	}
	m := newTestManager(t)
	journal := redirectBreadcrumbJournal(t)

	agentID := uniqueAgentID("probefail")
	username := "bunker-" + agentID

	stubbedDockerdWait(t)

	origProbe := probeAgentSession
	probeAgentSession = func(ctx context.Context, aid, un, sshKeyPath string) (int, error) {
		if un != username {
			t.Errorf("probe called with username %q, want %q", un, username)
		}
		return 3, errors.New("stubbed probe failure: session denied")
	}
	t.Cleanup(func() { probeAgentSession = origProbe })

	resp, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
	if err == nil {
		t.Fatalf("Spawn() returned a ready response although the session probe failed: %+v", resp)
	}
	if !strings.Contains(err.Error(), "failed at stage "+StageSessionProbe) {
		t.Errorf("error does not name the %q stage: %v", StageSessionProbe, err)
	}
	if !strings.Contains(err.Error(), "stubbed probe failure") {
		t.Errorf("error does not carry the probe cause: %v", err)
	}

	// No tracker record was registered.
	if rec := m.tracker.Get(agentID); rec != nil {
		t.Errorf("tracker registered the agent although the probe failed: %+v", rec)
	}
	if got := m.tracker.Count(); got != 0 {
		t.Errorf("tracker holds %d agents, want 0", got)
	}

	// Nothing was persisted to the durable registry.
	if data, readErr := os.ReadFile(m.cfg.Agent.Registry.Path); readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
		t.Errorf("registry was written although the probe failed: %q", string(data))
	}

	// The standard rollback ran: the user the pipeline really created in
	// stage user-create is gone again.
	if _, lookupErr := user.Lookup(username); lookupErr == nil {
		t.Errorf("rollback left the created user %q behind (userdel never ran)", username)
	}

	// The breadcrumb attributes the failure to the probe stage.
	raw, readErr := os.ReadFile(journal)
	if readErr != nil {
		t.Fatalf("no spawn-failure breadcrumb was written: %v", readErr)
	}
	var bc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &bc); err != nil {
		t.Fatalf("breadcrumb is not a JSON line: %v (%q)", err, string(raw))
	}
	if bc["stage"] != StageSessionProbe {
		t.Errorf("breadcrumb stage = %v, want %q", bc["stage"], StageSessionProbe)
	}
	if bc["agent_id"] != agentID {
		t.Errorf("breadcrumb agent_id = %v, want %q", bc["agent_id"], agentID)
	}
}

// TestSpawn_SessionProbeSuccessReturnsReady is the C2 happy-path proof:
// the probe succeeds → Spawn returns the normal ready response, the agent
// is registered with Status "running", and the spawn is persisted.
func TestSpawn_SessionProbeSuccessReturnsReady(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges (drives the real spawn stages up to the probe)")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("probeok")

	stubbedDockerdWait(t)

	origProbe := probeAgentSession
	probeAgentSession = func(ctx context.Context, aid, username, sshKeyPath string) (int, error) {
		if sshKeyPath == "" {
			t.Errorf("probe called with empty sshKeyPath")
		}
		if username != "bunker-"+aid {
			t.Errorf("probe username = %q, want %q", username, "bunker-"+aid)
		}
		return 1, nil
	}
	t.Cleanup(func() { probeAgentSession = origProbe })

	resp, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
	if err != nil {
		t.Fatalf("Spawn() failed although the probe succeeded: %v", err)
	}
	if resp.AgentId != agentID {
		t.Errorf("resp.AgentId = %q, want %q", resp.AgentId, agentID)
	}

	// Default leg: without ReturnSshPrivateKey the persisted key still lands on
	// disk, but the response must NOT carry the private key (GAP-128 contract).
	if resp.SshPrivateKey != "" {
		t.Errorf("default spawn response carries SshPrivateKey (%d bytes); want empty without ReturnSshPrivateKey", len(resp.SshPrivateKey))
	}
	keyPath := fmt.Sprintf("/etc/bunkerd/ssh/%s", agentID)
	content, readErr := os.ReadFile(keyPath)
	if readErr != nil {
		t.Fatalf("read persisted SSH key %s: %v", keyPath, readErr)
	}
	if !strings.HasPrefix(string(content), "-----BEGIN") {
		t.Errorf("persisted SSH key doesn't start with -----BEGIN: %q", string(content)[:50])
	}
	rec := m.tracker.Get(agentID)
	if rec == nil {
		t.Fatalf("tracker has no record for the successfully spawned agent")
	}
	if rec.Status != "running" {
		t.Errorf("record status = %q, want running", rec.Status)
	}
	data, readErr := os.ReadFile(m.cfg.Agent.Registry.Path)
	if readErr != nil || len(strings.TrimSpace(string(data))) == 0 {
		t.Errorf("registry has no spawn row for a successful spawn (read: %v)", readErr)
	}
	cleanupAgent(t, m, agentID)
}
