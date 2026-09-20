package cli

// NewGuardCommand returns `bunker guard` — the enforcement half of the
// do-not-build guard (GAP-105).
//
// The marker written by mount is only useful if something CHECKS it before a
// local build runs. The reliable check point is the shell: these wrappers are
// printed by `bunker guard install` and dropped into the operator's shell rc,
// so `go`, `make`, etc. inside a mount are intercepted before they touch SFTP.
//
// `bunker guard check <tool>` is the single entry the wrappers call; it exits 0
// (proceed) or non-zero with the refusal (stop). The wrappers are plain shell,
// so they work in any POSIX shell without bunker on the PATH... as long as the
// evaluation itself is cheap, which it is: one stat per parent directory.

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// NewGuardCommand builds the cobra command.
func NewGuardCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "Do-not-build guard for SSHFS mounts",
		Long: `Prevent local builds inside a bunker SSHFS mount.

A local build over SFTP is pathologically slow and defeats the point of
offloading compute to bunker agents. Mount writes a marker at the mountpoint
root; ` + "`bunker guard check <tool>`" + ` refuses intercepted build tools that
would run inside a mounted tree, and names the remote equivalent.

Install the shell wrappers once per shell with:

  eval "$(bunker guard install)

After that, running go/make/cargo/npm/... inside a mount prints the refusal and
runs nothing; outside a mount the wrappers are pure pass-through (one marker
stat per parent directory, typical cost well under a millisecond).`,
	}

	cmd.AddCommand(newGuardCheckCommand())
	cmd.AddCommand(newGuardInstallCommand())
	return cmd
}

func newGuardCheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "check <tool>",
		Args:      cobra.ExactArgs(1),
		ValidArgs: BuildToolsThatMustRunRemote,
		Short:     "Refuse an intercepted build tool inside a mounted tree (used by the shell wrappers)",
		RunE: func(cmd *cobra.Command, args []string) error {
			tool := args[0]
			wd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("resolve working directory: %w", err)
			}
			if err := CheckBuildGuard(tool, wd); err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(97) // distinct from generic errors so scripts can special-case
			}
			return nil
		},
	}
}

func newGuardInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Print the shell wrappers that install the guard",
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				exe = "bunker"
			}
			fmt.Print(guardWrapperScript(exe))
			return nil
		},
	}
}

// guardWrapperScript generates POSIX-compatible wrappers for every intercepted
// tool. Each wrapper: checks the guard first (pass-through when not refused),
// then execs the real binary found on PATH.
func guardWrapperScript(bunkerPath string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# bunker do-not-build guard — drop into your shell rc via: eval \"$(bunker guard install)\"")
	fmt.Fprintln(&b, "# Wrappers pass through untouched when not inside a bunker SSHFS mount.")
	for _, tool := range BuildToolsThatMustRunRemote {
		fmt.Fprintf(&b, `bunker_%s_guard() {
  if ! %s guard check %s >/dev/null 2>&1; then
    return 97
  fi
  command %s "$@"
}
%s() { bunker_%s_guard "$@"; }
`, tool, bunkerPath, tool, tool, tool, tool)
	}
	fmt.Fprintln(&b,
		`# To bypass (e.g. you genuinely know better): command <tool> …
# To uninstall: unset -f bunker_go_guard bunker_make_guard … & companions; see bunker guard install --print-uninstall`)
	return b.String()
}
