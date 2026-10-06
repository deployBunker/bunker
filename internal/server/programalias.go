package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"connectrpc.com/connect"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/programalias"
	"github.com/deployBunker/bunker/internal/resource"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// This file is the daemon side of GAP-066 (docker-as-installer).
//
// The flow an operator gets:
//
//	bunker alias set yq --image mikefarah/yq:4
//	bunker run <agent> -- yq --version          # pulls on first use
//
// and the flow an agent gets, for free, afterwards:
//
//	echo 'yq --version' > s.sh && bunker exec --script <agent> s.sh
//
// The second works because the daemon materializes the alias as a shim in the
// agent's own ~/bin (which is first on the exec PATH) and then runs that same
// shim for the first form — one file, two callers, so the two cannot drift.
//
// Authority stays on the daemon side throughout: the agent cannot name an
// image, a mount or a limit.  The image is pulled by the daemon (it is the
// daemon's job, and the daemon's journal carries the first-run latency), every
// mount is validated against the agent's home before any argv exists, and the
// container is given no docker socket.

// aliasRegistry returns the alias store, or nil when the service was built
// without one (unit tests that construct a bare bunkerdService).  A nil
// registry disables alias resolution entirely: execs behave exactly as they
// did before this feature existed.
func (s *bunkerdService) aliasRegistry() *programalias.Registry {
	return s.programAliases
}

// programAliasToProto converts a store alias to its wire form.
func programAliasToProto(a programalias.Alias) *v1.ProgramAlias {
	out := &v1.ProgramAlias{
		Name:        a.Name,
		Image:       a.Image,
		Entrypoint:  append([]string(nil), a.Entrypoint...),
		Network:     a.Network,
		Description: a.Description,
	}
	for _, m := range a.Mounts {
		out.Mounts = append(out.Mounts, &v1.ProgramAliasMount{
			Host:      m.Host,
			Container: m.Container,
			ReadOnly:  m.ReadOnly,
		})
	}
	return out
}

// programAliasFromProto converts a wire alias to its store form.
func programAliasFromProto(p *v1.ProgramAlias) programalias.Alias {
	if p == nil {
		return programalias.Alias{}
	}
	out := programalias.Alias{
		Name:        p.GetName(),
		Image:       p.GetImage(),
		Entrypoint:  append([]string(nil), p.GetEntrypoint()...),
		Network:     p.GetNetwork(),
		Description: p.GetDescription(),
	}
	for _, m := range p.GetMounts() {
		if m == nil {
			continue
		}
		out.Mounts = append(out.Mounts, programalias.Mount{
			Host:      m.GetHost(),
			Container: m.GetContainer(),
			ReadOnly:  m.GetReadOnly(),
		})
	}
	return out
}

// aliasValidationError reports whether err is a caller mistake (a malformed
// name/image/mount) as opposed to a store fault.
func aliasValidationError(err error) bool {
	for _, sentinel := range []error{
		programalias.ErrNameRequired,
		programalias.ErrNameInvalid,
		programalias.ErrImageRequired,
		programalias.ErrImageInvalid,
		programalias.ErrArgInvalid,
		programalias.ErrMountNotUnderHome,
		programalias.ErrMountOutsideHomeRoot,
		programalias.ErrMountHostRequired,
		programalias.ErrMountHostRelative,
		programalias.ErrMountContainerRelative,
		programalias.ErrMountDockerSock,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// ListProgramAliases returns every registered alias.
func (s *bunkerdService) ListProgramAliases(_ context.Context, _ *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	reg := s.aliasRegistry()
	if reg == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("program aliases are not enabled on this daemon"))
	}
	aliases, err := reg.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list program aliases: %w", err))
	}
	resp := &v1.ListProgramAliasesResponse{StorePath: reg.Path()}
	for _, a := range aliases {
		resp.Aliases = append(resp.Aliases, programAliasToProto(a))
	}
	return connect.NewResponse(resp), nil
}

// PutProgramAlias creates or replaces an alias.  Validation failures are
// CodeInvalidArgument and name the offending field.
func (s *bunkerdService) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	reg := s.aliasRegistry()
	if reg == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("program aliases are not enabled on this daemon"))
	}
	alias := programAliasFromProto(req.Msg.GetAlias())
	if err := programalias.ValidateAlias(alias); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	_, existed, err := reg.Get(alias.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read program aliases: %w", err))
	}
	if err := reg.Put(alias); err != nil {
		if aliasValidationError(err) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("write program aliases: %w", err))
	}
	status := "created"
	if existed {
		status = "updated"
	}
	s.logger.Info("program alias registered",
		"name", alias.Name, "image", alias.Image, "status", status)
	return connect.NewResponse(&v1.PutProgramAliasResponse{
		Alias:  programAliasToProto(alias),
		Status: status,
	}), nil
}

// DeleteProgramAlias removes an alias by name.
func (s *bunkerdService) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	reg := s.aliasRegistry()
	if reg == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("program aliases are not enabled on this daemon"))
	}
	name := req.Msg.GetName()
	if err := programalias.ValidateName(name); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	err := reg.Delete(name)
	switch {
	case err == nil:
		s.logger.Info("program alias deleted", "name", name)
		return connect.NewResponse(&v1.DeleteProgramAliasResponse{Name: name, Deleted: true}), nil
	case errors.Is(err, programalias.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, err)
	default:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete program alias: %w", err))
	}
}

// lookupProgramAlias resolves a command token against the registry.
//
// Resolution is deliberately narrow: only a BARE program name is considered.
// A command containing a `/` (an absolute or relative path) is the caller
// explicitly choosing a native binary, and a command carrying punctuation
// that is not legal in an alias name is not an alias at all.  Either way the
// registry is not consulted, which is also the documented escape hatch from an
// alias (`/usr/bin/yq`).
func (s *bunkerdService) lookupProgramAlias(command string) (programalias.Alias, bool) {
	reg := s.aliasRegistry()
	if reg == nil || command == "" || len(command) > programalias.NameMaxLen {
		return programalias.Alias{}, false
	}
	if err := programalias.ValidateName(command); err != nil {
		return programalias.Alias{}, false
	}
	a, ok, err := reg.Get(command)
	if err != nil || !ok {
		return programalias.Alias{}, false
	}
	return a, true
}

// programAliasLimits shapes the agent's spawn-time resource envelope into the
// container limits.  An agent with no recorded limits gets the alias package's
// modest defaults, never "unlimited".
func programAliasLimits(rec *resource.AgentRecord) programalias.Limits {
	if rec == nil || rec.Limits == nil {
		return programalias.Limits{}
	}
	return programalias.Limits{
		MemoryBytes: int64(rec.Limits.GetMemoryMaxBytes()),
		CPUs:        rec.Limits.GetCpuQuota(),
	}
}

// programAliasContainerEnv is the container-side environment the daemon asks
// for.  Today that is the containment-disclosure marker and nothing else: the
// daemon never forwards the caller's environment into an alias container.
func programAliasContainerEnv(disclosed bool) []string {
	if !disclosed {
		return nil
	}
	return []string{containmentSandboxEnv}
}

// agentDockerCommand runs a `docker <args...>` subcommand on the agent host
// against the agent's OWN rootless socket, and returns its combined output.
// It is a package-level seam so the pull/first-run path can be driven without
// a live agent.
var agentDockerCommand = func(ctx context.Context, agentID, sshKeyPath, dockerHost string, args ...string) ([]byte, error) {
	remote := append([]string{"env", "DOCKER_HOST=" + dockerHost, "docker"}, args...)
	cmd := buildSSHBaseCommand(ctx, agentID, sshKeyPath, remote...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if runErr := cmd.Run(); runErr != nil {
		return out.Bytes(), fmt.Errorf("%v: %s", runErr, truncateForLog(out.String(), 512))
	}
	return out.Bytes(), nil
}

// ensureProgramAliasImage makes the alias image present on the agent before a
// run, and reports how long a first-run pull took.
//
// This is the daemon's job, not the agent's: the agent's own command line
// carries no `docker pull`, so a program alias can never turn into an
// agent-driven registry fetch.  The image cache short-circuits the whole step
// for an image this daemon process has already confirmed — which is what makes
// the second run of an alias observably cheaper than the first (literally no
// extra round trip).
//
// A pull failure is returned to the caller so the exec fails loudly instead of
// racing a container start against a half-finished image.
func (s *bunkerdService) ensureProgramAliasImage(ctx context.Context, agentID, sshKeyPath, image string) (time.Duration, bool, error) {
	if image == "" {
		return 0, false, nil
	}
	if s.programAliasImages.Has(image) {
		return 0, false, nil
	}
	dockerHost := "unix:///run/bunker/" + agentID + "/docker.sock"

	if _, err := agentDockerCommand(ctx, agentID, sshKeyPath, dockerHost, "image", "inspect", image); err == nil {
		s.programAliasImages.Add(image)
		return 0, false, nil
	}

	start := time.Now()
	_, pullErr := agentDockerCommand(ctx, agentID, sshKeyPath, dockerHost, "pull", image)
	elapsed := time.Since(start)
	if pullErr != nil {
		return elapsed, true, fmt.Errorf("pull program-alias image %s: %w", image, pullErr)
	}
	s.programAliasImages.Add(image)
	return elapsed, true, nil
}

// truncateForLog bounds a captured command output for an error message.
func truncateForLog(s string, max int) string {
	s = string(bytes.TrimSpace([]byte(s)))
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// programAliasLimitsString renders the effective limits for a log line.
func programAliasLimitsString(l programalias.Limits) string {
	mem := l.MemoryBytes
	if mem <= 0 {
		mem = programalias.DefaultMemoryBytes
	}
	cpus := l.CPUs
	if cpus <= 0 {
		cpus = programalias.DefaultCPUs
	}
	pids := l.Pids
	if pids <= 0 {
		pids = programalias.DefaultPids
	}
	return "mem=" + strconv.FormatInt(mem, 10) + " cpus=" + strconv.FormatFloat(cpus, 'f', -1, 64) +
		" pids=" + strconv.Itoa(pids)
}

// logProgramAliasRun records one alias-based exec.  Deliberately INFO: an
// operator reading the journal needs to see that a program came from a
// container rather than from the host, which image it came from, and — on the
// first use only — how long the pull cost.
func (s *bunkerdService) logProgramAliasRun(agentID string, a programalias.Alias, pulled bool, pullDur time.Duration, limits programalias.Limits) {
	attrs := []any{
		"agent_id",
		agentID, "alias", a.Name,
		"image", a.Image,
		"limits", programAliasLimitsString(limits),
		"pull", pulled,
	}
	if pulled {
		attrs = append(attrs, "first_run_pull_ms", pullDur.Milliseconds())
	}
	s.logger.Info("program alias exec", attrs...)
}

// errProgramAliasIdentity is returned when an alias container cannot be given
// the agent's own identity because the agent user is unresolvable.  The
// image-spec path refuses in the same situation (DF-BUNKER-77); a program
// alias must not silently run as a foreign id either.
var errProgramAliasIdentity = errors.New("agent user unresolved; refusing to run a program-alias container as an unknown identity")

// buildAgentProgramAliasCommand builds the remote shell command for an
// alias-based exec: the daemon's env/PATH preamble, then the alias script
// (shim install + exec) handed to `sh -c`.  The shape mirrors
// buildAgentExecCommand so the two paths differ only in the script they run.
func buildAgentProgramAliasCommand(agentID, userHome string, a programalias.Alias, args []string, limits programalias.Limits, disclosed bool) (string, error) {
	if _, ok := agentContainerIdentityFlag(agentID); !ok {
		return "", errProgramAliasIdentity
	}
	script, err := programalias.RemoteExecScript(a, userHome, args, limits, programAliasContainerEnv(disclosed))
	if err != nil {
		// A mount that is not inside the agent home reaches here: only now is
		// the home known.
		return "", err
	}
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	agentPath := userHome + "/bin:" + agentExecBasePath
	envFile := fmt.Sprintf("/run/bunker/%s/env", agentID)
	sandboxEnv := ""
	if disclosed {
		sandboxEnv = containmentSandboxEnv + " "
	}
	return fmt.Sprintf("set -a; [ -f %s ] && . %s 2>/dev/null; set +a; env PATH=%s DOCKER_HOST=unix://%s TMPDIR=%s %ssh -c %s",
		envFile, envFile, agentPath, dockerSockPath, config.IsolationTmpDir, sandboxEnv, shellQuoteSingle(script)), nil
}

// buildAgentProgramAliasRawCommand builds the remote argv for a raw-mode alias
// exec.  No shell is involved: the docker argv is passed verbatim after the
// env(1) prefix, preserving raw mode's no-intermediate-shell contract.
func buildAgentProgramAliasRawCommand(agentID, userHome string, a programalias.Alias, args []string, limits programalias.Limits, disclosed bool) ([]string, error) {
	if _, ok := agentContainerIdentityFlag(agentID); !ok {
		return nil, errProgramAliasIdentity
	}
	dockerArgv, err := programalias.RawExecArgv(a, userHome, args, limits, programAliasContainerEnv(disclosed))
	if err != nil {
		return nil, err
	}
	argv := []string{
		"env",
		"PATH=" + userHome + "/bin:" + agentExecBasePath,
		"DOCKER_HOST=unix:///run/bunker/" + agentID + "/docker.sock",
		"TMPDIR=" + config.IsolationTmpDir,
	}
	if disclosed {
		argv = append(argv, containmentSandboxEnv)
	}
	return append(argv, dockerArgv...), nil
}

// The alias exec path's builders, behind seams so a test can substitute the
// command ExecAgent runs (the same DF-BUNKER-27 pattern the other builders
// use).
var (
	execSSHCommandAliasBuilder    = buildExecSSHCommandAlias
	execSSHRawCommandAliasBuilder = buildExecSSHRawCommandAlias
)

// buildExecSSHCommandAlias is buildExecSSHCommand for an alias-based exec.
func buildExecSSHCommandAlias(ctx context.Context, agentID, sshKeyPath, userHome string, a programalias.Alias, args []string, limits programalias.Limits, disclosed bool) (*exec.Cmd, error) {
	wrapped, err := buildAgentProgramAliasCommand(agentID, userHome, a, args, limits, disclosed)
	if err != nil {
		return nil, err
	}
	sshRemoteCmd := fmt.Sprintf("sh -c %s", shellQuoteSingle(wrapped))
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, sshRemoteCmd), nil
}

// buildExecSSHRawCommandAlias is buildExecSSHRawCommand for an alias-based exec.
func buildExecSSHRawCommandAlias(ctx context.Context, agentID, sshKeyPath, userHome string, a programalias.Alias, args []string, limits programalias.Limits, disclosed bool) (*exec.Cmd, error) {
	remoteArgv, err := buildAgentProgramAliasRawCommand(agentID, userHome, a, args, limits, disclosed)
	if err != nil {
		return nil, err
	}
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, remoteArgv...), nil
}
