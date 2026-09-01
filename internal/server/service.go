// Package server — bunkerd RPC service implementations.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/deployBunker/bunker/internal/tailscale"
	"github.com/deployBunker/bunker/internal/tunnel"
	"github.com/deployBunker/bunker/internal/version"
)

// serverStartTime records when the bunkerd process started. ServerInfo
// reports uptime as time since this instant. It is a variable so tests can
// inject a known start time.
var serverStartTime = time.Now()

// cpuSampler is the subset of *resource.CPUSampler used by bunkerdService.
// It exists so service-layer tests can stub CPU percent without reading the
// real host cgroup files (mirroring the agentManager pattern above).
type cpuSampler interface {
	Percent() float64
}

// bunkerdService implements bunkerv1connect.BunkerdHandler.
type bunkerdService struct {
	cfg          *config.Config
	logger       *slog.Logger
	agentMgr     agentManager
	tracker      *resource.Tracker
	tunnelMgr    *tunnel.TunnelManager
	tailscaleMgr *tailscale.TailscaleManager
	keyMgr       *apikey.Manager
	jwtAuth      *auth.JWTAuth
	cpuSampler   cpuSampler
	// auditLog is the daemon's audit trail writer (nil when audit logging
	// is disabled). QueryAudit reads from it; the audit interceptor writes
	// to it. It is only used for read access here — the interceptor owns
	// the write path.
	auditLog *audit.AuditLog
}

// agentManager is the subset of *agent.AgentManager used by bunkerdService.
// It exists primarily to make service-layer tests not require root.
type agentManager interface {
	Spawn(ctx context.Context, req *v1.SpawnAgentRequest) (*v1.SpawnAgentResponse, error)
	Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error)
	RunAgent(ctx context.Context, req *v1.RunAgentRequest) (*v1.RunAgentResponse, error)
	Stop()
}

// ServerInfo returns information about the bunkerd server.
func (s *bunkerdService) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	hostname, _ := os.Hostname()
	resp := &v1.ServerInfoResponse{
		Hostname:      hostname,
		Version:       version.Version,
		UptimeSeconds: uint64(time.Since(serverStartTime).Seconds()),
		AgentCount:    s.tracker.Count(),
		MaxAgents:     s.tracker.MaxAgents(),
	}
	return connect.NewResponse(resp), nil
}

// ServerMetrics returns resource usage metrics for the server.
func (s *bunkerdService) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	records := s.tracker.List()
	summaries := make([]*v1.AgentSummary, 0, len(records))
	for _, rec := range records {
		summaries = append(summaries, rec.ToAgentSummary())
	}

	resp := &v1.ServerMetricsResponse{
		Agents: summaries,
	}

	// CPU percent comes from delta sampling across calls; the first call on a
	// fresh sampler is a baseline and reports 0.
	if s.cpuSampler != nil {
		resp.CpuUsagePercent = s.cpuSampler.Percent()
	}

	// Memory from cgroup v2, falling back to /proc/meminfo when the cgroup
	// memory files are absent (see resource.ReadCgroupMetrics).
	if metrics, err := resource.ReadCgroupMetrics(); err == nil {
		resp.MemoryUsedBytes = metrics.MemoryUsedBytes
		resp.MemoryTotalBytes = metrics.MemoryLimitBytes
	}

	// Try to read filesystem disk stats
	if used, total, err := readDiskStats(); err == nil {
		resp.DiskUsedBytes = used
		resp.DiskTotalBytes = total
	}

	// Count docker sockets (proxy for running docker daemons)
	resp.DockerContainersTotal = countDockerSockets()

	return connect.NewResponse(resp), nil
}

// SpawnAgent creates a new isolated agent environment.
func (s *bunkerdService) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	// Check disk usage and warn if above 90% (spawns still proceed).
	if used, total, err := readDiskStats(); err == nil && total > 0 {
		pct := float64(used) / float64(total) * 100
		if pct > 90 {
			s.logger.Warn("disk usage above 90%, agent spawn may be affected",
				"disk_used_pct", fmt.Sprintf("%.1f%%", pct),
				"disk_used_bytes", used,
				"disk_total_bytes", total,
			)
		}
	}

	// Validate TTL format up front so invalid values surface as
	// CodeInvalidArgument (specs/api.md: "CodeInvalidArgument: Bad limits or
	// TTL format") instead of being silently ignored. The parsed value is
	// reused below for the agent-scoped API key TTL so both agree for day
	// units. Empty TTL is allowed and falls back to the server default.
	var reqTTL time.Duration
	if req.Msg.GetTtl() != "" {
		parsed, err := agent.ParseAgentTTL(req.Msg.GetTtl())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid ttl %q: %w", req.Msg.GetTtl(), err))
		}
		reqTTL = parsed
	}

	resp, err := s.agentMgr.Spawn(ctx, req.Msg)
	if err != nil {
		s.logger.Error("spawn agent failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Generate an agent-scoped opaque API sub-key when JWT auth is enabled.
	// The key is stored in the apikey manager and can be used by the agent
	// (or its owner) to call the Agent service scoped to this agent_id.
	if s.cfg.Auth.Enabled && s.cfg.Auth.JWTSecret != "" && s.keyMgr != nil {
		ttl := reqTTL
		if ttl <= 0 {
			ttl = s.cfg.Agent.DefaultTTL
			if ttl <= 0 {
				ttl = 6 * time.Hour
			}
		}
		if apiToken, _, err := s.keyMgr.Generate(resp.AgentId, ttl); err == nil {
			resp.ApiKey = apiToken
		} else {
			s.logger.Warn("failed to generate agent api key", "agent_id", resp.AgentId, "error", err)
		}
	}

	return connect.NewResponse(resp), nil
}

// DestroyAgent tears down an agent environment.
func (s *bunkerdService) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	resp, err := s.agentMgr.Destroy(ctx, req.Msg.AgentId, req.Msg.Force)
	if err != nil {
		s.logger.Error("destroy agent failed", "agent_id", req.Msg.AgentId, "error", err)
		// Map "not_found" to NotFound, other errors to Internal
		if resp != nil && resp.Status == "not_found" {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

// ListAgents returns all agents.
func (s *bunkerdService) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	records := s.tracker.List()
	summaries := make([]*v1.AgentSummary, 0, len(records))
	for _, rec := range records {
		// Compute per-agent disk usage (best-effort, async-safe).
		rec.DiskUsedBytes = agentDiskUsage(rec.AgentID)
		summaries = append(summaries, rec.ToAgentSummary())
	}
	return connect.NewResponse(&v1.ListAgentsResponse{
		Agents:     summaries,
		TotalCount: uint32(len(summaries)),
	}), nil
}

// GetAgent returns a single agent by ID.
func (s *bunkerdService) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
	}
	return connect.NewResponse(&v1.GetAgentResponse{
		Agent: rec.ToAgentSummary(),
	}), nil
}

// AgentMetrics returns resource usage for a specific agent.
func (s *bunkerdService) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
	}

	resp := &v1.AgentMetricsResponse{
		AgentId: rec.AgentID,
		Status:  rec.Status,
	}
	if rec.Limits != nil {
		resp.MemoryLimitBytes = rec.Limits.MemoryMaxBytes
		resp.DiskLimitBytes = rec.Limits.DiskMaxBytes
	}

	// Try to read the agent's own cgroup metrics (best-effort). The agent's
	// systemd user unit lives under user.slice/user-<uid>.slice/..., so resolve
	// the agent user's UID first. When the user or cgroup is absent (stopped or
	// destroyed agent, deleted user), the read degrades to the host-level read
	// and HostLevelFallback is set — never an error, never a panic.
	uid := 0
	userResolved := true
	if u, err := user.Lookup("bunker-" + rec.AgentID); err == nil {
		if parsedUID, err := strconv.Atoi(u.Uid); err == nil {
			uid = parsedUID
		} else {
			userResolved = false
			s.logger.Warn("agent user UID parse failed; metrics will fall back to host level",
				"agent_id", rec.AgentID, "error", err)
		}
	} else {
		userResolved = false
		s.logger.Warn("agent user lookup failed; metrics will fall back to host level",
			"agent_id", rec.AgentID, "error", err)
	}

	if userResolved {
		if metrics, err := resource.ReadAgentCgroupMetrics(uid, rec.AgentID); err == nil {
			resp.CpuUsagePercent = metrics.CPUUsagePercent
			resp.MemoryUsedBytes = metrics.MemoryUsedBytes
			resp.HostLevelFallback = metrics.HostLevelFallback
		}
	} else {
		// Unknown agent user: never attempt a user-0.slice read as if it were
		// a valid agent cgroup. Read host-level metrics directly and flag the
		// fallback so callers can warn that these are HOST values.
		if metrics, err := resource.ReadCgroupMetrics(); err == nil {
			resp.CpuUsagePercent = metrics.CPUUsagePercent
			resp.MemoryUsedBytes = metrics.MemoryUsedBytes
			resp.HostLevelFallback = true
		}
	}

	// Read per-agent disk usage (best-effort)
	resp.DiskUsedBytes = agentDiskUsage(rec.AgentID)

	return connect.NewResponse(resp), nil
}

// ExecAgent executes a command in the agent's environment via SSH.
func (s *bunkerdService) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	agentID := req.Msg.AgentId
	if agentID == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_id is required"))
	}

	// Stamp the target agent id into the streaming audit sink so the audit
	// interceptor records agent_id=<aid> for this exec even for master tokens
	// (DOGFOOD-012). Must happen before any early return so error paths are
	// covered too.
	audit.StampStreamAgentID(ctx, agentID)

	// Look up agent record
	rec := s.tracker.Get(agentID)
	if rec == nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", agentID))
	}

	// Build command to execute
	// The agent's dockerd listens on a per-agent Unix socket.  We need
	// DOCKER_HOST in the remote environment so that `docker` CLI commands
	// (and anything else that talks to Docker) reach the right socket.
	//
	// Two mechanisms:
	//   1. authorized_keys `environment=` prefix (set at spawn time) — works
	//      when sshd has PermitUserEnvironment=yes.
	//   2. `ssh -o SetEnv=DOCKER_HOST=...` — explicit client-side env push
	//      that works regardless of sshd config.  This is the fallback and
	//      the primary mechanism we rely on.
	//
	// We also set the variable in ~/.profile for interactive shells.
	userHome := "/home/bunker-" + agentID

	// Determine execution mode: raw (no shell) or shell-wrapped. Scripts are
	// uploaded and executed by the shell wrapper, so they share the same path.
	var cmd *exec.Cmd
	if req.Msg.GetRaw() {
		cmd = buildExecSSHRawCommand(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.Command, req.Msg.Args)
	} else if req.Msg.GetScriptContent() != "" {
		cmd = buildExecSSHScriptCommand(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.GetScriptContent())
	} else {
		cmd = buildExecSSHCommand(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.Command, req.Msg.Args)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("stdout pipe: %w", err))
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("stderr pipe: %w", err))
	}

	if err := cmd.Start(); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("start ssh: %w", err))
	}

	// Stream stdout and stderr concurrently
	var wg sync.WaitGroup
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(stdoutDone)
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				if err := stream.Send(&v1.ExecAgentResponse{
					Output: &v1.ExecAgentResponse_Stdout{Stdout: buf[:n]},
				}); err != nil {
					s.logger.Warn("send stdout", "error", err)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer close(stderrDone)
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				if err := stream.Send(&v1.ExecAgentResponse{
					Output: &v1.ExecAgentResponse_Stderr{Stderr: buf[:n]},
				}); err != nil {
					s.logger.Warn("send stderr", "error", err)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for command completion
	exitCode := int32(0)
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("ssh wait: %w", err))
		}
	}

	// Wait for streamers to finish
	wg.Wait()

	// Send final exit code
	if err := stream.Send(&v1.ExecAgentResponse{
		ExitCode: exitCode,
	}); err != nil {
		s.logger.Warn("send exit code", "error", err)
	}

	return nil
}

// RunAgent starts a command in the agent environment as a persistent systemd
// transient unit. The unit survives the RPC session ending. Non-detached
// (synchronous) runs are handled by the CLI via ExecAgent streaming.
func (s *bunkerdService) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	if req.Msg.GetAgentId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_id is required"))
	}
	if req.Msg.GetCommand() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("command is required"))
	}
	if !req.Msg.GetDetach() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("non-detached runs are not supported by RunAgent; use ExecAgent"))
	}
	if rec := s.tracker.Get(req.Msg.GetAgentId()); rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.GetAgentId()))
	}
	resp, err := s.agentMgr.RunAgent(ctx, req.Msg)
	if err != nil {
		s.logger.Error("run agent failed", "agent_id", req.Msg.GetAgentId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

// HeartbeatAgent acknowledges an agent heartbeat.
func (s *bunkerdService) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
	}
	// Extend TTL on heartbeat unless the agent has a zero TTL. Never SHRINK:
	// heartbeating a long-TTL agent (e.g. 720h) must not reset it to the
	// default 6h — a shorter expiry would silently destroy the agent on TTL
	// expiry (userdel + data loss) between renewal runs.
	ttl := 6 * time.Hour
	if s.cfg.Agent.DefaultTTL > 0 {
		ttl = s.cfg.Agent.DefaultTTL
	}
	candidate := time.Now().Add(ttl)
	if candidate.After(rec.ExpiresAt) {
		rec.ExpiresAt = candidate
	}
	return connect.NewResponse(&v1.HeartbeatAgentResponse{
		AgentId:      req.Msg.AgentId,
		ExpiresAt:    rec.ExpiresAt.Format(time.RFC3339),
		Acknowledged: true,
	}), nil
}

// QueryAudit returns audit trail records matching the request filters,
// oldest first. The trail is read from the daemon's audit log (live file +
// rotated backups) exactly as `bunker audit list/export --path` reads it
// locally — this is the remote query surface for operators who cannot (or
// must not) ssh in and grep raw files.
//
// The handler is read-only: it never writes records itself. (The record for
// this very query is appended by the audit interceptor, like every other
// authenticated RPC.)
func (s *bunkerdService) QueryAudit(ctx context.Context, req *connect.Request[v1.QueryAuditRequest]) (*connect.Response[v1.QueryAuditResponse], error) {
	if s.auditLog == nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("audit logging is disabled on this server"))
	}

	f := audit.Filter{
		AgentID: req.Msg.GetAgentId(),
		Method:  req.Msg.GetMethod(),
		Limit:   int(req.Msg.GetLimit()),
	}
	// Invalid timestamps are surfaced as CodeInvalidArgument so a typo in
	// --since/--until cannot silently widen the query to "everything".
	if since := req.Msg.GetSince(); since != "" {
		ts, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid since %q: %w", since, err))
		}
		f.Since = &ts
	}
	if until := req.Msg.GetUntil(); until != "" {
		ts, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid until %q: %w", until, err))
		}
		f.Until = &ts
	}

	records, err := audit.Query(s.auditLog.Path(), f)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read audit trail: %w", err))
	}

	out := make([]*v1.AuditRecord, 0, len(records))
	for _, r := range records {
		out = append(out, &v1.AuditRecord{
			Ts:         r.TS,
			Caller:     r.Caller,
			Method:     r.Method,
			RemoteAddr: r.RemoteAddr,
			AgentId:    r.AgentID,
			DurationMs: r.DurationMS,
			Outcome:    r.Outcome,
			Summary:    r.Summary,
			Hash:       r.Hash,
			PrevHash:   r.PrevHash,
		})
	}
	return connect.NewResponse(&v1.QueryAuditResponse{Records: out}), nil
}

// agentExecBasePath is the deterministic PATH base used for agent execs (the
// agent's own bin dir is prepended by each builder). It deliberately does NOT
// inherit the daemon's ambient $PATH: a polluted or pathologically long
// daemon PATH (e.g. scheduler shells accumulate hundreds of duplicate entries
// into ~16KB values) would be embedded verbatim into every agent command, and
// uutils env(1) fails to execvp such values (exit 126 "unknown error:
// execvp failed" / 127 "No such file or directory"), breaking agent execs on
// hosts where /usr/bin/env is uutils coreutils.
const agentExecBasePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// buildAgentExecCommand constructs the shell command that runs inside the agent
// via SSH.  It prefixes the user command with env(1) so PATH, DOCKER_HOST, and
// TMPDIR are set regardless of sshd PermitUserEnvironment/AcceptEnv settings,
// and sources /run/bunker/<id>/env so that `bunker env set` injections are
// visible to the command.
func buildAgentExecCommand(agentID, userHome, command string, args []string) string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	tmpDir := filepath.Join("/run", "bunker", agentID, "tmp")
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	envFile := fmt.Sprintf("/run/bunker/%s/env", agentID)
	remoteCmd := command
	if len(args) > 0 {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = shellQuoteSingle(a)
		}
		remoteCmd += " " + strings.Join(quoted, " ")
	}
	// set -a (allexport) around the source so injected vars are exported to
	// the child `sh -c` below — plain KEY=VALUE lines would otherwise only be
	// shell variables, invisible to the wrapped command. The [ -f ] guard
	// keeps a fresh agent (no env file yet) from making dash exit 2 on the
	// failed dot-source.
	return fmt.Sprintf("set -a; [ -f %s ] && . %s 2>/dev/null; set +a; env PATH=%s DOCKER_HOST=unix://%s TMPDIR=%s sh -c %s",
		envFile, envFile, agentPath, dockerSockPath, tmpDir, shellQuoteSingle(remoteCmd))
}

// shellQuoteSingle returns s wrapped in single quotes, with embedded single
// quotes escaped for POSIX sh.  Example: hello'world -> 'hello'\\”world'.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// buildAgentRawExecCommand constructs the remote argv for raw mode. The command
// is executed directly by the SSH server without an intermediate shell, so args
// are passed as-is and shell injection/metacharacters are not interpreted.
//
// Raw mode intentionally does NOT source /run/bunker/<id>/env — the env file is
// sourced by the *shell* at the top of buildAgentExecCommand / buildAgentScriptCommand,
// and `bunker env set` is meant for shell-aware commands. Use plain `bunker exec`
// (without --raw) or `bunker exec --script` to see env vars set via `bunker env set`.
func buildAgentRawExecCommand(agentID, userHome, command string, args []string) []string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	tmpDir := filepath.Join("/run", "bunker", agentID, "tmp")
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	// sshd's ForceCommand or default shell may still receive a string, but
	// passing a command with args and using ssh's internal exec channel (when
	// the remote shell is not forced) will execve directly. We keep a tiny
	// wrapper here: env(1) so we can set DOCKER_HOST and TMPDIR before the real binary.
	return append([]string{
		"env",
		"PATH=" + agentPath,
		"DOCKER_HOST=unix://" + dockerSockPath,
		"TMPDIR=" + tmpDir,
		command,
	}, args...)
}

// buildAgentScriptCommand writes scriptContent to a remote file and returns the
// shell command that executes it. The file is written via ssh heredoc.
func buildAgentScriptCommand(agentID, userHome, scriptContent string) string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	tmpDir := filepath.Join("/run", "bunker", agentID, "tmp")
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	scriptPath := filepath.Join(userHome, ".bunker", "exec-script.sh")
	envFile := fmt.Sprintf("/run/bunker/%s/env", agentID)
	// Use POSIX heredoc to create + chmod + execute the script in one SSH call.
	// We quote the EOF delimiter to prevent expansion of the script body.
	escaped := strings.ReplaceAll(scriptContent, "'", "'\\''")
	return fmt.Sprintf(
		"mkdir -p %q && cat > %q <<'EOFSCRIPT'\n%s\nEOFSCRIPT\nchmod +x %q && set -a; [ -f %s ] && . %s 2>/dev/null; set +a; env PATH=%s DOCKER_HOST=unix://%s TMPDIR=%s %q",
		filepath.Dir(scriptPath), scriptPath, escaped, scriptPath, envFile, envFile, agentPath, dockerSockPath, tmpDir, scriptPath,
	)
}

// buildExecSSHCommand returns an exec.Cmd that runs buildAgentExecCommand
// inside the agent via ssh.  The remote script is passed as a single quoted
// "sh -c '...'" argument to OpenSSH so that multi-token commands such as
// "docker version" are not misparsed by the inner shell.
func buildExecSSHCommand(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string) *exec.Cmd {
	wrappedCmd := buildAgentExecCommand(agentID, userHome, command, args)
	sshRemoteCmd := fmt.Sprintf("sh -c %s", shellQuoteSingle(wrappedCmd))
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, sshRemoteCmd)
}

// buildExecSSHRawCommand returns an exec.Cmd that runs command+args directly
// without a shell wrapper. Each arg is passed as a separate ssh argument; sshd
// will attempt to exec the requested program directly.
func buildExecSSHRawCommand(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string) *exec.Cmd {
	remoteArgv := buildAgentRawExecCommand(agentID, userHome, command, args)
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, remoteArgv...)
}

// buildExecSSHScriptCommand returns an exec.Cmd that uploads scriptContent via
// heredoc and executes it on the agent.
func buildExecSSHScriptCommand(ctx context.Context, agentID, sshKeyPath, userHome, scriptContent string) *exec.Cmd {
	wrappedCmd := buildAgentScriptCommand(agentID, userHome, scriptContent)
	sshRemoteCmd := fmt.Sprintf("sh -c %s", shellQuoteSingle(wrappedCmd))
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, sshRemoteCmd)
}

// buildSSHBaseCommand builds the common ssh command with the given remote args.
func buildSSHBaseCommand(ctx context.Context, agentID, sshKeyPath string, remoteArgs ...string) *exec.Cmd {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-i", sshKeyPath,
		fmt.Sprintf("bunker-%s@localhost", agentID),
	}
	args = append(args, remoteArgs...)
	return exec.CommandContext(ctx, "ssh", args...)
}

// agentService implements bunkerv1connect.AgentHandler.
type agentService struct {
	logger  *slog.Logger
	tracker *resource.Tracker
}

// GetInfo returns info about the authenticated agent.
func (s *agentService) GetInfo(ctx context.Context, req *connect.Request[v1.GetInfoRequest]) (*connect.Response[v1.GetInfoResponse], error) {
	// Extract agent_id from the auth context (JWT claims or scoped sub-key).
	agentID := ""
	if claims, ok := auth.ClaimsFromContext(ctx); ok && claims.AgentID != "" {
		agentID = claims.AgentID
	}

	resp := &v1.GetInfoResponse{
		Status: "running",
	}
	if agentID != "" {
		resp.AgentId = agentID
		// If we have a tracker record, populate more fields
		if rec := s.tracker.Get(agentID); rec != nil {
			status := rec.Status
			if status == "" {
				status = "running"
			}
			resp.Status = status
			resp.PublicUrl = rec.PublicURL
			resp.ExpiresAt = rec.ExpiresAt.Format(time.RFC3339)
			if rec.Limits != nil {
				resp.Limits = rec.Limits
			}
		}
	}
	return connect.NewResponse(resp), nil
}

// Metrics returns resource usage for the authenticated agent.
func (s *agentService) Metrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	resp := &v1.AgentMetricsResponse{
		AgentId: req.Msg.AgentId,
		Status:  "running",
	}
	if metrics, err := resource.ReadCgroupMetrics(); err == nil {
		resp.CpuUsagePercent = metrics.CPUUsagePercent
		resp.MemoryUsedBytes = metrics.MemoryUsedBytes
	}
	return connect.NewResponse(resp), nil
}

// Heartbeat sends a heartbeat from the authenticated agent.
func (s *agentService) Heartbeat(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
	}
	// Extend TTL on heartbeat; never SHRINK a longer expiry (same rule as
	// bunkerdService.HeartbeatAgent — a heartbeat must not reset a 720h
	// agent back to the 6h default).
	candidate := time.Now().Add(6 * time.Hour)
	if candidate.After(rec.ExpiresAt) {
		rec.ExpiresAt = candidate
	}
	return connect.NewResponse(&v1.HeartbeatAgentResponse{
		AgentId:      req.Msg.AgentId,
		ExpiresAt:    rec.ExpiresAt.Format(time.RFC3339),
		Acknowledged: true,
	}), nil
}

// readDiskStats returns used and total bytes for the root filesystem.
func readDiskStats() (used, total uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return 0, 0, err
	}
	total = stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	used = total - free
	return used, total, nil
}

// agentDiskUsage walks the agent's home directory and returns the total
// bytes consumed. Returns 0 on error (best-effort).
func agentDiskUsage(agentID string) uint64 {
	homeDir := fmt.Sprintf("/home/bunker-%s", agentID)
	var total uint64
	filepath.WalkDir(homeDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: skip inaccessible paths
		}
		if !d.IsDir() {
			if info, infoErr := d.Info(); infoErr == nil {
				total += uint64(info.Size())
			}
		}
		return nil
	})
	return total
}

// countDockerSockets counts the number of docker socket files in /run/bunker/*/docker.sock.
// This is a proxy for the number of running dockerd instances.
func countDockerSockets() uint32 {
	entries, err := os.ReadDir("/run/bunker")
	if err != nil {
		return 0
	}
	var count uint32
	for _, entry := range entries {
		if entry.IsDir() {
			sockPath := filepath.Join("/run/bunker", entry.Name(), "docker.sock")
			if info, statErr := os.Stat(sockPath); statErr == nil && info.Mode()&os.ModeSocket != 0 {
				count++
			}
		}
	}
	return count
}
