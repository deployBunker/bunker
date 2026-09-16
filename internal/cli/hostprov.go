package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/hostsetup"
)

// NewHostProvisionCommand returns the `bunker host-provision` command: the
// idempotent installer for the host half of the GAP-075 isolation boundary.
//
// It runs on the HOST (as root), not against a daemon:
//
//   - the agent isolation group (default bunker-agents) + /srv/bunker-share,
//     the cross-agent exchange root (group/setgid)
//   - pam_namespace instance parent   per-session private /tmp (default root)
//   - the agent-scoped sshd PAM session block + Bunker's namespace.d drop-in
//   - a tmp.mount drop-in capping the host /tmp tmpfs
//
// Nothing is changed unless --apply is given, and /etc/fstab is never touched.
func NewHostProvisionCommand() *cobra.Command {
	var (
		apply       bool
		showStatus  bool
		uninstall   bool
		skipTmpCap  bool
		hostTmpMax  int64
		scratchMax  int64
		scratchRoot string
		agentGroup  string
		tmpInstRoot string
		asJSON      bool

		daemonBinary    string
		allowDaemonSkew bool
	)

	cmd := &cobra.Command{
		Use:   "host-provision",
		Short: "Provision the per-agent isolation boundary on this host (GAP-075)",
		Long: `Provision the host half of the agent isolation boundary.

Every AGENT gets an enforced private /tmp:

  * SSH sessions (bunker exec, scp, sshfs, the docker transport) get a private
    per-session /tmp from pam_namespace. Bunker's sshd session block is
    agent-scoped by NAME, not by group state, and fails closed:

        session [success=2 auth_err=ignore default=die] pam_succeed_if.so quiet user !~ bunker-*
        session [success=ignore default=die]            pam_exec.so quiet <helper> verify <group>
        session required                                pam_namespace.so

    The classifier keys on the reserved 'bunker-*' username pattern, so a
    deleted agent group cannot turn an agent session into an ordinary one. That
    pattern test needs Linux-PAM >= 1.6; the installer probes the module for it
    before writing the block and refuses if it is missing (an unsupported
    pattern under default=die would deny every session on the host). An
    ordinary non-agent SSH user (and root) is jumped over all three modules and
    keeps the host /tmp. For an agent session, the root-owned pam_exec helper
    proves — before pam_namespace runs — that the exact Bunker /tmp rule is in
    place, that the session user is in the agent group, that the instance
    parent is correct and that its whole trust chain (root-owned, non-writable
    helper directory -> root-owned sha256 manifest -> the helper's own bytes)
    is intact; any failure, including a missing helper, a missing/valid-but-
    wrong/malformed drop-in, a missing group, a lost membership, or a helper
    directory an agent could write, DENIES the session instead of silently
    degrading to a shared /tmp;
  * transient systemd units (rootless dockerd, detached runs) carry
    PrivateTmp=yes, which is applied by the daemon at spawn/run time.

The agent group holds every agent, independently of the shared-scratch
setting: it is the identity the helper verifies, so membership is provisioned
even when the exchange directory is disabled.

Cross-agent exchange is strictly opt-in through a single bounded directory:
/srv/bunker-share, root-owned with the agent group, setgid and NO group or
world write bit (mode 2750), where each agent's subdirectory is a size-capped
tmpfs. The root is re-asserted and stat-ed back on every apply: a
group-writable exchange root would let an agent create a plain, uncapped
directory or file beside its own capped one, which is the whole point of the
cap. An agent whose scratch cannot be mounted at its cap gets NO scratch
directory (never an unbounded one).

The host's own /tmp tmpfs is capped with a systemd drop-in for tmp.mount and a
non-destructive remount. /etc/fstab is never read or rewritten.

Without --apply the command only prints the plan.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			opts := hostsetup.DefaultOptions()
			opts.ScratchRoot = scratchRoot
			opts.AgentGroup = agentGroup
			opts.TmpInstanceRoot = tmpInstRoot
			if scratchMax > 0 {
				opts.ScratchPerAgent = uint64(scratchMax)
			}
			if hostTmpMax > 0 {
				opts.HostTmpMaxBytes = uint64(hostTmpMax)
			}
			opts.SkipHostTmpCap = skipTmpCap
			opts.DaemonBinary = daemonBinary
			opts = opts.WithDefaults()

			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			// INT-DEMO-001: --status reports the skew reading but never
			// gates on it. The override flag is meaningless there.
			if showStatus {
				st, err := opts.Status(ctx)
				if err != nil {
					return fmt.Errorf("probe host: %w", err)
				}
				if asJSON {
					return writeJSON(out, opts, st)
				}
				fmt.Fprintln(out, st.String())
				if !st.Isolated() {
					fmt.Fprintln(out, "isolation is NOT active: run `bunker host-provision --apply` as root")
				}
				if st.ScratchRootPresent && !st.ScratchRootModeOK {
					fmt.Fprintf(out, "the cross-agent exchange root %s is NOT setgid/no-group-write (want %04o): an agent could create an uncapped entry beside its capped directory\n", opts.ScratchRoot, hostsetup.ScratchRootMode)
				}
				return nil
			}

			if uninstall {
				if !apply {
					fmt.Fprintln(out, "dry run — pass --apply to remove the Bunker-managed host configuration")
				}
				// INT-DEMO-001: the uninstall path is deliberately NEVER gated
				// by the daemon-skew check — returning the host to a shared /tmp
				// must always remain possible, whatever daemon is installed.
				rep, err := opts.RemoveTmpNamespace(ctx)
				printReport(out, rep)
				if err != nil {
					return err
				}
				rep, err = opts.RemoveHostTmpCap(ctx, apply)
				printReport(out, rep)
				if err != nil {
					return err
				}
				// The shared scratch tree is host data, not configuration:
				// removing it would delete agents' exchanged files, so it is
				// only reported.
				fmt.Fprintf(out, "note: %s holds agent data and was not removed\n", opts.ScratchRoot)
				return nil
			}

			if !apply {
				fmt.Fprintln(out, "dry run — pass --apply to make these changes")
			}

			// INT-DEMO-001: refuse (or, with --allow-daemon-skew, loudly warn
			// about) an installed daemon older than the spawn-side isolation
			// grant BEFORE anything is planned or written.
			if err := opts.CheckDaemonSkew(ctx, allowDaemonSkew, errOut); err != nil {
				return err
			}

			rep, err := opts.Apply(ctx, apply)
			printReport(out, rep)
			if err != nil {
				return err
			}

			if apply && !skipTmpCap && opts.HostTmpMaxBytes > 0 {
				fmt.Fprintf(out, "host /tmp tmpfs cap: %d bytes via %s\n", opts.HostTmpMaxBytes, opts.TmpMountDropInPath())
			}

			if apply {
				st, err := opts.Status(ctx)
				if err != nil {
					return fmt.Errorf("verify host: %w", err)
				}
				if !st.Isolated() {
					ns := st.TmpNamespace
					return fmt.Errorf("isolation is still not active after apply (module=%v classifier=%v classifier_pattern=%v pam_exec=%v "+
						"drop-in=%v drop-in_rule=%v drop-in_owner=%v drop-in_mode=%v helper=%v helper_hash=%v helper_owner=%v helper_mode=%v "+
						"helper_dir=%v helper_dir_owner=%v helper_dir_mode=%v manifest=%v manifest_owner=%v manifest_mode=%v "+
						"pam_block=%v agent_group=%v deployed_group=%q instance_root=%v instance_root_mode=%v instance_root_owner=%v): %s",
						ns.ModulePresent, ns.GuardModulePresent, ns.GuardModuleSupportsPattern, ns.ExecModulePresent,
						ns.ConfPresent, ns.ConfRuleOK, ns.ConfOwnerOK, ns.ConfModeOK,
						ns.HelperPresent, ns.HelperIntegrityOK, ns.HelperOwnerOK, ns.HelperModeOK,
						ns.HelperDirPresent, ns.HelperDirOwnerOK, ns.HelperDirModeOK,
						ns.HelperManifestPresent, ns.HelperManifestOwnerOK, ns.HelperManifestModeOK,
						ns.PAMBlockPresent, ns.AgentGroupPresent, ns.DeployedVerifyGroup,
						ns.InstanceRootPresent, ns.InstanceRootModeOK, ns.InstanceRootOwnerOK, ns.ConfRuleDetail)
				}
				if !st.ScratchRootModeOK {
					fmt.Fprintf(out, "WARNING: the cross-agent exchange root %s is not setgid/no-group-write — an agent could create an uncapped entry beside its capped directory (host-provision --apply repairs it)\n", opts.ScratchRoot)
				}
				fmt.Fprintf(out, "host isolation is active: members of %s get their own /tmp; root and non-member SSH users keep the host /tmp\n",
					opts.AgentGroup)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&apply, "apply", false, "Make the changes (default: print the plan only)")
	cmd.Flags().BoolVar(&showStatus, "status", false, "Report the current isolation state and exit")
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "Remove Bunker-managed host configuration (with --apply)")
	cmd.Flags().BoolVar(&skipTmpCap, "skip-host-tmp-cap", false, "Do not install the host /tmp tmpfs cap")
	cmd.Flags().Int64Var(&hostTmpMax, "host-tmp-max-bytes", int64(hostsetup.DefaultHostTmpMaxBytes), "Host /tmp tmpfs cap in bytes")
	cmd.Flags().Int64Var(&scratchMax, "scratch-per-agent-bytes", int64(hostsetup.DefaultScratchMaxBytes), "Per-agent shared-scratch cap in bytes")
	cmd.Flags().StringVar(&scratchRoot, "scratch-root", hostsetup.DefaultScratchRoot, "Shared scratch root directory")
	cmd.Flags().StringVar(&agentGroup, "agent-group", hostsetup.DefaultAgentGroup, "Isolation group: pam_namespace is applied only to its members, and it owns the shared scratch")
	// Kept as an alias for scripts written against the first GAP-075 revision.
	cmd.Flags().StringVar(&agentGroup, "scratch-group", hostsetup.DefaultScratchGroup, "Deprecated alias of --agent-group")
	cmd.Flags().StringVar(&tmpInstRoot, "private-tmp-root", hostsetup.DefaultTmpInstanceRoot, "pam_namespace /tmp instance parent")
	cmd.Flags().StringVar(&daemonBinary, "daemon-binary", hostsetup.DefaultDaemonBinary, "Installed daemon binary the version-skew probe inspects (must report >= "+hostsetup.MinDaemonVersion+", the release that added the spawn-side isolation grant)")
	cmd.Flags().BoolVar(&allowDaemonSkew, "allow-daemon-skew", false, "Proceed even when the installed daemon is older than "+hostsetup.MinDaemonVersion+" (prints a loud warning; agents it spawns will be denied SSH sessions until the daemon is upgraded)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit machine-readable output (--status)")

	return cmd
}

// printReport writes a provisioning report, one line per change.
func printReport(out io.Writer, rep *hostsetup.Report) {
	if rep == nil {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(rep.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintln(out, "  "+line)
	}
}

// writeJSON emits a JSON view of the isolation status.
func writeJSON(out io.Writer, opts hostsetup.Options, st hostsetup.Status) error {
	payload := map[string]any{
		"isolated": st.Isolated(),
		"daemon_skew": map[string]any{
			"state":             string(st.DaemonSkew),
			"minimum_version":   hostsetup.MinDaemonVersion,
			"installed_binary":  st.DaemonSkewBuild.Binary,
			"installed_version": st.DaemonSkewBuild.Version,
			"installed_commit":  st.DaemonSkewBuild.Commit,
			"installed_built":   st.DaemonSkewBuild.Built,
		},
		"private_tmp": map[string]any{
			"module_present":            st.TmpNamespace.ModulePresent,
			"module_path":               st.TmpNamespace.ModulePath,
			"classifier_module":         st.TmpNamespace.GuardModulePresent,
			"classifier_module_path":    st.TmpNamespace.GuardModulePath,
			"classifier_pattern_ok":     st.TmpNamespace.GuardModuleSupportsPattern,
			"classifier_missing_tokens": st.TmpNamespace.GuardModuleMissingTokens,
			"pam_exec_module":           st.TmpNamespace.ExecModulePresent,
			"pam_exec_module_path":      st.TmpNamespace.ExecModulePath,
			"agent_name_pattern":        hostsetup.NamespacePAMAgentPattern,
			"agent_group":               st.TmpNamespace.AgentGroup,
			"agent_group_present":       st.TmpNamespace.AgentGroupPresent,
			"sshd_pam_group":            st.TmpNamespace.DeployedVerifyGroup,
			"namespace_drop_in":         st.TmpNamespace.ConfPresent,
			"namespace_conf":            st.TmpNamespace.ConfPath,
			"namespace_drop_in_rule_ok": st.TmpNamespace.ConfRuleOK,
			"namespace_drop_in_rule":    st.TmpNamespace.ConfRuleDetail,
			"namespace_drop_in_owner":   st.TmpNamespace.ConfOwner,
			"namespace_drop_in_mode":    fmt.Sprintf("%04o", st.TmpNamespace.ConfMode),
			"helper":                    st.TmpNamespace.HelperPath,
			"helper_present":            st.TmpNamespace.HelperPresent,
			"helper_owner":              st.TmpNamespace.HelperOwner,
			"helper_mode":               fmt.Sprintf("%04o", st.TmpNamespace.HelperMode),
			"helper_owner_ok":           st.TmpNamespace.HelperOwnerOK,
			"helper_mode_ok":            st.TmpNamespace.HelperModeOK,
			"helper_hash":               st.TmpNamespace.HelperHash,
			"helper_expected_hash":      st.TmpNamespace.HelperExpectedHash,
			"helper_integrity_ok":       st.TmpNamespace.HelperIntegrityOK,
			"helper_dir":                st.TmpNamespace.HelperDir,
			"helper_dir_present":        st.TmpNamespace.HelperDirPresent,
			"helper_dir_owner":          st.TmpNamespace.HelperDirOwner,
			"helper_dir_mode":           fmt.Sprintf("%04o", st.TmpNamespace.HelperDirMode),
			"helper_dir_owner_ok":       st.TmpNamespace.HelperDirOwnerOK,
			"helper_dir_mode_ok":        st.TmpNamespace.HelperDirModeOK,
			"manifest_owner":            st.TmpNamespace.HelperManifestOwner,
			"manifest_mode":             fmt.Sprintf("%04o", st.TmpNamespace.HelperManifestMode),
			"manifest_owner_ok":         st.TmpNamespace.HelperManifestOwnerOK,
			"manifest_mode_ok":          st.TmpNamespace.HelperManifestModeOK,
			"sshd_pam_block":            st.TmpNamespace.PAMBlockPresent,
			"sshd_pam_path":             st.TmpNamespace.SSHDConfigPath,
			"instance_root":             st.TmpNamespace.InstanceRoot,
			"instance_root_present":     st.TmpNamespace.InstanceRootPresent,
			"instance_root_mode":        fmt.Sprintf("%04o", st.TmpNamespace.InstanceRootMode),
			"instance_root_mode_ok":     st.TmpNamespace.InstanceRootModeOK,
			"instance_root_owner":       st.TmpNamespace.InstanceRootOwner,
			"instance_root_owner_ok":    st.TmpNamespace.InstanceRootOwnerOK,
			"ownership_verifiable":      st.TmpNamespace.OwnershipVerifiable,
		},
		"shared_scratch": map[string]any{
			"group_present":         st.AgentGroupPresent,
			"root_present":          st.ScratchRootPresent,
			"root_owner":            st.ScratchRootOwner,
			"root_mode_ok":          st.ScratchRootModeOK,
			"root_expected_mode":    fmt.Sprintf("%04o", hostsetup.ScratchRootMode),
			"root":                  opts.ScratchRoot,
			"group":                 opts.AgentGroup,
			"cap_bytes":             opts.ScratchPerAgent,
			"exchange_not_writable": st.ScratchRootModeOK,
		},
		"host_tmp": map[string]any{
			"fstype":          st.HostTmp.FSType,
			"is_tmpfs":        st.HostTmp.IsTmpfs,
			"live_size_bytes": st.HostTmp.SizeBytes,
			"drop_in":         st.HostTmp.DropInPath,
			"drop_in_present": st.HostTmp.DropInPresent,
			"cap_bytes":       st.HostTmp.DropInCappedBytes,
		},
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}
