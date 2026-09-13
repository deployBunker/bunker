package hostsetup

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Status is the complete observed state of the GAP-075 host provisioning.
type Status struct {
	AgentGroupPresent  bool
	ScratchRootPresent bool
	ScratchRootOwner   string // "user:group mode" as reported by stat
	// ScratchRootModeOK is true when the exchange root is present, setgid and
	// NOT writable by the agent group or the world — the property that stops an
	// agent from creating an uncapped entry beside its own capped directory. It
	// is reported next to (not inside) the isolation verdict: a wide scratch
	// root does not deny sessions, it silently widens the exchange tree, so it
	// must be visible on its own line.
	ScratchRootModeOK bool
	TmpNamespace      TmpNamespaceState
	HostTmp           HostTmpState
}

// Isolated answers the one question the isolation boundary exists for: does
// every agent session on this host get its own /tmp? It is true only when the
// static boundary the RUNTIME pam_exec precondition enforces is fully in place:
// the three PAM modules are present (and the classifier can express the
// agent-name pattern), Bunker's drop-in exists with exactly the required /tmp
// rule and the whole namespace configuration set is parseable, the helper TRUST
// CHAIN (helper directory -> sha256 manifest -> root-owned helper) is intact —
// present, root-owned and not group/world writable, with a matching hash — the
// agent-scoped session block is intact in the sshd stack and names the
// configured group, the agent group exists, and the instance parent exists,
// root-owned and mode 0000. Anything that would make the helper deny a
// `bunker-*` session (or let an agent rewrite the boundary) makes this false:
// "isolated" must never be reported for a host that denies or shares.
func (s Status) Isolated() bool { return s.TmpNamespace.Active }

// String renders the operator-facing status report.
func (s Status) String() string {
	var b strings.Builder
	ns := s.TmpNamespace
	fmt.Fprintf(&b, "per-session private /tmp (pam_namespace, agent-scoped)\n")
	fmt.Fprintf(&b, "  module present:        %v%s\n", ns.ModulePresent, optPath(ns.ModulePath))
	fmt.Fprintf(&b, "  classifier module:     %v%s\n", ns.GuardModulePresent, optPath(ns.GuardModulePath))
	fmt.Fprintf(&b, "  classifier pattern ok: %v%s\n", ns.GuardModuleSupportsPattern, missingTokensDetail(ns))
	fmt.Fprintf(&b, "  precondition module:   %v%s\n", ns.ExecModulePresent, optPath(ns.ExecModulePath))
	fmt.Fprintf(&b, "  agent name pattern:    %s\n", NamespacePAMAgentPattern)
	fmt.Fprintf(&b, "  agent group:           %s (present: %v)\n", ns.AgentGroup, ns.AgentGroupPresent)
	fmt.Fprintf(&b, "  deployed block group:  %s\n", guardGroupOrNothing(ns.DeployedVerifyGroup))
	fmt.Fprintf(&b, "  namespace drop-in:     %v (%s)\n", ns.ConfPresent, ns.ConfPath)
	fmt.Fprintf(&b, "  drop-in rule ok:       %v%s\n", ns.ConfRuleOK, ruleDetail(ns))
	fmt.Fprintf(&b, "  drop-in owner/mode:    %s %04o\n", ownerOrNothing(ns.ConfOwner), ns.ConfMode)
	fmt.Fprintf(&b, "  precondition helper:   %v%s\n", ns.HelperPresent, optPath(ns.HelperPath))
	fmt.Fprintf(&b, "  helper owner/mode:     %s %04o\n", ownerOrNothing(ns.HelperOwner), ns.HelperMode)
	fmt.Fprintf(&b, "  helper manifest:       %v (content matches: %v)\n", ns.HelperManifestPresent, ns.HelperIntegrityOK)
	fmt.Fprintf(&b, "  manifest owner/mode:   %s %04o\n", ownerOrNothing(ns.HelperManifestOwner), ns.HelperManifestMode)
	fmt.Fprintf(&b, "  helper directory:      %v%s\n", ns.HelperDirPresent, optPath(ns.HelperDir))
	fmt.Fprintf(&b, "  helper dir owner/mode: %s %04o\n", ownerOrNothing(ns.HelperDirOwner), ns.HelperDirMode)
	fmt.Fprintf(&b, "  sshd PAM block:        %v (%s)\n", ns.PAMBlockPresent, ns.SSHDConfigPath)
	fmt.Fprintf(&b, "  instance parent:       %s (%s)\n", instanceRootState(ns), ns.InstanceRoot)
	fmt.Fprintf(&b, "  instance parent owner: %s\n", ownerOrNothing(ns.InstanceRootOwner))
	fmt.Fprintf(&b, "  ownership verified:    %v (root-only observation)\n", ns.OwnershipVerifiable)
	fmt.Fprintf(&b, "  ACTIVE:                %v\n", s.Isolated())
	fmt.Fprintf(&b, "shared scratch (cross-agent exchange)\n")
	fmt.Fprintf(&b, "  group present:         %v\n", s.AgentGroupPresent)
	fmt.Fprintf(&b, "  root present:          %v %s\n", s.ScratchRootPresent, s.ScratchRootOwner)
	fmt.Fprintf(&b, "  root mode ok:          %v (setgid, no group/world write; want %04o)\n", s.ScratchRootModeOK, ScratchRootMode)
	fmt.Fprintf(&b, "host /tmp tmpfs cap\n")
	fmt.Fprintf(&b, "  /tmp filesystem:       %s\n", fstypeOrNothing(s.HostTmp.FSType))
	fmt.Fprintf(&b, "  live size=:            %d\n", s.HostTmp.SizeBytes)
	fmt.Fprintf(&b, "  drop-in present:       %v (%s)\n", s.HostTmp.DropInPresent, s.HostTmp.DropInPath)
	fmt.Fprintf(&b, "  drop-in size=:         %d\n", s.HostTmp.DropInCappedBytes)
	return b.String()
}

// missingTokensDetail names the tokens a classifier module lacks, so an
// operator can see WHY the host is not active (a stale Linux-PAM cannot express
// the agent-name pattern and the block would deny every session).
func missingTokensDetail(ns TmpNamespaceState) string {
	if ns.GuardModuleSupportsPattern || len(ns.GuardModuleMissingTokens) == 0 {
		return ""
	}
	return " (missing " + strings.Join(ns.GuardModuleMissingTokens, ", ") + " — needs Linux-PAM >= 1.6)"
}

// ruleDetail renders the reason the drop-in rule check failed, so an operator
// sees WHICH part of the namespace configuration the runtime helper would
// reject instead of only "false".
func ruleDetail(ns TmpNamespaceState) string {
	if ns.ConfRuleOK || ns.ConfRuleDetail == "" {
		return ""
	}
	return " (" + ns.ConfRuleDetail + ")"
}

// ownerOrNothing renders a "uid:gid" pair, or a placeholder when it could not
// be read.
func ownerOrNothing(owner string) string {
	if owner == "" {
		return "unknown"
	}
	return owner
}

// instanceRootState renders the instance parent the way an operator needs to
// read it: absent, or its mode (which pam_namespace requires to be 0000).
func instanceRootState(ns TmpNamespaceState) string {
	if !ns.InstanceRootPresent {
		return "absent"
	}
	return fmt.Sprintf("mode %04o", ns.InstanceRootMode)
}

// guardGroupOrNothing renders the deployed verifier's group, naming that no
// Bunker block is present when it is empty.
func guardGroupOrNothing(group string) string {
	if group == "" {
		return "none (no Bunker verifier line in the sshd stack)"
	}
	return group
}

func optPath(p string) string {
	if p == "" {
		return ""
	}
	return " — " + p
}

// Status observes every piece of the provisioning without changing anything.
func (o Options) Status(ctx context.Context) (Status, error) {
	o = o.WithDefaults()
	st := Status{}

	if _, err := o.run(ctx, "getent", "group", o.AgentGroup); err == nil {
		st.AgentGroupPresent = true
	}
	if _, err := os.Stat(o.ScratchRoot); err == nil {
		st.ScratchRootPresent = true
	}
	if fi, err := os.Stat(o.ScratchRoot); err == nil {
		st.ScratchRootModeOK = modeNotGroupOrWorldWritable(fi.Mode()) && fi.Mode()&os.ModeSetgid != 0
	}
	if out, err := o.run(ctx, "stat", "-c", "%U:%G %a", o.ScratchRoot); err == nil {
		st.ScratchRootOwner = strings.TrimSpace(string(out))
	} else {
		st.ScratchRootOwner = "absent"
	}

	ns, err := o.TmpNamespaceStatus()
	if err != nil {
		return st, err
	}
	st.TmpNamespace = ns

	host, err := o.HostTmpStatus(ctx)
	if err != nil {
		return st, err
	}
	st.HostTmp = host
	return st, nil
}

// Apply provisions the whole boundary: shared scratch, the per-session private
// /tmp, and the host /tmp cap. With apply=false it makes no changes and
// reports the plan. The three steps are independent — an operator may run only
// EnsureTmpNamespace on a host with no agents — so a failure in any step is
// returned with the report gathered so far.
func (o Options) Apply(ctx context.Context, apply bool) (*Report, error) {
	o = o.WithDefaults()
	combined := &Report{}

	if apply {
		scratch, err := o.EnsureSharedScratch(ctx)
		combined.Changes = append(combined.Changes, scratch.Changes...)
		if err != nil {
			return combined, err
		}
	} else {
		// The dry run must still say what the scratch step WOULD do, so an
		// operator can see the whole plan before applying it.
		groupExists := false
		if _, err := o.run(ctx, "getent", "group", o.AgentGroup); err == nil {
			groupExists = true
		}
		if groupExists {
			combined.Add("ok", "group:"+o.AgentGroup, "agent group already exists", false)
		} else {
			combined.Add("create", "group:"+o.AgentGroup, "create system group", false)
		}
		if _, err := os.Stat(o.ScratchRoot); err == nil {
			combined.Add("ok", o.ScratchRoot, "scratch root already exists", false)
		} else {
			combined.Add("create", o.ScratchRoot, fmt.Sprintf("create root-owned setgid directory (mode %04o)", ScratchRootMode), false)
		}
	}

	ns, err := o.ensureTmpNamespaceApplied(ctx, apply)
	combined.Changes = append(combined.Changes, ns.Changes...)
	if err != nil {
		return combined, err
	}

	if o.SkipHostTmpCap {
		combined.Add("skip", o.TmpMountDropInPath(), "host /tmp cap disabled for this run", false)
		return combined, nil
	}
	tmp, err := o.EnsureHostTmpCap(ctx, apply)
	combined.Changes = append(combined.Changes, tmp.Changes...)
	if err != nil {
		return combined, err
	}
	return combined, nil
}
