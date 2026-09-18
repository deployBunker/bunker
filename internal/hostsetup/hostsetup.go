// Package hostsetup performs idempotent, host-level provisioning for the
// Bunker isolation boundary (GAP-075).
//
// GAP-075 removes the shared-/tmp isolation class. Every agent runs with an
// ENFORCED private /tmp and cross-agent exchange happens only through an
// explicit, bounded shared scratch directory. Three host-level pieces make
// that true, and all three live here so they can be tested without mutating a
// developer host:
//
//  1. Private /tmp for every AGENT SSH session (scratch.go is the
//     shared-scratch half; TmpNamespace in tmpnamespace.go binds each agent
//     session's /tmp to a per-user instance directory through pam_namespace,
//     applied only to members of the agent group — ordinary operator sessions
//     keep the host /tmp).
//  2. /srv/bunker-share — the ONLY sanctioned cross-agent exchange point,
//     with group/setgid semantics and a kernel-enforced per-agent size cap
//     (scratch.go).
//  3. A size cap for the host's own /tmp tmpfs, installed as a systemd
//     drop-in for tmp.mount and applied live with a NON-destructive remount
//     (tmpcap.go). /etc/fstab is never read, rewritten, or duplicated.
//
// Everything here is idempotent: running it twice changes nothing the second
// time. Nothing here is applied implicitly by observing a struct — callers
// must call the Ensure* function, and every function takes an apply/dry-run
// decision where it would otherwise touch host state.
//
// Every path is a field of Options so tests can point the whole package at a
// temp directory (see Options.WithDefaults) instead of the real host.
package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner runs a host command and returns its combined output. It is a seam:
// production uses DefaultRunner (exec.CommandContext); tests inject a fake so
// no host state is touched and the exact argv can be asserted.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// DefaultRunner executes name with args via exec.CommandContext.
func DefaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w (output: %s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Options are the host paths, identities and limits the provisioners use.
// The zero value is valid: WithDefaults fills every empty field with the
// documented production default, so a caller that only cares about one knob
// (or a test that only redirects the paths it touches) can leave the rest
// alone.
type Options struct {
	// Root is an optional path prefix used to redirect EVERY absolute path
	// below into a sandbox tree. Tests may instead override the individual
	// paths; Root exists so a test can exercise the real default layout
	// (e.g. /srv/bunker-share, /etc/security/...) without touching the host.
	Root string

	// ScratchRoot is the single cross-agent exchange directory.
	ScratchRoot string
	// AgentGroup is the isolation group every agent joins. It is the SINGLE
	// source of truth for two things: the fail-closed pam_exec precondition
	// requires the membership of every agent session it verifies (see
	// NamespacePAMVerifyLine and PamGuardScript), and the group owns the
	// shared-scratch tree, so a member can read a peer's exchanged files. Every
	// agent gets the membership at spawn, independently of whether the shared
	// scratch is enabled. It is NOT what decides whether a session is an agent
	// session — that is the reserved `bunker-*` name pattern, so deleting this
	// group can never turn an agent session into an ordinary one.
	AgentGroup string
	// ScratchPerAgent caps each agent's scratch directory (kernel-enforced
	// tmpfs size). 0 means "unbounded", which is never used by default.
	ScratchPerAgent uint64
	// ScratchEnabled gates per-agent scratch provisioning in spawn.
	ScratchEnabled bool
	// SkipHostTmpCap suppresses the host /tmp tmpfs cap step of Apply. It
	// exists for hosts whose /tmp is managed elsewhere; the cap itself stays
	// the documented default everywhere else.
	SkipHostTmpCap bool

	// TmpInstanceRoot is the pam_namespace instance parent for /tmp. It is
	// created mode 0000 as pam_namespace requires, and holds one instance
	// directory per agent.
	TmpInstanceRoot string
	// NamespaceConfDir / NamespaceConfName hold the pam_namespace drop-in.
	// The module reads /etc/security/namespace.d in addition to the main
	// namespace.conf, so Bunker never edits the distribution's file.
	NamespaceConfDir  string
	NamespaceConfName string
	// NamespaceConfFile is the distribution's namespace.conf. Bunker never
	// writes it, but the helper reads it: pam_namespace merges it with the
	// drop-in directory, so an operator rule there can defeat the isolation.
	NamespaceConfFile string
	// SSHDConfigPath is the PAM stack for sshd sessions.
	SSHDConfigPath string

	// PamHelperDir holds the root-owned pam_exec precondition helper (see
	// PamGuardScript) and its content manifest. Both files live in one
	// root-owned directory that Bunker creates and removes with the block.
	PamHelperDir string
	// PamHelperName is the helper file name (an absolute-path join of
	// PamHelperDir and PamHelperName is what the PAM block invokes).
	PamHelperName string
	// PamHelperManifestName is the file holding the sha256 of the helper
	// content, so the helper can detect its own drift at session time.
	PamHelperManifestName string
	// PamHelperToolPath is the PATH the helper sets for ITSELF before running
	// any command. pam_exec inherits the PAM environment (which a
	// PAM-environment-writing module could have extended), so the helper must
	// not resolve sha256sum/getent/id through an inherited PATH. Tests render
	// it with a stub directory first.
	PamHelperToolPath string
	// PamHelperModulePath is the pam_namespace.so path the helper requires.
	// It is filled by WithDefaults-adjacent detection (Ensure* sets it from
	// the module actually found on this host) so a host that installs the
	// module in a non-default location still gets a correct check.
	PamHelperModulePath string

	// TmpMountDropInDir / TmpMountDropInName hold the systemd drop-in that
	// caps the host /tmp tmpfs.
	TmpMountDropInDir  string
	TmpMountDropInName string
	// HostTmpMaxBytes is the host /tmp tmpfs cap installed by that drop-in.
	HostTmpMaxBytes uint64

	// Runner executes host commands. Nil means DefaultRunner.
	Runner Runner

	// DaemonBinary is the installed daemon binary the version-skew probe
	// (INT-DEMO-001) inspects before installing: `<DaemonBinary> --version`
	// must report the isolation-grant capability (GrantCapability), because a
	// daemon that does not grant isolation-group membership at spawn time
	// leaves the fail-closed PAM precondition denying every agent session.
	// The capability is the proof; MinDaemonVersion is a secondary floor for
	// daemons that report it. It defaults to the
	// same path internal/systemd uses for the unit ExecStart, so the probe
	// inspects the binary systemd actually runs. Apply gates on it; the
	// uninstall path never does.
	DaemonBinary string
	// DaemonVersionRunner executes the daemon version probe. Nil means
	// DefaultRunner. It is its own seam (not Runner) so tests can fake the
	// daemon's --version answer without pretending to be the whole host.
	DaemonVersionRunner DaemonVersionRunner

	// DaemonSkewAllowed records that the operator explicitly accepted the
	// daemon skew (the --allow-daemon-skew override): Apply must then PROCEED
	// past its internal CheckDaemonSkew gate, with the warning the caller
	// printed — exactly one warning must reach the operator, so Apply's own
	// internal call stays silent (io.Discard). Default false, so every OTHER
	// library caller keeps today's refusal on a skewed daemon.
	DaemonSkewAllowed bool
}

// Defaults for every field of Options. They are exported so docs, tests and
// the CLI all quote the same numbers.
const (
	DefaultScratchRoot = "/srv/bunker-share"
	// DefaultAgentGroup is the dedicated isolation group every agent joins.
	// It is deliberately NOT a distro or operator group ("bunker" was the
	// rejected default): the fail-closed precondition requires exactly this
	// group and the session user's membership of it, and the shared-scratch
	// tree keys on it, so ordinary operator accounts are never members.
	DefaultAgentGroup = "bunker-agents"
	// DefaultScratchGroup is the shared-scratch group. It IS the agent
	// isolation group: one group keeps membership, the PAM condition and the
	// setgid exchange semantics from ever drifting apart.
	DefaultScratchGroup    = DefaultAgentGroup
	DefaultScratchMaxBytes = uint64(256 << 20) // 256 MiB per agent
	DefaultTmpInstanceRoot = "/var/lib/bunkerd/agent-tmp"
	DefaultNamespaceConf   = "50-bunker-agents.conf"
	DefaultSSHDConfig      = "/etc/pam.d/sshd"
	DefaultTmpDropInName   = "50-bunker-size.conf"
	DefaultHostTmpMaxBytes = uint64(2 << 30) // 2 GiB host /tmp tmpfs

	// NamespaceConfDirBunker is the module's extra-config directory.
	NamespaceConfDirBunker = "/etc/security/namespace.d"
	// DefaultNamespaceConfFile is the distribution's namespace configuration,
	// which pam_namespace reads BEFORE the drop-in directory.
	DefaultNamespaceConfFile = "/etc/security/namespace.conf"
	// TmpMountDropInDirBunker is where the tmp.mount drop-in is installed.
	TmpMountDropInDirBunker = "/etc/systemd/system/tmp.mount.d"

	// NamespacePAMModuleLine is the sshd session line that performs the
	// polyinstantiation itself, and it is `required`: a session whose
	// namespace cannot be set up FAILS TO OPEN instead of quietly continuing
	// with the host's shared /tmp.
	//
	// `ignore_config_error` is deliberately NOT used. That option makes
	// pam_namespace SKIP a malformed namespace config line and carry on
	// (pam_namespace(8): "If a line in the configuration file corresponding to
	// a polyinstantiated directory contains format error, skip that line
	// process the next line. Without this option, pam will return an error to
	// the calling program resulting in termination of the session."). With it,
	// a dropped or malformed Bunker drop-in would silently degrade every agent
	// session back to the shared host /tmp — the exact class GAP-075 exists to
	// eliminate. Without it, a broken boundary is a loud, fail-closed session
	// failure.
	NamespacePAMModuleLine = "session    required     pam_namespace.so"

	// NamespacePAMAgentPattern is the reserved, generated agent username
	// pattern. Every Bunker agent is `bunker-<id>` (see
	// Options.TmpInstanceDir and manager_spawn.go's useradd call), so the
	// agent/non-agent question can be answered from the NAME ALONE — which is
	// what the classifier line does, and why a deleted agent group can no
	// longer make an agent session look like an ordinary operator session.
	NamespacePAMAgentPattern = "bunker-*"

	// namespaceMarker identifies Bunker-managed blocks in files we share
	// with the distribution (currently /etc/pam.d/sshd).
	namespaceMarker = "# bunker GAP-075: per-agent private /tmp (managed by `bunker host-provision`)"

	// DefaultPamHelperDir / DefaultPamHelperName / DefaultPamHelperManifestName
	// are the root-owned pam_exec precondition helper and its content manifest
	// (see NamespacePAMVerifyLine and PamGuardScript).
	DefaultPamHelperDir          = "/usr/lib/bunker"
	DefaultPamHelperName         = "pam-tmp-guard"
	DefaultPamHelperManifestName = "pam-tmp-guard.sha256"
	// PamHelperDirMode is the mode of the directory that holds the helper and
	// its manifest. The session-time helper checks its whole trust chain
	// (directory -> manifest -> helper), so this directory is the FIRST link
	// that closes the replace-by-rename bypass: a group/world-writable
	// /usr/lib/bunker would let an agent swap in its own helper AND manifest.
	// 0755 is an upper bound — a tighter mode is never widened.
	PamHelperDirMode os.FileMode = 0o755
	// DefaultPamHelperToolPath is the PATH the helper gives itself before it
	// runs anything: the boundary must not be decided by a command resolved
	// through an inherited (PAM) environment.
	DefaultPamHelperToolPath = "/usr/sbin:/usr/bin:/sbin:/bin"
)

// namespacePAMClassifierControl is the control field of the classifier line.
//
// Control semantics (pam.conf(5) actions + pam_succeed_if's return codes —
// evaluate_noglob() returns PAM_SUCCESS for a NON-matching user and
// PAM_AUTH_ERR for a matching user; pam_get_user() failure returns
// PAM_USER_UNKNOWN; an unknown attribute or an incomplete condition returns
// PAM_SERVICE_ERR, pam_succeed_if.c):
//
//	success=2      the name does NOT match the reserved agent pattern: jump
//	               over the next 2 modules — the verifier and pam_namespace —
//	               so an ordinary non-agent SSH user skips the ENTIRE managed
//	               block and keeps the host /tmp.
//	auth_err=ignore the name DOES match the agent pattern: continue to the
//	               verifier, which decides whether the boundary is intact.
//	default=die    any other answer (unknown user, allocation or module error,
//	               an incomplete condition) is a session we cannot classify:
//	               deny it immediately rather than let it reach a shared /tmp.
const namespacePAMClassifierControl = "[success=2 auth_err=ignore default=die]"

// namespacePAMVerifyControl is the control field of the verifier line.
//
// pam_exec returns PAM_SUCCESS only when the command exits 0 and
// PAM_SYSTEM_ERR for a non-zero exit, a signal, or a failed execve
// (call_exec() in modules/pam_exec/pam_exec.c), so:
//
//	success=ignore the helper proved the boundary is provisioned for this
//	               agent session: continue to pam_namespace.
//	default=die    anything else — the helper denied, the helper is missing,
//	               or it crashed — denies the session IMMEDIATELY. This is the
//	               fail-closed precondition: a deleted helper, a deleted or
//	               wrong drop-in, a malformed drop-in, a missing agent group
//	               or a missing membership all end the session instead of
//	               silently sharing /tmp.
const namespacePAMVerifyControl = "[success=ignore default=die]"

// NamespaceUserExclusion is the fourth namespace.conf field. It is DEFENSE IN
// DEPTH, not the scoping mechanism: the primary gate is the sshd PAM block,
// whose classifier and verifier both deny a non-agent session before the
// module is reached. Listing root here means that even if an operator adds root
// to the agent group, root still keeps the host's own /tmp. (The field is a
// comma-separated list of exact user names — pam_namespace resolves each name
// with getpwnam() and logs an unknown name away, so it cannot express a group
// or a pattern and must never be load-bearing.)
const NamespaceUserExclusion = "root"

// NamespaceMethod is the polyinstantiation method: one instance directory per
// user name. `noinit` suppresses the distro's /etc/security/namespace.init
// script so nothing outside Bunker can populate an instance directory.
const NamespaceMethod = "user:noinit"

// NamespacePAMClassifierLine renders the OUTER guard: the line that decides
// whether this session is an agent session, using the reserved agent username
// pattern and nothing else. It is deliberately independent of group state —
// pam_succeed_if's `user !~ <glob>` reads the PAM_USER item, so deleting the
// agent group cannot make an agent session look like an operator session (the
// failure mode the first revision shipped: `user ingroup <group>` answers
// PAM_AUTH_ERR for a missing group, exactly as it does for a non-member, and
// the old control field jumped those sessions over pam_namespace).
func NamespacePAMClassifierLine() string {
	return "session    " + namespacePAMClassifierControl + "    " +
		"pam_succeed_if.so quiet user !~ " + NamespacePAMAgentPattern
}

// NamespacePAMVerifyLine renders the agent-only fail-closed precondition: a
// root-owned pam_exec helper that proves the boundary is really provisioned
// (exact Bunker /tmp rule, agent group + membership, instance parent, helper
// integrity) before pam_namespace runs. It is invoked AFTER the classifier, so
// only agent sessions ever reach it.
//
// The helper's own path AND the agent group travel in the line, so the status
// report can show which group the deployed block keys on and drift between the
// daemon's configuration and the host is visible instead of silent.
func NamespacePAMVerifyLine(helperPath, agentGroup string) string {
	return "session    " + namespacePAMVerifyControl + "    pam_exec.so quiet " +
		helperPath + " verify " + agentGroup
}

// NamespacePAMBlock returns Bunker's managed sshd session block in order: the
// marker comment, the classifier, the fail-closed verifier, and the
// pam_namespace module line.
//
// The three module lines MUST stay adjacent: the classifier's control field
// jumps over exactly 2 modules for a non-agent session and the verifier must
// run immediately before pam_namespace, so the block is written, verified and
// repaired as one unit (see pamBlockIntact and ensureSSHDNamespaceBlock).
func NamespacePAMBlock(helperPath, agentGroup string) []string {
	return []string{
		namespaceMarker,
		NamespacePAMClassifierLine(),
		NamespacePAMVerifyLine(helperPath, agentGroup),
		NamespacePAMModuleLine,
	}
}

// DefaultOptions returns the production host layout with defaults applied.
func DefaultOptions() Options {
	o := Options{ScratchEnabled: true}
	return o.WithDefaults()
}

// WithDefaults fills every unset field with its documented default and
// applies Root to the resulting absolute paths. It never mutates the
// receiver's semantics for fields the caller set explicitly.
func (o Options) WithDefaults() Options {
	if o.ScratchRoot == "" {
		o.ScratchRoot = DefaultScratchRoot
	}
	if o.AgentGroup == "" {
		o.AgentGroup = DefaultAgentGroup
	}
	if o.ScratchPerAgent == 0 {
		o.ScratchPerAgent = DefaultScratchMaxBytes
	}
	if o.TmpInstanceRoot == "" {
		o.TmpInstanceRoot = DefaultTmpInstanceRoot
	}
	if o.NamespaceConfDir == "" {
		o.NamespaceConfDir = NamespaceConfDirBunker
	}
	if o.NamespaceConfName == "" {
		o.NamespaceConfName = DefaultNamespaceConf
	}
	if o.NamespaceConfFile == "" {
		o.NamespaceConfFile = DefaultNamespaceConfFile
	}
	if o.SSHDConfigPath == "" {
		o.SSHDConfigPath = DefaultSSHDConfig
	}
	if o.PamHelperDir == "" {
		o.PamHelperDir = DefaultPamHelperDir
	}
	if o.PamHelperName == "" {
		o.PamHelperName = DefaultPamHelperName
	}
	if o.PamHelperManifestName == "" {
		o.PamHelperManifestName = DefaultPamHelperManifestName
	}
	if o.PamHelperToolPath == "" {
		o.PamHelperToolPath = DefaultPamHelperToolPath
	}
	// The helper must name the exact pam_namespace.so it requires. When the
	// caller did not pin one, detect it the way the installer does; a host
	// without the module gets an empty value, which PamGuardScript renders as
	// a check that can never pass (the installer refuses before that point).
	if o.PamHelperModulePath == "" {
		o.PamHelperModulePath = detectModulePath(o, namespaceModulePaths)
	}
	if o.TmpMountDropInDir == "" {
		o.TmpMountDropInDir = TmpMountDropInDirBunker
	}
	if o.TmpMountDropInName == "" {
		o.TmpMountDropInName = DefaultTmpDropInName
	}
	if o.HostTmpMaxBytes == 0 {
		o.HostTmpMaxBytes = DefaultHostTmpMaxBytes
	}
	if o.DaemonBinary == "" {
		o.DaemonBinary = DefaultDaemonBinary
	}
	o.ScratchRoot = o.applyRoot(o.ScratchRoot)
	o.TmpInstanceRoot = o.applyRoot(o.TmpInstanceRoot)
	o.NamespaceConfDir = o.applyRoot(o.NamespaceConfDir)
	o.NamespaceConfFile = o.applyRoot(o.NamespaceConfFile)
	o.SSHDConfigPath = o.applyRoot(o.SSHDConfigPath)
	o.TmpMountDropInDir = o.applyRoot(o.TmpMountDropInDir)
	o.PamHelperDir = o.applyRoot(o.PamHelperDir)
	return o
}

// detectModulePath returns the first installed module path from candidates,
// honouring the Options.Root sandbox prefix ("" when none is installed).
func detectModulePath(o Options, candidates []string) string {
	if path, ok := findModule(o, candidates); ok {
		return path
	}
	return ""
}

// applyRoot redirects an absolute path into the sandbox tree named by Root.
// It is idempotent — a path already inside the sandbox is returned unchanged —
// because every provisioner calls WithDefaults, and WithDefaults is therefore
// called several times on options that have already been redirected.
func (o Options) applyRoot(path string) string {
	if o.Root == "" || path == "" || !filepath.IsAbs(path) {
		// A relative path is left alone so validation can reject it; silently
		// re-rooting it would hide an unsafe value.
		return path
	}
	if path == o.Root || strings.HasPrefix(path, o.Root+string(os.PathSeparator)) {
		return path
	}
	return filepath.Join(o.Root, path)
}

// runner returns the command runner to use.
func (o Options) runner() Runner {
	if o.Runner != nil {
		return o.Runner
	}
	return DefaultRunner
}

// run executes a host command through the configured runner.
func (o Options) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return o.runner()(ctx, name, args...)
}

// NamespaceConfPath is the pam_namespace drop-in file path.
func (o Options) NamespaceConfPath() string {
	return filepath.Join(o.NamespaceConfDir, o.NamespaceConfName)
}

// TmpMountDropInPath is the tmp.mount drop-in file path.
func (o Options) TmpMountDropInPath() string {
	return filepath.Join(o.TmpMountDropInDir, o.TmpMountDropInName)
}

// PamHelperPath is the pam_exec precondition helper path: the file the PAM
// block invokes, and the file the helper hashes to detect its own drift.
func (o Options) PamHelperPath() string {
	return filepath.Join(o.PamHelperDir, o.PamHelperName)
}

// PamHelperManifestPath is the file holding the expected sha256 of the helper
// content.
func (o Options) PamHelperManifestPath() string {
	return filepath.Join(o.PamHelperDir, o.PamHelperManifestName)
}

// NamespacePAMBlockForOptions renders the managed sshd block for these options.
func (o Options) NamespacePAMBlockForOptions() []string {
	return NamespacePAMBlock(o.PamHelperPath(), o.AgentGroup)
}

// ScratchDir is the per-agent scratch directory inside ScratchRoot.
func (o Options) ScratchDir(agentID string) string {
	return filepath.Join(o.ScratchRoot, agentID)
}

// TmpInstanceDir is the pam_namespace /tmp instance directory for an agent.
func (o Options) TmpInstanceDir(agentID string) string {
	return filepath.Join(o.TmpInstanceRoot, "bunker-"+agentID)
}

// Change is one provisioning action, or a deliberate no-op, reported to the
// operator. Dry runs return the same list with Applied=false.
type Change struct {
	Action  string // create|write|remove|mount|remount|chmod|chown|skip|ok
	Target  string
	Detail  string
	Applied bool
}

// Report is the ordered set of changes a provisioner considered or made.
type Report struct {
	Changes []Change
}

// Add appends a change and returns it for further annotation.
func (r *Report) Add(action, target, detail string, applied bool) *Change {
	r.Changes = append(r.Changes, Change{Action: action, Target: target, Detail: detail, Applied: applied})
	return &r.Changes[len(r.Changes)-1]
}

// Mutations returns the changes that actually modified host state. Pure
// verifications ("ok") and deliberate no-ops ("skip") are excluded, which is
// what makes idempotency assertable: a second run must have none.
func (r *Report) Mutations() []Change {
	var out []Change
	for _, c := range r.Changes {
		if !c.Applied || c.Action == "ok" || c.Action == "skip" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Has reports whether the report contains any change with the given action.
func (r *Report) Has(action string) bool {
	for _, c := range r.Changes {
		if c.Action == action {
			return true
		}
	}
	return false
}

// Applied reports how many changes were actually made.
func (r *Report) Applied() int {
	n := 0
	for _, c := range r.Changes {
		if c.Applied {
			n++
		}
	}
	return n
}

// String renders the report one change per line, prefixed with the applied
// state so a dry run and an apply are visually distinguishable.
func (r *Report) String() string {
	if len(r.Changes) == 0 {
		return "no changes"
	}
	var b strings.Builder
	for _, c := range r.Changes {
		state := "plan"
		if c.Applied {
			state = "done"
		}
		fmt.Fprintf(&b, "%-4s %-7s %s", state, c.Action, c.Target)
		if c.Detail != "" {
			fmt.Fprintf(&b, " — %s", c.Detail)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// isExitError reports whether err came from a command that ran and exited
// non-zero (as opposed to a missing binary or a context cancellation). Probes
// like `mountpoint -q` use a non-zero exit as a boolean answer, so callers
// must not treat every error as a hard failure.
func isExitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

// writeFileIdempotent writes content to path with mode only when the on-disk
// bytes differ. It reports whether it wrote. Parent directories are created.
func writeFileIdempotent(path string, content []byte, mode os.FileMode) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(content) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create dir for %s: %w", path, err)
	}
	// Write through a temp file in the same directory so a crash can never
	// leave a half-written host config behind (a torn /etc/pam.d/sshd would
	// break every login on the host).
	tmp := path + ".bunker-tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return false, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return true, nil
}
