package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/hostsetup"
	"github.com/deployBunker/bunker/internal/resource"
)

// hostRecorder is a fake host: it records every provisioning command the agent
// manager issues and answers the probes (mountpoint, getent, findmnt, id) from
// simulated state, so the isolation wiring can be asserted without root and
// without touching the machine running the tests.
type hostRecorder struct {
	calls        []string
	groupExists  bool
	members      map[string]bool
	mounted      map[string]bool
	opts         map[string]string
	failUsermod  bool
	failGroupadd bool

	// scratchRootOwner is the answer to `stat -c '%u:%g %a'` for the exchange
	// ROOT: "<uid>:<gid> <octal>". Per-path by construction (only the root is
	// ever asked), because a fake that answered the same shape for every path
	// could not tell a correct exchange root from the measured defect
	// (DF-BUNKER-40).
	scratchRootOwner string
}

func newHostRecorder() *hostRecorder {
	return &hostRecorder{
		members: map[string]bool{},
		mounted: map[string]bool{},
		opts:    map[string]string{},
	}
}

func (r *hostRecorder) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	switch name {
	case "getent":
		if r.groupExists {
			return []byte(args[1] + ":x:1001:"), nil
		}
		return nil, os.ErrNotExist
	case "groupadd":
		if r.failGroupadd {
			return []byte("groupadd: cannot lock /etc/group"), errors.New("exit status 1")
		}
		r.groupExists = true
		return nil, nil
	case "id":
		return []byte("users"), nil
	case "usermod":
		if r.failUsermod {
			return []byte("usermod: group does not exist"), errors.New("exit status 6")
		}
		r.members[args[len(args)-1]] = true
		return nil, nil
	case "mountpoint":
		if r.mounted[args[len(args)-1]] {
			return nil, nil
		}
		return nil, os.ErrNotExist
	case "findmnt":
		return []byte(r.opts[args[len(args)-1]]), nil
	case "mount":
		dir := args[len(args)-1]
		r.mounted[dir] = true
		r.opts[dir] = mountOptsFrom(args)
		return nil, nil
	case "umount":
		delete(r.mounted, args[len(args)-1])
		return nil, nil
	case "stat":
		// The exchange root's owner, as the host would report it. Empty means
		// "no answer" (a non-zero exit), which the gate reads as unobservable.
		return []byte(r.scratchRootOwner), nil
	default: // chown, chmod, systemctl, …
		return nil, nil
	}
}

// mountOptsFrom extracts the -o value of a recorded mount argv.
func mountOptsFrom(args []string) string {
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (r *hostRecorder) ran(command string) bool {
	for _, c := range r.calls {
		if c == command {
			return true
		}
	}
	return false
}

func (r *hostRecorder) ranPrefix(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// TestBuildRootlessDockerdArgs pins the exact per-agent dockerd unit argv:
// acceptance criterion A requires --property=PrivateTmp=yes in the spawn unit,
// and the rest of the argv must be unchanged apart from the TMPDIR value.
func TestBuildRootlessDockerdArgs(t *testing.T) {
	args, env := buildRootlessDockerdArgs(dockerdUnitArgs{
		AgentID:        "abc123",
		UnitName:       "bunker-docker-abc123",
		UID:            "1001",
		GID:            "1002",
		UserHome:       "/home/bunker-abc123",
		RuntimeDir:     "/run/bunker/abc123/run",
		DockerSockPath: "/run/bunker/abc123/docker.sock",
		RootlessBin:    "/home/bunker-abc123/bin/dockerd-rootless.sh",
		CPUQuota:       2.0,
		MemoryMax:      4294967296,
		DiskMax:        21474836480,
		MaxProcesses:   4096,
		MaxOpenFiles:   65536,
	})

	// The property block is pinned positionally: PrivateTmp must ride the same
	// systemd-run property list as the identity/PAM properties.
	wantHead := []string{
		"--system",
		"--unit=bunker-docker-abc123",
		"--uid=1001",
		"--gid=1002",
		"--property=PAMName=login",
		"--property=PrivateTmp=yes",
		"--property=CPUQuota=200%",
		"--property=MemoryMax=4294967296",
		"--property=LimitFSIZE=21474836480",
		"--property=TasksMax=4096",
		"--property=LimitNOFILE=65536:65536",
	}
	if len(args) < len(wantHead) {
		t.Fatalf("argv too short: %v", args)
	}
	for i, want := range wantHead {
		if args[i] != want {
			t.Fatalf("argv[%d] = %q, want %q\nfull argv: %v", i, args[i], want, args)
		}
	}

	// Every environment variable must be passed with --setenv (systemd-run
	// --system does not inherit the caller's environment) and TMPDIR must be
	// the enforced /tmp, never the legacy per-agent path.
	for _, e := range env {
		if !containsArg(args, "--setenv="+e) {
			t.Errorf("environment entry %q missing from argv", e)
		}
	}
	if !containsArg(args, "--setenv=TMPDIR="+config.IsolationTmpDir) {
		t.Errorf("argv missing --setenv=TMPDIR=%s: %v", config.IsolationTmpDir, args)
	}
	if strings.Contains(strings.Join(args, " "), "/run/bunker/abc123/tmp") {
		t.Errorf("argv still advertises the legacy TMPDIR: %v", args)
	}

	// The unit's command must remain the rootless dockerd entrypoint.
	if got, want := args[len(args)-2], "/home/bunker-abc123/bin/dockerd-rootless.sh"; got != want {
		t.Errorf("argv[-2] = %q, want %q", got, want)
	}
	if got, want := args[len(args)-1], "--host=unix:///run/bunker/abc123/docker.sock"; got != want {
		t.Errorf("argv[-1] = %q, want %q", got, want)
	}

	// Exactly one PrivateTmp property: a duplicate would be a copy/paste bug.
	if n := countArg(args, "--property=PrivateTmp=yes"); n != 1 {
		t.Errorf("PrivateTmp=yes appears %d times, want 1", n)
	}
}

// TestBuildRootlessDockerdArgs_NoLimitsOmitsLimitProperties keeps the
// no-limits shape honest: the properties we do not configure must be absent,
// not emitted as zeros.
func TestBuildRootlessDockerdArgs_NoLimitsOmitsLimitProperties(t *testing.T) {
	args, _ := buildRootlessDockerdArgs(dockerdUnitArgs{
		AgentID:        "abc123",
		UnitName:       "bunker-docker-abc123",
		UID:            "1001",
		GID:            "1001",
		UserHome:       "/home/bunker-abc123",
		RuntimeDir:     "/run/bunker/abc123/run",
		DockerSockPath: "/run/bunker/abc123/docker.sock",
		RootlessBin:    "/bin/true",
	})
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"CPUQuota", "MemoryMax", "LimitFSIZE", "TasksMax", "LimitNOFILE"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("unconfigured %s property was emitted: %v", forbidden, args)
		}
	}
	if !strings.Contains(joined, "--property=PrivateTmp=yes") {
		t.Errorf("PrivateTmp must be unconditional: %v", args)
	}
}

// TestRunAgentUnitHasPrivateTmp covers the detached-run unit: acceptance
// criterion A names RunAgent explicitly.
func TestRunAgentUnitHasPrivateTmp(t *testing.T) {
	args := buildRunAgentArgs("abc123", "1001", "1001", "bunker-run-abc123-deadbeef", "make", []string{"test"}, nil, nil, false)
	if n := countArg(args, "--property=PrivateTmp=yes"); n != 1 {
		t.Errorf("detached run argv has %d PrivateTmp properties, want 1: %v", n, args)
	}
	if !containsArg(args, "--setenv=TMPDIR="+config.IsolationTmpDir) {
		t.Errorf("detached run argv is not pointed at the enforced /tmp: %v", args)
	}
	if strings.Contains(strings.Join(args, " "), "/run/bunker/abc123/tmp") {
		t.Errorf("detached run argv still advertises the legacy TMPDIR: %v", args)
	}
}

// TestProvisionIsolation_BoundedScratchAndTmpInstance drives the manager's
// provisioning step against a fake host: the bounded scratch mount, the
// setgid ownership and the private-/tmp instance directory must all be issued.
func TestProvisionIsolation_BoundedScratchAndTmpInstance(t *testing.T) {
	rec := newHostRecorder()
	scratchRoot := filepath.Join(t.TempDir(), "srv/bunker-share")
	ensureProvisionedScratchRoot(t, rec, scratchRoot)
	instanceRoot := instanceRootFor(t)

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.Isolation.SharedScratchRoot = scratchRoot
	cfg.Agent.Isolation.PrivateTmpRoot = instanceRoot
	cfg.Agent.Isolation.SharedScratchPerAgentBytes = 64 << 20
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()
	m.hostRunner = rec.run

	m.provisionIsolation(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001)

	dir := filepath.Join(scratchRoot, "agent-a")
	wantMount := "mount -t tmpfs -o size=67108864,mode=0770,nosuid,nodev tmpfs " + dir
	if !rec.ran(wantMount) {
		t.Errorf("bounded scratch mount not issued\nwant: %s\ncalls: %v", wantMount, rec.calls)
	}
	if !rec.ran("usermod -aG " + hostsetup.DefaultScratchGroup + " bunker-agent-a") {
		t.Errorf("agent was not added to the scratch group: %v", rec.calls)
	}
	if !rec.ran("chmod 2770 " + dir) {
		t.Errorf("scratch dir is not setgid: %v", rec.calls)
	}
	if !rec.ranPrefix("chown 1001:") {
		t.Errorf("scratch dir ownership not set: %v", rec.calls)
	}
	instDir := filepath.Join(instanceRoot, "bunker-agent-a")
	if !rec.ran("chmod 0700 " + instDir) {
		t.Errorf("private /tmp instance dir not provisioned: %v", rec.calls)
	}
	if !rec.ranPrefix("chown 1001:1001 " + instDir) {
		t.Errorf("private /tmp instance dir ownership not set: %v", rec.calls)
	}
}

// TestProvisionIsolation_ScratchDisabledStillGetsPrivateTmp is the fail-safe
// direction: turning the exchange point off must not remove the private /tmp.
func TestProvisionIsolation_ScratchDisabledStillGetsPrivateTmp(t *testing.T) {
	rec := newHostRecorder()
	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.Isolation.SharedScratchEnabled = false
	cfg.Agent.Isolation.SharedScratchRoot = filepath.Join(t.TempDir(), "srv/bunker-share")
	cfg.Agent.Isolation.PrivateTmpRoot = instanceRootFor(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()
	m.hostRunner = rec.run

	m.provisionIsolation(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001)

	if rec.ranPrefix("mount -t tmpfs") {
		t.Errorf("scratch was mounted while disabled by config: %v", rec.calls)
	}
	if !rec.ran("chmod 0700 " + filepath.Join(cfg.Agent.Isolation.PrivateTmpRoot, "bunker-agent-a")) {
		t.Errorf("private /tmp instance missing while scratch is disabled: %v", rec.calls)
	}
	// The isolation group membership must be granted even when the optional
	// exchange directory is off: the sshd pam_exec precondition requires it, so
	// an agent without it has every session DENIED rather than sharing /tmp.
	if !rec.ran("usermod -aG " + hostsetup.DefaultAgentGroup + " bunker-agent-a") {
		t.Errorf("agent was not added to the isolation group while scratch is disabled: %v", rec.calls)
	}
	if !rec.ran("chown 1001:1001 " + filepath.Join(cfg.Agent.Isolation.PrivateTmpRoot, "bunker-agent-a")) {
		t.Errorf("private /tmp instance ownership missing while scratch is disabled: %v", rec.calls)
	}
}

// TestProvisionIsolation_MembershipIsMandatoryWithoutScratch pins the coupling
// the review demanded: the group every agent joins is the isolation identity,
// not a shared-scratch feature, so it is granted unconditionally — and a
// membership that cannot be granted is a hard error (fail closed) rather than
// a warning that leaves the agent on the host /tmp.
func TestProvisionIsolation_MembershipIsMandatoryWithoutScratch(t *testing.T) {
	for _, scratch := range []bool{true, false} {
		name := "scratch_disabled"
		if scratch {
			name = "scratch_enabled"
		}
		t.Run(name, func(t *testing.T) {
			rec := newHostRecorder()
			cfg := config.DefaultConfig()
			isolateRegistry(t, cfg)
			cfg.Agent.Isolation.SharedScratchEnabled = scratch
			cfg.Agent.Isolation.SharedScratchRoot = filepath.Join(t.TempDir(), "srv/bunker-share")
			cfg.Agent.Isolation.PrivateTmpRoot = instanceRootFor(t)
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
			defer m.Stop()
			m.hostRunner = rec.run

			if err := m.provisionIsolation(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001); err != nil {
				t.Fatalf("provisionIsolation() error = %v", err)
			}
			if !rec.ran("usermod -aG " + hostsetup.DefaultAgentGroup + " bunker-agent-a") {
				t.Errorf("isolation membership not granted (scratch=%v): %v", scratch, rec.calls)
			}
		})
	}
}

// TestProvisionIsolation_MembershipFailureIsFatal is the fail-closed direction:
// an agent that cannot be put into the isolation group would log in with the
// host's shared /tmp, so the spawn must not proceed.
func TestProvisionIsolation_MembershipFailureIsFatal(t *testing.T) {
	rec := newHostRecorder()
	rec.failUsermod = true
	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.Isolation.SharedScratchRoot = filepath.Join(t.TempDir(), "srv/bunker-share")
	cfg.Agent.Isolation.PrivateTmpRoot = instanceRootFor(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()
	m.hostRunner = rec.run

	err := m.provisionIsolation(context.Background(), "agent-a", "bunker-agent-a", 1001, 1001)
	if err == nil {
		t.Fatal("provisionIsolation() succeeded while the isolation group membership could not be granted")
	}
	if !strings.Contains(err.Error(), hostsetup.DefaultAgentGroup) {
		t.Errorf("error = %v, want it to name the isolation group", err)
	}
	if rec.ranPrefix("mount -t tmpfs") {
		t.Errorf("scratch was provisioned after the membership failed: %v", rec.calls)
	}
}

// TestSpawnFailsClosedWithoutIsolationMembership is the anti-phantom wiring
// proof: the fail-closed decision must be taken by Spawn itself, not only by
// the helper its unit test calls.
func TestSpawnFailsClosedWithoutIsolationMembership(t *testing.T) {
	rec := newHostRecorder()
	rec.failUsermod = true
	scratchRoot := filepath.Join(t.TempDir(), "srv/bunker-share")

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.Isolation.SharedScratchRoot = scratchRoot
	cfg.Agent.Isolation.PrivateTmpRoot = instanceRootFor(t)
	cfg.Agent.SSHDir = filepath.Join(t.TempDir(), "ssh")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()
	m.hostRunner = rec.run

	binDir := t.TempDir()
	stub := "#!/bin/sh\necho \"useradd: user '$2' already exists\" >&2\nexit 9\n"
	if err := os.WriteFile(filepath.Join(binDir, "useradd"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	restore := lookupAgentUser
	lookupAgentUser = func(username string) (*user.User, error) {
		return &user.User{Username: username, Uid: "1001", Gid: "1001", HomeDir: "/home/" + username}, nil
	}
	defer func() { lookupAgentUser = restore }()

	agentID := uniqueAgentID("gap075-closed")
	_, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
	if err == nil {
		t.Fatal("Spawn() succeeded although the agent could not join the isolation group")
	}
	if !strings.Contains(err.Error(), hostsetup.DefaultAgentGroup) {
		t.Errorf("Spawn() error = %v, want it to name the isolation group", err)
	}
	// The spawn was rolled back: no scratch, and never a bounded scratch that
	// the agent could reach.
	dir := filepath.Join(scratchRoot, agentID)
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("failed spawn left a scratch directory behind: %v", statErr)
	}
}

// TestDestroyRemovesIsolation proves the destroy path consults the isolation
// provisioners, without needing a real user (the scratch paths are probed
// before userdel).
func TestDestroyRemovesIsolation(t *testing.T) {
	rec := newHostRecorder()
	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	scratchRoot := filepath.Join(t.TempDir(), "srv/bunker-share")
	instanceRoot := instanceRootFor(t)
	cfg.Agent.Isolation.SharedScratchRoot = scratchRoot
	cfg.Agent.Isolation.PrivateTmpRoot = instanceRoot
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()
	m.hostRunner = rec.run

	// The scratch directory exists (as if the agent had been provisioned), so
	// destroy must remove it.
	dir := filepath.Join(scratchRoot, "ghost-agent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	rec.mounted[dir] = true
	rec.opts[dir] = "size=67108864,mode=0770,nosuid,nodev"

	if _, err := m.Destroy(context.Background(), "ghost-agent", true); err != nil {
		t.Fatalf("Destroy() error = %v", err)
	}

	if !rec.ran("umount -l " + dir) {
		t.Errorf("destroy did not unmount the agent's scratch: %v", rec.calls)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("destroy left the scratch directory behind: %v", err)
	}
	instDir := filepath.Join(instanceRoot, "bunker-ghost-agent")
	if _, err := os.Stat(instDir); !os.IsNotExist(err) {
		t.Errorf("destroy left the tmp instance directory behind: %v", err)
	}
}

// TestSpawnWiresIsolation is the anti-phantom check for the spawn path: it
// drives the REAL Spawn with a stub `useradd` and an injected user lookup, and
// asserts that the bounded scratch and the private-/tmp instance were actually
// provisioned before the spawn failed on the (unwritable) agent home. Without
// this, a refactor could silently drop the provisioning call from Spawn while
// every helper test stayed green.
func TestSpawnWiresIsolation(t *testing.T) {
	rec := newHostRecorder()
	scratchRoot := filepath.Join(t.TempDir(), "srv/bunker-share")
	// The exchange root is verified on EVERY spawn (DF-BUNKER-40), so the
	// fixture has to model a host that was provisioned: a root that is absent
	// or root:root is correctly refused rather than silently accepted.
	ensureProvisionedScratchRoot(t, rec, scratchRoot)
	instanceRoot := instanceRootFor(t)

	cfg := config.DefaultConfig()
	isolateRegistry(t, cfg)
	cfg.Agent.Isolation.SharedScratchRoot = scratchRoot
	cfg.Agent.Isolation.PrivateTmpRoot = instanceRoot
	cfg.Agent.Isolation.SharedScratchPerAgentBytes = 64 << 20
	cfg.Agent.SSHDir = filepath.Join(t.TempDir(), "ssh")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
	defer m.Stop()
	m.hostRunner = rec.run

	// A stub useradd that reports the user as already existing keeps the test
	// free of real user creation, and an injected lookup supplies the uid/gid
	// the provisioning step needs.
	binDir := t.TempDir()
	stub := "#!/bin/sh\necho \"useradd: user '$2' already exists\" >&2\nexit 9\n"
	if err := os.WriteFile(filepath.Join(binDir, "useradd"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	restore := lookupAgentUser
	lookupAgentUser = func(username string) (*user.User, error) {
		return &user.User{Username: username, Uid: "1001", Gid: "1001", HomeDir: "/home/" + username}, nil
	}
	defer func() { lookupAgentUser = restore }()

	// The spawn is expected to fail at the agent home (the test does not own
	// /home); the provisioning step runs BEFORE that and is what we assert on.
	agentID := uniqueAgentID("gap075")
	if _, err := m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"}); err == nil {
		t.Fatalf("Spawn unexpectedly succeeded for %s", agentID)
	}

	dir := filepath.Join(scratchRoot, agentID)
	if !rec.ranPrefix("mount -t tmpfs -o size=67108864,mode=0770,nosuid,nodev tmpfs " + dir) {
		t.Errorf("Spawn did not provision the bounded scratch for %s\ncalls: %v", agentID, rec.calls)
	}
	if !rec.ran("usermod -aG " + hostsetup.DefaultScratchGroup + " bunker-" + agentID) {
		t.Errorf("Spawn did not add the agent to the scratch group: %v", rec.calls)
	}
	if !rec.ran("chmod 0700 " + filepath.Join(instanceRoot, "bunker-"+agentID)) {
		t.Errorf("Spawn did not provision the private /tmp instance dir: %v", rec.calls)
	}
	// Rollback must not leave the provisioned exchange directory behind.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("failed spawn left the scratch directory behind: %v", err)
	}
}

// ensureProvisionedScratchRoot models a host whose exchange root is already in
// its documented shape: the directory exists setgid 2750 and the fake host
// reports root ownership with the agent group. On a sandboxed path the go
// process cannot chown to a foreign group, so the group column is what the fake
// `stat` answers while the MODE — read from the real directory by the gate — is
// made true on disk.
func ensureProvisionedScratchRoot(t *testing.T, rec *hostRecorder, scratchRoot string) {
	t.Helper()
	if err := os.MkdirAll(scratchRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scratchRoot, os.ModeSetgid|0o750); err != nil {
		t.Fatal(err)
	}
	// The gid the fake group resolves to (see hostRecorder.run's getent).
	rec.scratchRootOwner = "0:1001 " + hostsetup.OctalModeForTest(hostsetup.ScratchRootMode)
}

// TestSpawnVerifiesExchangeRoot is the DF-BUNKER-40 anti-phantom check at the
// level the defect was measured: the spawn path. Every spawn printed "shared
// scratch ready" while the exchange ROOT was root:root 0750, so no agent could
// even list /srv/bunker-share. A root that cannot be brought to its documented
// shape must therefore abort the scratch step — no per-agent directory, no
// bounded mount — and the refusal (plus the operator remedy) must reach the
// spawn log rather than reporting a ready scratch.
//
// The scratch step stays BEST-EFFORT at the spawn stage by design: the
// documented refusal is "this agent has no shared scratch" (its private /tmp is
// unaffected), and the spawn continues. That is why the assertion is on the log
// for the failure direction and on the issued commands for the success one.
func TestSpawnVerifiesExchangeRoot(t *testing.T) {
	const perAgent = 64 << 20

	tests := []struct {
		name        string
		rootOwner   string
		wantScratch bool
	}{
		{
			name:        "a correct exchange root lets the scratch be provisioned",
			rootOwner:   "0:1001 " + hostsetup.OctalModeForTest(hostsetup.ScratchRootMode),
			wantScratch: true,
		},
		{
			// The measured defect: root:root 0750. mkdir under it would
			// succeed and the agent still could not reach the tree.
			name:      "the measured root:root 0750 refuses the scratch",
			rootOwner: "0:0 750",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newHostRecorder()
			rec.scratchRootOwner = tc.rootOwner
			scratchRoot := filepath.Join(t.TempDir(), "srv/bunker-share")
			// The root's MODE is read from the real directory, so the fixture
			// has to exist with the mode its `stat` answer describes: setgid
			// 2750 for the healthy host, plain 0750 for the measured defect.
			rootSetgid := os.FileMode(0)
			if tc.wantScratch {
				rootSetgid = os.ModeSetgid
			}
			if err := os.MkdirAll(scratchRoot, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(scratchRoot, rootSetgid|0o750); err != nil {
				t.Fatal(err)
			}

			var logBuf bytes.Buffer
			cfg := config.DefaultConfig()
			isolateRegistry(t, cfg)
			cfg.Agent.Isolation.SharedScratchRoot = scratchRoot
			cfg.Agent.Isolation.PrivateTmpRoot = instanceRootFor(t)
			cfg.Agent.Isolation.SharedScratchPerAgentBytes = perAgent
			cfg.Agent.SSHDir = filepath.Join(t.TempDir(), "ssh")
			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			m := NewAgentManager(cfg, logger, resource.NewTracker(cfg.Agent.MaxAgents, logger), nil, nil)
			defer m.Stop()
			m.hostRunner = rec.run

			binDir := t.TempDir()
			stub := "#!/bin/sh\necho \"useradd: user '$2' already exists\" >&2\nexit 9\n"
			if err := os.WriteFile(filepath.Join(binDir, "useradd"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			restore := lookupAgentUser
			lookupAgentUser = func(username string) (*user.User, error) {
				return &user.User{Username: username, Uid: "1001", Gid: "1001", HomeDir: "/home/" + username}, nil
			}
			defer func() { lookupAgentUser = restore }()

			// The spawn fails later, at the agent home (this test does not own
			// /home); the scratch step runs BEFORE that and is what matters.
			agentID := uniqueAgentID("dfbunker40")
			_, _ = m.Spawn(context.Background(), &v1.SpawnAgentRequest{AgentId: agentID, Ttl: "1h"})
			dir := filepath.Join(scratchRoot, agentID)

			if tc.wantScratch {
				if !rec.ran("mount -t tmpfs -o size=67108864,mode=0770,nosuid,nodev tmpfs " + dir) {
					t.Errorf("Spawn did not provision the bounded scratch under a correct root\ncalls: %v", rec.calls)
				}
				if !strings.Contains(logBuf.String(), "shared scratch ready") {
					t.Errorf("Spawn did not report the scratch as ready on a correct host:\n%s", logBuf.String())
				}
				return
			}

			if rec.ranPrefix("mount -t tmpfs") {
				t.Errorf("a bounded scratch was mounted under an unusable exchange root: %v", rec.calls)
			}
			if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
				t.Errorf("spawn with an unusable exchange root left %s behind: %v", dir, statErr)
			}
			// The refusal must be LOUD and actionable: this is the log line an
			// operator sees on the host where every spawn used to claim the
			// scratch was fine.
			logged := logBuf.String()
			if strings.Contains(logged, "shared scratch ready") {
				t.Errorf("an unusable exchange root was reported as a ready scratch:\n%s", logged)
			}
			if !strings.Contains(logged, "host-provision --apply") {
				t.Errorf("the refusal did not name the operator remedy:\n%s", logged)
			}
		})
	}
}

// instanceRootFor returns a private-tmp instance root inside a temp dir. The
// provisioner restricts that directory to mode 0000 (pam_namespace's
// requirement), which even its owner cannot traverse, so the directory is
// relaxed again during test cleanup.
func instanceRootFor(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "var/lib/bunker/agent-tmp")
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return dir
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func countArg(argv []string, want string) int {
	n := 0
	for _, a := range argv {
		if a == want {
			n++
		}
	}
	return n
}

// TestResetStaleDockerdUnit is a regression guard for the step that clears a
// leftover unit before systemd-run creates it: a loaded/failed transient unit
// makes `systemd-run --unit=` fail with "already loaded", so stop/disable/
// reset-failed plus a direct kill of any surviving dockerd must be issued
// before every spawn. The commands are stubs on PATH, so the sequence is
// asserted without systemd and without touching any real service.
func TestResetStaleDockerdUnit(t *testing.T) {
	binDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "calls")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$(basename \"$0\") $*\" >> " + record + "\n" +
		// pgrep must report "no matching process" (exit 1) so stopDockerdDirect
		// ends immediately instead of waiting for a timeout.
		"case \"$(basename \"$0\")\" in pgrep) exit 1;; esac\nexit 0\n"
	for _, name := range []string{"systemctl", "pgrep"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	resetStaleDockerdUnit(context.Background(), "bunker-docker-agent-a", "bunker-agent-a", logger)

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no commands were issued: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"systemctl stop bunker-docker-agent-a\n",
		"systemctl disable bunker-docker-agent-a\n",
		"systemctl reset-failed bunker-docker-agent-a\n",
		"pgrep -u bunker-agent-a -f dockerd\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stale-unit reset did not issue %q\nissued:\n%s", want, got)
		}
	}
}
