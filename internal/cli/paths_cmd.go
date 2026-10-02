package cli

import (
	"fmt"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/spf13/cobra"
)

// NewPathsCommand returns the `bunker paths` diagnostic (GAP-181): for every
// secret/state path the CLI and daemon resolve — bunkerd auth token,
// jwt_secret, CLI per-host token, agent SSH keys dir, audit/state paths,
// secrets dir — it prints the resolved ABSOLUTE location, the file (or
// intended) mode, and WHICH resolution rule produced it. Locations and modes
// only: secret VALUES are never printed (VerifyNoLeaks asserts that in the
// test suite, and the output contains no value by construction).
//
// With --enforce the command additionally fails closed: every listed
// filesystem path must exist, and the first missing one is reported naming
// the exact path searched — the same contract the components' own gates
// (CheckAuth, EnsureJWTSecret, audit open) enforce at their boundaries.
func NewPathsCommand() *cobra.Command {
	var (
		enforce   bool
		noDaemon  bool
		daemonCfg string
	)
	cmd := &cobra.Command{
		Use:   "paths",
		Short: "Print every resolved secret/state path (locations and modes only)",
		Long: `Prints, for every secret/state path bunker and bunkerd resolve:

  the resolved absolute location, the file mode (or the mode it would be
  created with), and WHICH resolution rule produced it.

The precedence chain is flag > environment variable > per-user config dir
(${XDG_CONFIG_HOME:-~/.config}/bunker, with the legacy ~/.bunker kept as a
read alias) > documented system paths (/etc/bunkerd, /var/log/bunkerd,
/var/lib/bunkerd). Resolution never consults the process working directory,
so running the binary from any folder resolves the same paths and creates
nothing in that folder.

Secret VALUES are never printed — locations and modes only.

Use --enforce to fail closed when a required path is missing: the error
names the exact path searched instead of a generic "no token".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			res := ResolvePaths()

			if !noDaemon {
				path := daemonConfigPath()
				if daemonCfg != "" {
					path = daemonCfg
				}
				if cfg, ok := loadDaemonConfig(); ok {
					// GAP-129 semantics: the config's *_FILE paths were
					// resolved at Load; the env indirection is re-read here
					// the same way resolveSecret reads it.
					res = ResolveDaemonPaths(cfg, path)
				}
				// A missing/unreadable daemon config is not an error: the
				// CLI-side entries stay, the daemon entries show the
				// documented defaults with rule=system.
				res.DaemonConfig = daemonConfigEntry(path)
				res = withDaemonDefaults(res, path)
			}

			PrintPaths(out, res, "bunker paths — resolved secret/state locations (GAP-181)")

			if enforce {
				if err := EnforceAll(res); err != nil {
					return err
				}
				fmt.Fprintln(out, "OK: every listed path exists.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&enforce, "enforce", false, "Fail closed: error naming the exact path when a required path is missing")
	cmd.Flags().BoolVar(&noDaemon, "cli-only", false, "Print only CLI-side paths (skip the daemon config and its paths)")
	cmd.Flags().StringVar(&daemonCfg, "daemon-config", "", "Daemon config file to consult (default: --daemon-config flag > $BUNKERD_CONFIG > /etc/bunkerd/config.yaml)")
	return cmd
}

// daemonConfigEntry renders the daemon-config-file row with the daemon-level
// rule vocabulary (flag/env/system) instead of the CLI-side xdg label.
func daemonConfigEntry(path string) PathEntry {
	return PathEntry{
		Component: "bunkerd",
		Name:      "daemon config file",
		Path:      path,
		Mode:      modeOf(path, 0o600),
		Rule:      daemonConfigRule(),
		Exists:    fileExists(path),
	}
}

// withDaemonDefaults fills the daemon-side rows for a CLI run where no
// daemon config file exists: the documented system defaults, labeled
// rule=system, with the secret rows left at their unset markers so the
// listing stays honest about what the daemon would actually resolve.
func withDaemonDefaults(res *ResolvedPaths, daemonCfgPath string) *ResolvedPaths {
	if res.DaemonConfig.Exists {
		return res
	}
	empty := &config.Config{Agent: config.DefaultConfig().Agent}
	empty.Audit.Path = ""
	empty.Agent.Registry.Path = ""
	base := ResolveDaemonPaths(empty, daemonCfgPath)
	res.AuditLog = base.AuditLog
	res.AuditShip = base.AuditShip
	res.Registry = base.Registry
	res.SecretsDir = base.SecretsDir
	return res
}
