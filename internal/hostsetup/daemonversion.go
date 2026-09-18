// Daemon version-skew probe (INT-DEMO-001).
//
// The GAP-075 host hardening is only safe when the installed daemon grants
// isolation-group membership at spawn time. This file probes the INSTALLED
// daemon binary and lets the installer refuse (or the operator override) when
// the daemon predates the spawn-side grant, so the host-side/daemon version
// skew that locked out every agent session on the live demo host (2026-09-16)
// becomes impossible to install silently.
package hostsetup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultDaemonBinary is the installed daemon binary the version-skew probe
// inspects. It is the SAME default internal/systemd uses for the unit's
// ExecStart, so the binary probed here is the binary systemd actually runs.
const DefaultDaemonBinary = "/usr/local/bin/bunkerd"

// MinDaemonVersion is the minimum daemon VERSION a host-side provision may be
// installed alongside. It is a SECONDARY check — the authoritative signal is
// the CAPABILITY the daemon reports (GrantCapability), because a version number
// cannot prove that a build carries the grant.
//
// The host half of GAP-075 fails CLOSED: the agent-scoped sshd PAM block
// denies every `bunker-*` session unless the logging-in agent is a member of
// the isolation group (the pam_exec verifier), and that membership is granted
// at SPAWN time by the daemon's isolation-provision stage
// (internal/agent, StageIsolationProvision). A daemon older than the stage
// predates the grant: every agent it spawns stays OUT of the group, so after
// host-provision --apply every SSH session into a healthy, running agent is
// denied (pam_open_session: System error → bare ssh exit status 254) while
// bunker exec, mount and cp break and the agent is still reported running.
// The live demo host hit exactly this on 2026-09-16: the host-side block was
// installed on 2026-09-13 on top of a 2026-08-30 daemon build.
//
// NO RELEASED TAG CARRIES THE GRANT, so this floor is not a capability
// guarantee and never was: the newest tag v0.1.4 (2026-09-12) predates
// internal/hostsetup entirely (`git ls-tree --name-only v0.1.4 internal/`
// lists no hostsetup; the package was added in 207e0e5 on 2026-09-16) and no
// tag contains that commit (`git tag --contains 207e0e5` is empty). A v0.1.4
// binary therefore self-reports Version "0.1.4" while carrying no grant — a
// version comparison alone answers OK for exactly the build that must be
// refused. The probe therefore requires the reported capability FIRST (see
// GrantCapability) and keeps this version floor only as a secondary check;
// TestGrantFloorMatchesReality fails if GrantMinTag and the tag list disagree
// in either direction.
//
// WHY Version (and not Commit, not Built): Commit is a short SHA whose
// ordering means nothing, and Built timestamps the BUILD machine, not the
// code. Version is injected by the Makefile LDFLAGS on every release build
// and falls back to the module version for `go install` builds
// (internal/version's deriveFromBuildInfo), so it is the one field that
// carries a comparable minimum for BOTH ldflags-built and go-install
// binaries. A binary reporting "unknown" (bare `go build`, no ldflags, no VCS
// metadata) cannot prove it carries the grant and is treated as a probe
// failure (UNKNOWN → warn, never refuse — see ProbeDaemonVersion).
const MinDaemonVersion = "0.1.4"

// GrantCapability is the capability token the daemon-skew probe REQUIRES in
// `bunkerd --version` output before a host-side provision may proceed. It
// names the spawn-side isolation grant (internal/agent's provisionIsolation /
// StageIsolationProvision), which is the only thing that keeps the fail-closed
// sshd PAM precondition from denying every agent session.
//
// Kept as a literal because internal/agent imports internal/hostsetup
// (internal/agent/isolation.go), so importing it back would be an import
// cycle; equality with agent.IsolationGrantCapability is pinned by a test in
// internal/agent.
const GrantCapability = "isolation-grant"

// GrantMinTag is the first RELEASE TAG whose tree carries the spawn-side grant
// (the token above). It is EMPTY because no released tag carries it today: the
// newest tag v0.1.4 (2026-09-12) predates internal/hostsetup (added in
// 207e0e5, 2026-09-16), which README.md states in the same words ("the
// `v0.1.4` tag predates the isolation implementation").
//
// Set it to the tag name when such a tag is cut. TestGrantFloorMatchesReality
// fails while this constant and the tag list disagree in EITHER direction: with
// the constant empty no visible tag may contain the token, and with a name set
// that tag must exist and carry it.
const GrantMinTag = ""

// DaemonProbeTimeout bounds ONE --version probe of the installed daemon. The
// probe must never hang the installer: a wedged binary (or a hung filesystem)
// degrades to a warning, not to a dead host-provision run.
const DaemonProbeTimeout = 5 * time.Second

// DaemonVersionRunner runs the daemon version probe. It is a seam parallel to
// Runner: production uses DefaultRunner (exec.CommandContext); unit tests
// inject a fake so no real binary is ever exec'd. Nil means DefaultRunner.
type DaemonVersionRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// DaemonProbeState is the outcome of the version-skew decision for the
// installed daemon.
type DaemonProbeState string

const (
	// DaemonSkewOK: the installed daemon reports the required capability
	// (GrantCapability) AND is equal or newer than MinDaemonVersion — no
	// refusal, no warning.
	DaemonSkewOK DaemonProbeState = "OK"
	// DaemonSkewSkewed: the installed daemon cannot prove the spawn-side
	// grant — it does not report GrantCapability, or it reports it but
	// predates MinDaemonVersion. The installer refuses (unless the operator
	// overrides) because spawning against this daemon would deny every agent
	// SSH session. An ABSENT capability is SKEWED at ANY version: that is
	// exactly the v0.1.4 shape (it reports 0.1.4 and carries no grant).
	DaemonSkewSkewed DaemonProbeState = "SKEWED"
	// DaemonSkewUnknown: the probe could not rule the skew out (binary
	// absent, not executable, timed out, or unparseable output). A host may
	// legitimately be provisioned before the daemon is installed, so this is
	// a LOUD WARNING, never a refusal.
	DaemonSkewUnknown DaemonProbeState = "UNKNOWN"
)

// DaemonBuild is what the probe parsed from the installed daemon's --version
// output. Zero fields mean the probe could not read them.
type DaemonBuild struct {
	Binary       string   // the probed path
	Version      string   // e.g. "0.1.4" (a leading "v" is stripped)
	Commit       string   // short SHA, or the module version fallback
	Built        string   // build timestamp as the binary reports it
	Capabilities []string // tokens from the `caps:` line; nil when absent/empty
}

// HasCapability reports whether the build reported name in its `caps:` line.
// Matching is case-insensitive and trims surrounding space on both sides, so a
// build that prints "Caps: Isolation-Grant" is not silently rejected. An empty
// name never matches.
func (b DaemonBuild) HasCapability(name string) bool {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return false
	}
	for _, have := range b.Capabilities {
		if strings.ToLower(strings.TrimSpace(have)) == want {
			return true
		}
	}
	return false
}

// SkewVersion renders the parsed revision for operator-facing messages.
func (b DaemonBuild) SkewVersion() string {
	ver, commit, built := b.Version, b.Commit, b.Built
	if ver == "" {
		ver = "unknown"
	}
	if commit == "" {
		commit = "unknown"
	}
	if built == "" {
		built = "unknown"
	}
	return fmt.Sprintf("version %s, commit %s, built %s", ver, commit, built)
}

// ParseDaemonVersionOutput parses `bunkerd --version` output: the word
// "bunkerd" followed by the version on the same or the next token, then
// indented "commit:" / "built:" / "caps:" lines (see cmd/bunkerd/main.go). It
// is lenient about indentation and a leading "v" on the version, and strict
// about needing at least a parseable version.
//
// The caps line carries this build's capability tokens, comma- and/or
// space-separated; a `caps:` line with no value means "no capabilities" (NOT
// an error — a build that reports none is simply refused by the capability
// check). Unknown lines are still ignored.
func ParseDaemonVersionOutput(out []byte) (DaemonBuild, error) {
	var b DaemonBuild
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		case b.Version == "" && strings.HasPrefix(line, "bunkerd"):
			rest := strings.TrimSpace(strings.TrimPrefix(line, "bunkerd"))
			if rest == "" {
				continue
			}
			b.Version = strings.TrimPrefix(rest, "v")
		case strings.HasPrefix(line, "commit:"):
			b.Commit = strings.TrimSpace(strings.TrimPrefix(line, "commit:"))
		case strings.HasPrefix(line, "built:"):
			b.Built = strings.TrimSpace(strings.TrimPrefix(line, "built:"))
		case strings.HasPrefix(line, "caps:"):
			b.Capabilities = parseCapabilities(strings.TrimPrefix(line, "caps:"))
		}
	}
	if b.Version == "" || b.Version == "unknown" {
		return b, fmt.Errorf("no version in --version output (%d bytes)", len(out))
	}
	return b, nil
}

// parseCapabilities splits a `caps:` value on commas and whitespace and drops
// empty entries, so "a,b", "a, b" and "a b" all yield [a b] and an empty value
// yields nil. It is case-preserving: HasCapability does the case-insensitive
// comparison.
func parseCapabilities(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// ProbeDaemonVersion runs `<DaemonBinary> --version` and parses the result.
// It NEVER fails: the (build, state, error) triple reports what it observed,
// and error is a diagnostic for DaemonSkewUnknown only. The probe is bounded
// by DaemonProbeTimeout so a wedged daemon binary cannot hang the installer.
func (o Options) ProbeDaemonVersion(ctx context.Context) (DaemonBuild, DaemonProbeState, error) {
	o = o.WithDefaults()
	binary := o.DaemonBinary

	runner := o.DaemonVersionRunner
	if runner == nil {
		runner = DefaultRunner
	}

	pctx, cancel := context.WithTimeout(ctx, DaemonProbeTimeout)
	defer cancel()

	out, err := runner(pctx, binary, "--version")
	if b, perr := ParseDaemonVersionOutput(out); perr == nil {
		// The binary printed a parseable version; an exit error next to it
		// does not change the answer. The SKEW decision is made HERE, against
		// the parsed build (capability first, version floor second) — a
		// parseable-but-capless daemon is exactly the incident class this
		// probe exists for.
		b.Binary = binary
		return b, daemonSkewState(b), nil
	}
	if err == nil {
		return DaemonBuild{Binary: binary}, DaemonSkewUnknown,
			fmt.Errorf("%s --version printed unparseable output: %s", binary, probeSnippet(out))
	}
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return DaemonBuild{Binary: binary}, DaemonSkewUnknown,
			fmt.Errorf("daemon binary %s not found (not installed yet?): %v", binary, probeErrText(err))
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return DaemonBuild{Binary: binary}, DaemonSkewUnknown,
			fmt.Errorf("daemon binary %s did not answer --version within %s: %v", binary, DaemonProbeTimeout, probeErrText(err))
	default:
		return DaemonBuild{Binary: binary}, DaemonSkewUnknown,
			fmt.Errorf("daemon binary %s could not be probed: %v (output: %s)", binary, probeErrText(err), probeSnippet(out))
	}
}

// DaemonSkew returns a non-nil error when build cannot prove the spawn-side
// grant. The error IS the operator-facing refusal: one actionable, multi-line
// message naming the installed revision, the REQUIRED capability, the concrete
// failure mode in the operator's terms, and both remediations. The version
// floor is mentioned only as the secondary check it now is.
func DaemonSkew(build DaemonBuild) error {
	if daemonSkewState(build) == DaemonSkewOK {
		return nil
	}
	return fmt.Errorf(`refusing to provision: the installed daemon %s (%s) does not report the spawn-side isolation capability %q this installer requires (reported capabilities: %s).

%s

A daemon that does not report %q at spawn time does not add the agents it spawns to the isolation group, so the fail-closed sshd PAM precondition (pam_exec verify <group>) denies every bunker-* session: bunker exec, mount and cp all fail against healthy, RUNNING agents with a bare exit status 254 (pam_open_session: System error) while the agent is still reported running.

Remediation (either one):
  1. upgrade the daemon to a build that reports %q in "bunkerd --version" and re-run this command (the %s floor is a secondary version check only — it never proves the capability); or
  2. run "bunker host-provision --uninstall --apply" to remove the host-side hardening and return the host to a shared /tmp.
Never hand-delete only the PAM drop-in: the remaining pam_exec precondition fails closed and would lock out every agent.
(--allow-daemon-skew proceeds anyway.)`,
		build.Binary, build.SkewVersion(), GrantCapability, build.capabilityList(), versionFloorNote(build), GrantCapability, GrantCapability, MinDaemonVersion)
}

// capabilityList renders the reported capability tokens for operator-facing
// messages, or "none reported" when the daemon advertises nothing.
func (b DaemonBuild) capabilityList() string {
	if len(b.Capabilities) == 0 {
		return "none reported"
	}
	return strings.Join(b.Capabilities, ",")
}

// versionFloorNote is the SECONDARY half of the refusal diagnosis: it names how
// the installed version stands against the floor, separating "no capability"
// from "capability but an ancient version" in the operator's message.
func versionFloorNote(build DaemonBuild) string {
	if versionAtLeast(build.Version, MinDaemonVersion) {
		return fmt.Sprintf("Its version %s is at or above the %s floor, so the version comparison alone would have accepted it — the missing capability is the reason for this refusal.", build.Version, MinDaemonVersion)
	}
	return fmt.Sprintf("Its version is also below the %s floor.", MinDaemonVersion)
}

// DaemonSkewHint is the one-line WARNING the installer prints when the skew
// could not be ruled out (probe failure) or the operator overrode a skew.
func DaemonSkewHint(state DaemonProbeState, build DaemonBuild, probeErr error) string {
	min := MinDaemonVersion
	consequence := fmt.Sprintf("a daemon that does not report the %s capability does not add spawned agents to the isolation group, so every bunker-* SSH session is denied (exec/mount/cp fail with a bare exit 254)", GrantCapability)
	switch state {
	case DaemonSkewUnknown:
		return fmt.Sprintf("WARNING: daemon version skew could NOT be ruled out: %v. If the installed daemon does not report %s (the %s version floor is a secondary check only), %s — upgrade the daemon before relying on agent sessions", probeErr, GrantCapability, min, consequence)
	case DaemonSkewSkewed:
		return fmt.Sprintf("WARNING: proceeding with --allow-daemon-skew: the installed daemon %s (%s) does not prove the %s capability (%s); %s until the daemon is upgraded", build.Binary, build.SkewVersion(), GrantCapability, build.capabilitySuffix(), consequence)
	default:
		return ""
	}
}

// capabilitySuffix renders "reported capabilities: ..." for the override
// warning, so the operator sees what the daemon actually advertised.
func (b DaemonBuild) capabilitySuffix() string {
	return "reported capabilities: " + b.capabilityList()
}

// CheckDaemonSkew is the installer-side decision (INT-DEMO-001): probe the
// installed daemon, REFUSE when the build cannot prove the spawn-side grant —
// it does not report GrantCapability at any version, or reports it below
// MinDaemonVersion — unless allow is set, which proceeds with a loud warning.
// WARN without refusing on a probe failure, and stay silent when the daemon
// reports the capability at or above the floor. The uninstall path must never be
// gated on this check: returning the host to a shared /tmp must always remain
// possible.
func (o Options) CheckDaemonSkew(ctx context.Context, allow bool, warn io.Writer) error {
	build, state, perr := o.ProbeDaemonVersion(ctx)
	switch state {
	case DaemonSkewOK:
		return nil
	case DaemonSkewSkewed:
		if allow {
			fmt.Fprintln(warn, DaemonSkewHint(DaemonSkewSkewed, build, perr))
			return nil
		}
		return DaemonSkew(build)
	default:
		fmt.Fprintln(warn, DaemonSkewHint(DaemonSkewUnknown, build, perr))
		return nil
	}
}

// daemonSkewState classifies a parsed build for Status/CheckDaemonSkew. The
// reported CAPABILITY is authoritative and the version floor is secondary:
//
//	capability present AND version >= MinDaemonVersion -> OK
//	capability present AND version <  MinDaemonVersion -> SKEWED (floor applies)
//	capability ABSENT  at any version                  -> SKEWED
//
// The last rule is the point of the capability at all: a bare `go build` of any
// revision reports the package default version (internal/version.Version), which
// is exactly how a v0.1.4 tag binary claims 0.1.4 while carrying no grant — the
// token, not the number, is the proof.
func daemonSkewState(build DaemonBuild) DaemonProbeState {
	if !build.HasCapability(GrantCapability) {
		return DaemonSkewSkewed
	}
	if !versionAtLeast(build.Version, MinDaemonVersion) {
		return DaemonSkewSkewed
	}
	return DaemonSkewOK
}

// versionAtLeast reports whether semver-ish string have >= min. It compares
// dot-separated numeric components (a leading "v" is ignored); a
// non-numeric or empty have is "not comparable" and returns false, which is
// the fail-safe answer for a bare `go build` binary with version "unknown".
func versionAtLeast(have, min string) bool {
	have, min = strings.TrimPrefix(have, "v"), strings.TrimPrefix(min, "v")
	hp, mp := strings.Split(have, "."), strings.Split(min, ".")
	for i := 0; i < len(hp) || i < len(mp); i++ {
		h, m := 0, 0
		var err error
		if i < len(hp) {
			if h, err = strconv.Atoi(hp[i]); err != nil {
				return false
			}
		}
		if i < len(mp) {
			if m, err = strconv.Atoi(mp[i]); err != nil {
				return false
			}
		}
		if h != m {
			return h > m
		}
	}
	return true
}

// probeSnippet renders probe output for a warning line (first line, capped).
func probeSnippet(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

// probeErrText renders a probe error without a multi-line wrapper.
func probeErrText(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
