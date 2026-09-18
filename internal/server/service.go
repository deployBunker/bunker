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
	"github.com/deployBunker/bunker/internal/hostsetup"
	"github.com/deployBunker/bunker/internal/imagespec"
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
	heartbeats   heartbeatManager
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
	// tmpIsolationOnce guards the one-time host probe behind ServerInfo's
	// /tmp isolation report (DF-BUNKER-9). TmpNamespaceStatus stats the
	// host's PAM/sshd configuration and runs getent, so it must not run on
	// every ServerInfo call; the observed result is cached below. The
	// cached values are written inside the Once and read by ServerInfo.
	tmpIsolationOnce   sync.Once
	tmpIsolationLevel  string
	tmpIsolationDetail string
}

// heartbeatManager is the narrow slice of the agent manager the heartbeat
// RPCs use. Heartbeats are routed through the manager (GAP-070) so the
// durable registry records every lifecycle change; a nil implementation
// (tests, unwired services) falls back to the pre-existing direct tracker
// mutation.
type heartbeatManager interface {
	Heartbeat(agentID string, ttl time.Duration) (*resource.AgentRecord, error)
}

// agentManager is the subset of *agent.AgentManager used by bunkerdService.
// It exists primarily to make service-layer tests not require root.
type agentManager interface {
	Spawn(ctx context.Context, req *v1.SpawnAgentRequest) (*v1.SpawnAgentResponse, error)
	Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error)
	// GAP-071 lifecycle control: pause/resume/restart without destroying.
	StopAgent(ctx context.Context, agentID string) (*v1.StopAgentResponse, error)
	StartAgent(ctx context.Context, agentID string) (*v1.StartAgentResponse, error)
	RestartAgent(ctx context.Context, agentID string) (*v1.RestartAgentResponse, error)
	RunAgent(ctx context.Context, req *v1.RunAgentRequest) (*v1.RunAgentResponse, error)
	// ResidueInventory probes the host's residue planes (orphan users, homes,
	// keys, linger entries) for the operator status surface (DF-BUNKER-21).
	// Read-only, never fails: an unreadable plane is reported in Status/Detail.
	ResidueInventory() agent.ResidueInventory
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
	// DF-BUNKER-9: advertise which /tmp policy this daemon actually enforces
	// so a README-vs-reality downgrade (e.g. a tagged release without the
	// GAP-075 isolation work) is visible instead of silent. The probe runs
	// once per process and never fails the RPC.
	resp.TmpIsolation, resp.TmpIsolationDetail = s.tmpIsolation()
	// DF-BUNKER-21: report the residue the daemon itself can see on the host,
	// so a leaked agent (an orphan user/home/key/linger entry with no agent
	// behind it) is visible to an operator instead of only in a rollback
	// breadcrumb. Read-only; a daemon without a manager reports nothing rather
	// than inventing zeroes.
	resp.Residue = s.residueInventory()
	return connect.NewResponse(resp), nil
}

// residueInventory maps the agent manager's host probe into the ServerInfo
// message. A service without a manager (tests, unwired services) reports NO
// residue rather than a fabricated zero inventory: an absent message is
// distinguishable from a clean host by construction (see the proto field).
func (s *bunkerdService) residueInventory() *v1.ResidueInventory {
	if s.agentMgr == nil {
		return nil
	}
	inv := s.agentMgr.ResidueInventory()
	return &v1.ResidueInventory{
		OrphanUsers:        uint32(maxInt(inv.OrphanUsers, 0)),
		OrphanHomes:        uint32(maxInt(inv.OrphanHomes, 0)),
		OrphanKeys:         uint32(maxInt(inv.OrphanKeys, 0)),
		StaleLingerEntries: uint32(maxInt(inv.StaleLinger, 0)),
		RegisteredAgents:   uint32(maxInt(inv.Registered, 0)),
		Status:             inv.Status,
		Detail:             inv.Detail,
	}
}

// maxInt clamps a probe count at zero before the uint32 conversion: the proto
// counts are unsigned, and a negative value must never wrap into ~4 billion
// "residue" items.
func maxInt(v, min int) int {
	if v < min {
		return min
	}
	return v
}

// The /tmp isolation levels ServerInfo reports in ServerInfoResponse
// tmp_isolation. An EMPTY field means the daemon predates capability
// reporting (any tagged v0.1.x release).
const (
	tmpIsolationPrivate    = "private"
	tmpIsolationHostShared = "host-shared"
	tmpIsolationUnknown    = "unknown"
)

// tmpIsolationFromState maps an observed hostsetup.TmpNamespaceState into the
// (level, detail) pair ServerInfo reports. It is pure — no I/O, no root
// required — so the four branches are pinned by a table-driven unit test that
// passes as the non-root user running the tests:
//
//   - probe error                            -> ("unknown", <error>)
//   - not OwnershipVerifiable (daemon !root) -> ("unknown", <reason>)
//   - st.Active                              -> ("private", <confirmation>)
//   - otherwise                              -> ("host-shared", <first failing
//     provisioning reason, or a fallback>)
func tmpIsolationFromState(st hostsetup.TmpNamespaceState, err error) (level, detail string) {
	if err != nil {
		return tmpIsolationUnknown, err.Error()
	}
	if !st.OwnershipVerifiable {
		return tmpIsolationUnknown, "daemon is not running as root — /tmp isolation state cannot be verified"
	}
	if st.Active {
		return tmpIsolationPrivate, "per-session pam_namespace instance is provisioned and enforced"
	}
	if reason := tmpIsolationInactiveReason(st); reason != "" {
		return tmpIsolationHostShared, reason
	}
	return tmpIsolationHostShared, "pam_namespace private-/tmp provisioning is not active on this host"
}

// tmpIsolationInactiveReason names the FIRST property of the observed state
// that keeps pam_namespace private-/tmp provisioning inactive, mirroring the
// conjunction TmpNamespaceState.evaluateActive checks. Empty only when every
// observed property passes (the caller then falls back to a generic reason).
func tmpIsolationInactiveReason(st hostsetup.TmpNamespaceState) string {
	switch {
	case !st.ModulePresent:
		return "pam_namespace.so is not installed on this host"
	case !st.GuardModulePresent:
		return "pam_succeed_if.so is not installed on this host"
	case !st.GuardModuleSupportsPattern:
		return "pam_succeed_if.so does not support the agent-name pattern test (needs Linux-PAM >= 1.6)"
	case !st.ExecModulePresent:
		return "pam_exec.so is not installed on this host"
	case !st.ConfPresent:
		return "the pam_namespace drop-in configuration is missing"
	case !st.ConfRuleOK && st.ConfRuleDetail != "":
		return st.ConfRuleDetail
	case !st.ConfOwnerOK || !st.ConfModeOK:
		return "the pam_namespace drop-in is not root-owned and non-writable"
	case !st.HelperPresent:
		return "the pam_exec precondition helper is missing"
	case !st.HelperIntegrityOK:
		return "the pam_exec precondition helper has drifted from its manifest"
	case !st.HelperOwnerOK || !st.HelperModeOK:
		return "the pam_exec precondition helper is not root-owned and non-writable"
	case !st.HelperManifestPresent:
		return "the pam_exec helper manifest is missing"
	case !st.HelperManifestOwnerOK || !st.HelperManifestModeOK:
		return "the pam_exec helper manifest is not root-owned and non-writable"
	case !st.HelperDirPresent:
		return "the pam_exec helper directory is missing"
	case !st.HelperDirOwnerOK || !st.HelperDirModeOK:
		return "the pam_exec helper directory is not root-owned and non-writable"
	case !st.PAMBlockPresent:
		return "the agent-scoped sshd PAM session block is missing or not intact"
	case st.DeployedVerifyGroup != st.AgentGroup:
		return "the deployed sshd verifier names a different agent group than this configuration"
	case !st.AgentGroupPresent:
		return "the agent isolation group does not exist on this host"
	case !st.InstanceRootPresent:
		return "the /tmp instance parent directory is missing"
	case !st.InstanceRootOwnerOK || !st.InstanceRootModeOK:
		return "the /tmp instance parent is not root-owned mode 0000"
	}
	return ""
}

// tmpIsolation returns the cached (level, detail) /tmp isolation report for
// this daemon (DF-BUNKER-9). TmpNamespaceStatus stats the host's PAM/sshd
// configuration and runs getent, so the probe runs exactly ONCE per process
// (sync.Once) and ServerInfo — a hot path — reuses the result. A nil
// configuration (tests construct the service without one) or a probe failure
// degrades to ("unknown", …); it never makes ServerInfo error or panic.
func (s *bunkerdService) tmpIsolation() (level, detail string) {
	s.tmpIsolationOnce.Do(func() {
		s.tmpIsolationLevel, s.tmpIsolationDetail = s.probeTmpIsolation()
	})
	return s.tmpIsolationLevel, s.tmpIsolationDetail
}

// probeTmpIsolation builds the hostsetup options the same way
// internal/agent.(*AgentManager).hostSetup does (configured agent group and
// private-/tmp instance root over the production defaults), minus the command
// runner — status reporting observes the host, it never changes it.
func (s *bunkerdService) probeTmpIsolation() (level, detail string) {
	if s.cfg == nil {
		return tmpIsolationUnknown, "daemon configuration is unavailable — /tmp isolation state cannot be verified"
	}
	iso := s.cfg.Agent.Isolation
	iso.Defaults()
	o := hostsetup.DefaultOptions()
	o.AgentGroup = iso.AgentGroup
	o.TmpInstanceRoot = iso.PrivateTmpRoot
	o = o.WithDefaults()
	return tmpIsolationFromState(o.TmpNamespaceStatus())
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

	// Validate the image spec up front so invalid values surface as
	// CodeInvalidArgument BEFORE user creation, port allocation, Docker
	// startup, or any image build (GAP-064). Disabled features reject specs
	// the same way. The spec is re-validated (cheaply) inside the agent
	// manager; this early check is the RPC contract boundary.
	if req.Msg.GetImageSpec() != nil {
		if !s.cfg.Agent.ImageSpec.Enabled {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("image spec support is disabled on this server"))
		}
		if _, err := imagespec.FromProto(req.Msg.GetImageSpec()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid image spec: %w", err))
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

// StopAgent pauses an agent: its session units and processes are stopped while
// the agent itself — user, home, container, port range and tracker record —
// survives, so it can be resumed with StartAgent (GAP-071).
func (s *bunkerdService) StopAgent(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	resp, err := s.agentMgr.StopAgent(ctx, req.Msg.GetAgentId())
	if err != nil {
		s.logger.Error("stop agent failed", "agent_id", req.Msg.GetAgentId(), "error", err)
		return nil, lifecycleConnectError(respStatus(resp), err)
	}
	return connect.NewResponse(resp), nil
}

// StartAgent resumes a stopped agent (GAP-071).
func (s *bunkerdService) StartAgent(ctx context.Context, req *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
	resp, err := s.agentMgr.StartAgent(ctx, req.Msg.GetAgentId())
	if err != nil {
		s.logger.Error("start agent failed", "agent_id", req.Msg.GetAgentId(), "error", err)
		return nil, lifecycleConnectError(respStatus(resp), err)
	}
	return connect.NewResponse(resp), nil
}

// RestartAgent stops and starts an agent in one call and resets its heartbeat
// expiry — the recovery path for a wedged session (GAP-071).
func (s *bunkerdService) RestartAgent(ctx context.Context, req *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
	resp, err := s.agentMgr.RestartAgent(ctx, req.Msg.GetAgentId())
	if err != nil {
		s.logger.Error("restart agent failed", "agent_id", req.Msg.GetAgentId(), "error", err)
		return nil, lifecycleConnectError(respStatus(resp), err)
	}
	return connect.NewResponse(resp), nil
}

// lifecycleResponse is the shape the three lifecycle responses share: the
// agent id plus a status drawn from the lifecycle vocabulary.
type lifecycleResponse interface {
	GetAgentId() string
	GetStatus() string
}

// respStatus reads the status out of any lifecycle response (nil-safe), so the
// error mapping never dereferences a nil response.
func respStatus(resp lifecycleResponse) string {
	if resp == nil {
		return ""
	}
	return resp.GetStatus()
}

// lifecycleConnectError maps a lifecycle manager error onto the connect codes
// DestroyAgent established: an unknown agent is CodeNotFound, everything else
// is CodeInternal. A stopped agent is NOT routed here — the RPCs that cannot
// serve a stopped agent (Exec/Run/Heartbeat) return CodeFailedPrecondition
// with the "agent_stopped" token instead.
func lifecycleConnectError(status string, err error) error {
	if status == agent.StatusNotFound {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// stoppedPreconditionError maps the agent package's stopped sentinel onto
// CodeFailedPrecondition, keeping the sentinel's message (which carries the
// stable token "agent_stopped") intact for the client.
func stoppedPreconditionError(err error) error {
	return connect.NewError(connect.CodeFailedPrecondition, err)
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
	// GAP-071: a stopped agent still EXISTS — a command must not be attempted
	// against it, and the client must be able to tell "stopped" from "gone"
	// (CodeFailedPrecondition + agent_stopped, never CodeNotFound).
	if err := agent.StoppedStatusError(rec, agentID); err != nil {
		return stoppedPreconditionError(err)
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
	// GAP-067: when containment disclosure is enabled the same explicit env
	// injection path that carries PATH/DOCKER_HOST/TMPDIR also carries
	// BUNKER_SANDBOX=1. Disabled → the built commands are byte-identical to
	// the pre-GAP-067 output.
	disclosed := s.cfg != nil && s.cfg.Containment.Disclosure
	// GAP-069: an agent spawned with an image spec carries its image ref on the
	// tracker record (and in the durable registry, so a replayed/adopted agent
	// keeps it). With a ref set, exec runs the command INSIDE a fresh container
	// of that image through the agent's own rootless dockerd, instead of in the
	// bare host user context. An empty ref (no image spec) is delegated
	// unchanged to the pre-GAP-069 host-context builders.
	imageRef := rec.Image
	var cmd *exec.Cmd
	if req.Msg.GetRaw() {
		cmd = buildExecSSHRawCommandImage(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.Command, req.Msg.Args, disclosed, imageRef)
	} else if req.Msg.GetScriptContent() != "" {
		cmd = buildExecSSHScriptCommandImage(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.GetScriptContent(), disclosed, imageRef)
	} else {
		cmd = buildExecSSHCommandImage(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.Command, req.Msg.Args, disclosed, imageRef)
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

	// Stream stdout and stderr concurrently. The stdout streamer records
	// whether ANY process stdout was forwarded and whether the last byte
	// ended in '\n'; those facts are read AFTER wg.Wait() to build the
	// marker frame (GAP-067) — writes are synchronized by the WaitGroup.
	var wg sync.WaitGroup
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	var stdoutSent bool
	var stdoutEndsNewline bool
	// Byte counters for the streamed process output. The stdout count is a
	// minimal extension of the existing sent/newline state; the stderr
	// count has no prior equivalent and is what lets the session-denial
	// classifier distinguish "ssh said nothing" from "ssh reported an
	// error". Both are written by their streamer goroutine and read after
	// wg.Wait(), which is what synchronizes them.
	var stdoutBytes int
	var stderrBytes int

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(stdoutDone)
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if markerCountsAsSent(n) {
				stdoutSent = true
				stdoutBytes += n
				stdoutEndsNewline = buf[n-1] == '\n'
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
				stderrBytes += n
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

	// GAP-067 containment disclosure: after all process output frames and
	// before the exit-code frame, an allowed system-info probe gets ONE
	// self-describing marker line on stdout. The frame is built from the
	// ACTUAL streamed-stdout state (any bytes sent? last byte a newline?)
	// so a probe whose output lacks a trailing newline still gets the
	// marker on its own line client-side, while empty output and
	// newline-terminated output get no extra blank line. The command's
	// exit code — success or failure — is sent unchanged below. Non-probe
	// commands and all execs when the flag is disabled produce no marker
	// and no extra bytes. Script uploads (req.Msg.Command empty) are never
	// probes.
	if disclosed && isContainmentProbe(req.Msg.Command, req.Msg.Args) {
		if frame := markerFrameForStream(stdoutSent, stdoutEndsNewline, true); frame != "" {
			if err := stream.Send(&v1.ExecAgentResponse{
				Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte(frame)},
			}); err != nil {
				s.logger.Warn("send containment marker", "error", err)
			}
		}
	}

	// Session-denial diagnostic (INT-DEMO-001): a session rejected by PAM
	// before the command ran leaves ssh exiting 254 with the banner as the
	// only stdout and nothing on stderr, so the operator otherwise learns
	// nothing. When that exact signature is present, stream ONE stderr
	// frame naming the likely cause and the checks. The exit code below is
	// unchanged, and this sits after the GAP-067 marker block so no
	// existing frame is suppressed or reordered.
	if diag, denied := classifyExecSessionDenial(int(exitCode), stderrBytes, stdoutBytes); denied {
		s.logger.Warn("exec session denied before command ran",
			"agent_id", agentID, "exit_code", exitCode)
		if err := stream.Send(&v1.ExecAgentResponse{
			Output: &v1.ExecAgentResponse_Stderr{Stderr: []byte(diag + "\n")},
		}); err != nil {
			s.logger.Warn("send session-denial diagnostic", "error", err)
		}
	}

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
	rec := s.tracker.Get(req.Msg.GetAgentId())
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.GetAgentId()))
	}
	// GAP-071: RunAgent against a stopped agent is a failed precondition, not
	// a missing agent.
	if err := agent.StoppedStatusError(rec, req.Msg.GetAgentId()); err != nil {
		return nil, stoppedPreconditionError(err)
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
	// The TTL to extend by: the configured default, or 6h when unset.
	ttl := 6 * time.Hour
	if s.cfg.Agent.DefaultTTL > 0 {
		ttl = s.cfg.Agent.DefaultTTL
	}
	// GAP-071: a stopped agent cannot heartbeat — extending the TTL of an
	// agent that is not running would silently keep it alive until the reaper
	// destroys it. The client gets the distinct agent_stopped signal so it can
	// start or restart the agent instead.
	if rec := s.tracker.Get(req.Msg.AgentId); rec != nil {
		if err := agent.StoppedStatusError(rec, req.Msg.AgentId); err != nil {
			return nil, stoppedPreconditionError(err)
		}
	}
	// Route through the agent manager so the durable registry records the
	// extension (GAP-070). The manager applies the same never-SHRINK rule:
	// heartbeating a long-TTL agent (e.g. 720h) must not reset it to the
	// default 6h — a shorter expiry would silently destroy the agent on TTL
	// expiry (userdel + data loss) between renewal runs.
	if s.heartbeats != nil {
		rec, err := s.heartbeats.Heartbeat(req.Msg.AgentId, ttl)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return connect.NewResponse(&v1.HeartbeatAgentResponse{
			AgentId:      req.Msg.AgentId,
			ExpiresAt:    rec.ExpiresAt.Format(time.RFC3339),
			Acknowledged: true,
		}), nil
	}
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
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
//
// GAP-075: TMPDIR is /tmp — the agent session's OWN private /tmp instance,
// bound by pam_namespace for members of the agent group (every agent joins it
// at spawn; ordinary operator sessions and root are not members and keep the
// host /tmp). The session observes a genuinely private /tmp (not a per-agent
// directory in a shared namespace), so no command can read or collide with
// another agent's temporary files or root's.
func buildAgentExecCommand(agentID, userHome, command string, args []string, disclosed bool) string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	// GAP-075: TMPDIR is the enforced private /tmp of the agent's session
	// (pam_namespace binds the session's own /tmp instance there). The legacy
	// /run/bunker/<id>/tmp is not an isolation boundary and is no longer
	// advertised as TMPDIR.
	tmpDir := config.IsolationTmpDir
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	envFile := fmt.Sprintf("/run/bunker/%s/env", agentID)
	// GAP-067 containment disclosure: the sandbox env var rides the SAME
	// explicit env(1) injection path as PATH/DOCKER_HOST/TMPDIR, so it is
	// visible to the wrapped command regardless of sshd config. When
	// disclosed is false this string is empty and the built command is
	// byte-identical to the pre-GAP-067 output.
	sandboxEnv := ""
	if disclosed {
		sandboxEnv = containmentSandboxEnv + " "
	}
	remoteCmd := buildAgentRemoteCmd(command, args)
	// set -a (allexport) around the source so injected vars are exported to
	// the child `sh -c` below — plain KEY=VALUE lines would otherwise only be
	// shell variables, invisible to the wrapped command. The [ -f ] guard
	// keeps a fresh agent (no env file yet) from making dash exit 2 on the
	// failed dot-source.
	return fmt.Sprintf("set -a; [ -f %s ] && . %s 2>/dev/null; set +a; env PATH=%s DOCKER_HOST=unix://%s TMPDIR=%s %ssh -c %s",
		envFile, envFile, agentPath, dockerSockPath, tmpDir, sandboxEnv, shellQuoteSingle(remoteCmd))
}

// buildAgentRemoteCmd joins the user command with its shell-quoted args into
// the single command string both the host-context and the image-container
// shell exec paths hand to `sh -c`.
func buildAgentRemoteCmd(command string, args []string) string {
	if len(args) == 0 {
		return command
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuoteSingle(a)
	}
	return command + " " + strings.Join(quoted, " ")
}

// containerRunPrefix returns the leading `docker run` argv for an image-backed
// exec (GAP-069): --rm so a one-shot exec never leaves a container behind, plus
// the containment disclosure marker as a container env var when enabled, so the
// in-container view matches the host-context one (GAP-067).
func containerRunPrefix(disclosed bool) []string {
	argv := []string{"docker", "run", "--rm"}
	if disclosed {
		argv = append(argv, "-e", containmentSandboxEnv)
	}
	return argv
}

// buildAgentImageExecCommand is buildAgentExecCommand (own rootless dockerd
// reachable through DOCKER_HOST, agent env file sourced) with the user command
// executed inside a FRESH container of imageRef instead of in the bare host
// user context (GAP-069). The docker CLI runs on the REMOTE side as the agent
// user against the agent's own socket; the host is never asked to run docker.
//
// The command is re-quoted with the same POSIX single-quote scheme the
// host-context path uses, because it now crosses two shell layers: the agent's
// sshd shell (which parses the docker argv) and the container's `sh -lc`.
func buildAgentImageExecCommand(agentID, userHome, command string, args []string, disclosed bool, imageRef string) string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	// GAP-075: TMPDIR is the enforced private /tmp of the agent's session
	// (pam_namespace binds the session's own /tmp instance there). The legacy
	// /run/bunker/<id>/tmp is not an isolation boundary and is no longer
	// advertised as TMPDIR.
	tmpDir := config.IsolationTmpDir
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	envFile := fmt.Sprintf("/run/bunker/%s/env", agentID)
	sandboxEnv := ""
	if disclosed {
		sandboxEnv = containmentSandboxEnv + " "
	}
	remoteCmd := buildAgentRemoteCmd(command, args)

	// The agent home is bind-mounted at the SAME absolute path and used as the
	// working directory, so paths that are valid in the host context (the
	// agent's home, files an operator just copied in) stay valid in-container.
	runArgv := containerRunPrefix(disclosed)
	runArgv = append(runArgv, "-v", userHome+":"+userHome, "-w", userHome, imageRef, "sh", "-lc",
		shellQuoteSingle(remoteCmd))

	return fmt.Sprintf("set -a; [ -f %s ] && . %s 2>/dev/null; set +a; env PATH=%s DOCKER_HOST=unix://%s TMPDIR=%s %ssh -c %s",
		envFile, envFile, agentPath, dockerSockPath, tmpDir, sandboxEnv,
		shellQuoteSingle(strings.Join(runArgv, " ")))
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
func buildAgentRawExecCommand(agentID, userHome, command string, args []string, disclosed bool) []string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	// GAP-075: TMPDIR is the enforced private /tmp of the agent's session
	// (pam_namespace binds the session's own /tmp instance there). The legacy
	// /run/bunker/<id>/tmp is not an isolation boundary and is no longer
	// advertised as TMPDIR.
	tmpDir := config.IsolationTmpDir
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	// sshd's ForceCommand or default shell may still receive a string, but
	// passing a command with args and using ssh's internal exec channel (when
	// the remote shell is not forced) will execve directly. We keep a tiny
	// wrapper here: env(1) so we can set DOCKER_HOST and TMPDIR before the real binary.
	argv := []string{
		"env",
		"PATH=" + agentPath,
		"DOCKER_HOST=unix://" + dockerSockPath,
		"TMPDIR=" + tmpDir,
	}
	// GAP-067 containment disclosure: BUNKER_SANDBOX=1 rides the same env(1)
	// argv as PATH/DOCKER_HOST/TMPDIR. When disclosure is disabled no extra
	// argv element is appended and the argv is identical to pre-GAP-067.
	if disclosed {
		argv = append(argv, containmentSandboxEnv)
	}
	argv = append(argv, command)
	return append(argv, args...)
}

// buildAgentImageRawExecCommand is buildAgentRawExecCommand with the command
// executed inside a fresh container of imageRef (GAP-069). Raw mode keeps its
// no-intermediate-shell contract: the command and args are handed to docker run
// verbatim (`docker run --rm <image> <command> <args...>`) with no sh wrapper,
// so no shell metacharacter is interpreted a layer earlier than it was before.
// The home directory is NOT bind-mounted in raw mode — raw mode never sourced
// the agent env file either; use shell mode when host paths are needed.
func buildAgentImageRawExecCommand(agentID, userHome, command string, args []string, disclosed bool, imageRef string) []string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	// GAP-075: TMPDIR is the enforced private /tmp of the agent's session
	// (pam_namespace binds the session's own /tmp instance there). The legacy
	// /run/bunker/<id>/tmp is not an isolation boundary and is no longer
	// advertised as TMPDIR.
	tmpDir := config.IsolationTmpDir
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	argv := []string{
		"env",
		"PATH=" + agentPath,
		"DOCKER_HOST=unix://" + dockerSockPath,
		"TMPDIR=" + tmpDir,
	}
	if disclosed {
		argv = append(argv, containmentSandboxEnv)
	}
	argv = append(argv, containerRunPrefix(disclosed)...)
	argv = append(argv, imageRef, command)
	return append(argv, args...)
}

// buildAgentScriptCommand writes scriptContent to a remote file and returns the
// shell command that executes it. The file is written via ssh heredoc.
func buildAgentScriptCommand(agentID, userHome, scriptContent string, disclosed bool) string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	// GAP-075: TMPDIR is the enforced private /tmp of the agent's session
	// (pam_namespace binds the session's own /tmp instance there). The legacy
	// /run/bunker/<id>/tmp is not an isolation boundary and is no longer
	// advertised as TMPDIR.
	tmpDir := config.IsolationTmpDir
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	scriptPath := filepath.Join(userHome, ".bunker", "exec-script.sh")
	envFile := fmt.Sprintf("/run/bunker/%s/env", agentID)
	// GAP-067 containment disclosure: same env(1) injection as the shell
	// exec path. Empty string when disabled — byte-identical output.
	sandboxEnv := ""
	if disclosed {
		sandboxEnv = containmentSandboxEnv + " "
	}
	// Use POSIX heredoc to create + chmod + execute the script in one SSH call.
	// We quote the EOF delimiter to prevent expansion of the script body.
	escaped := strings.ReplaceAll(scriptContent, "'", "'\\''")
	return fmt.Sprintf(
		"mkdir -p %q && cat > %q <<'EOFSCRIPT'\n%s\nEOFSCRIPT\nchmod +x %q && set -a; [ -f %s ] && . %s 2>/dev/null; set +a; env PATH=%s DOCKER_HOST=unix://%s TMPDIR=%s %s%q",
		filepath.Dir(scriptPath), scriptPath, escaped, scriptPath, envFile, envFile, agentPath, dockerSockPath, tmpDir, sandboxEnv, scriptPath,
	)
}

// buildAgentImageScriptCommand is buildAgentScriptCommand with the uploaded
// script executed inside a fresh container of imageRef (GAP-069). The upload
// flow is unchanged (heredoc → chmod +x) so the script file still lands in the
// agent's home; only the final invocation is wrapped, and it runs the script by
// PATH inside the container, which is possible because the agent home is
// bind-mounted at the same absolute path.
func buildAgentImageScriptCommand(agentID, userHome, scriptContent string, disclosed bool, imageRef string) string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	// GAP-075: TMPDIR is the enforced private /tmp of the agent's session
	// (pam_namespace binds the session's own /tmp instance there). The legacy
	// /run/bunker/<id>/tmp is not an isolation boundary and is no longer
	// advertised as TMPDIR.
	tmpDir := config.IsolationTmpDir
	agentBinPath := filepath.Join(userHome, "bin")
	agentPath := agentBinPath + ":" + agentExecBasePath
	scriptPath := filepath.Join(userHome, ".bunker", "exec-script.sh")
	envFile := fmt.Sprintf("/run/bunker/%s/env", agentID)
	sandboxEnv := ""
	if disclosed {
		sandboxEnv = containmentSandboxEnv + " "
	}
	escaped := strings.ReplaceAll(scriptContent, "'", "'\\''")

	runArgv := containerRunPrefix(disclosed)
	runArgv = append(runArgv, "-v", userHome+":"+userHome, imageRef, "sh", shellQuoteSingle(scriptPath))

	return fmt.Sprintf(
		"mkdir -p %q && cat > %q <<'EOFSCRIPT'\n%s\nEOFSCRIPT\nchmod +x %q && set -a; [ -f %s ] && . %s 2>/dev/null; set +a; env PATH=%s DOCKER_HOST=unix://%s TMPDIR=%s %s%s",
		filepath.Dir(scriptPath), scriptPath, escaped, scriptPath, envFile, envFile, agentPath, dockerSockPath, tmpDir, sandboxEnv,
		strings.Join(runArgv, " "),
	)
}

// buildExecSSHCommand returns an exec.Cmd that runs buildAgentExecCommand
// inside the agent via ssh.  The remote script is passed as a single quoted
// "sh -c '...'" argument to OpenSSH so that multi-token commands such as
// "docker version" are not misparsed by the inner shell.
func buildExecSSHCommand(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string, disclosed bool) *exec.Cmd {
	wrappedCmd := buildAgentExecCommand(agentID, userHome, command, args, disclosed)
	sshRemoteCmd := fmt.Sprintf("sh -c %s", shellQuoteSingle(wrappedCmd))
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, sshRemoteCmd)
}

// buildExecSSHRawCommand returns an exec.Cmd that runs command+args directly
// without a shell wrapper. Each arg is passed as a separate ssh argument; sshd
// will attempt to exec the requested program directly.
func buildExecSSHRawCommand(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string, disclosed bool) *exec.Cmd {
	remoteArgv := buildAgentRawExecCommand(agentID, userHome, command, args, disclosed)
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, remoteArgv...)
}

// buildExecSSHScriptCommand returns an exec.Cmd that uploads scriptContent via
// heredoc and executes it on the agent.
func buildExecSSHScriptCommand(ctx context.Context, agentID, sshKeyPath, userHome, scriptContent string, disclosed bool) *exec.Cmd {
	wrappedCmd := buildAgentScriptCommand(agentID, userHome, scriptContent, disclosed)
	sshRemoteCmd := fmt.Sprintf("sh -c %s", shellQuoteSingle(wrappedCmd))
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, sshRemoteCmd)
}

// The *Image variants below are the GAP-069 exec path: when the agent record
// carries an image-spec ref, the user command runs inside a fresh container of
// that image (through the agent's own rootless dockerd) instead of in the bare
// host user context. An EMPTY imageRef delegates to the original builder
// unchanged, so an agent spawned without an image spec keeps the exact
// pre-GAP-069 command — the delegation is what makes that byte-identity
// structural rather than a promise.

// buildExecSSHCommandImage is buildExecSSHCommand for an image-backed agent.
func buildExecSSHCommandImage(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string, disclosed bool, imageRef string) *exec.Cmd {
	if imageRef == "" {
		return buildExecSSHCommand(ctx, agentID, sshKeyPath, userHome, command, args, disclosed)
	}
	wrappedCmd := buildAgentImageExecCommand(agentID, userHome, command, args, disclosed, imageRef)
	sshRemoteCmd := fmt.Sprintf("sh -c %s", shellQuoteSingle(wrappedCmd))
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, sshRemoteCmd)
}

// buildExecSSHRawCommandImage is buildExecSSHRawCommand for an image-backed agent.
func buildExecSSHRawCommandImage(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string, disclosed bool, imageRef string) *exec.Cmd {
	if imageRef == "" {
		return buildExecSSHRawCommand(ctx, agentID, sshKeyPath, userHome, command, args, disclosed)
	}
	remoteArgv := buildAgentImageRawExecCommand(agentID, userHome, command, args, disclosed, imageRef)
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, remoteArgv...)
}

// buildExecSSHScriptCommandImage is buildExecSSHScriptCommand for an image-backed agent.
func buildExecSSHScriptCommandImage(ctx context.Context, agentID, sshKeyPath, userHome, scriptContent string, disclosed bool, imageRef string) *exec.Cmd {
	if imageRef == "" {
		return buildExecSSHScriptCommand(ctx, agentID, sshKeyPath, userHome, scriptContent, disclosed)
	}
	wrappedCmd := buildAgentImageScriptCommand(agentID, userHome, scriptContent, disclosed, imageRef)
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
	logger     *slog.Logger
	tracker    *resource.Tracker
	heartbeats heartbeatManager
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
	// Routed through the agent manager so the durable registry sees the
	// extension (GAP-070); falls back to the tracker when unwired.
	if s.heartbeats != nil {
		rec, err := s.heartbeats.Heartbeat(req.Msg.AgentId, 6*time.Hour)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return connect.NewResponse(&v1.HeartbeatAgentResponse{
			AgentId:      req.Msg.AgentId,
			ExpiresAt:    rec.ExpiresAt.Format(time.RFC3339),
			Acknowledged: true,
		}), nil
	}
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
