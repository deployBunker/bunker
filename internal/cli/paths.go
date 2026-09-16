package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/config"
)

// This file is the single resolution point for every client-local default
// path the CLI uses: the CLI config file (configFilePath), and the default
// file paths of the local-file commands (audit list/verify/export --path,
// registry compact --path, audit status --path).
//
// Precedence everywhere, highest first:
//
//  1. an explicit command-line flag (--config, --path)
//  2. an environment variable (BUNKER_HOME, BUNKERD_CONFIG)
//  3. the historical default ($HOME/.bunker/... and the documented
//     daemon constants /var/log/bunkerd/audit.log and
//     /var/lib/bunkerd/agents.jsonl)
//
// An empty or whitespace-only flag or env value is treated as UNSET, so
// `BUNKER_HOME="" bunker list` behaves exactly like `bunker list`.

// envBunkerHome is the environment variable that relocates the bunker CLI
// state directory (CLI config at $BUNKER_HOME/config.yaml, agent keys under
// $BUNKER_HOME/keys/). When it is unset the state directory is $HOME/.bunker.
const envBunkerHome = "BUNKER_HOME"

// envBunkerdConfig is the environment variable that names the DAEMON config
// file the local-file commands consult for their default paths. It is the
// env-tier equivalent of the root --daemon-config flag.
const envBunkerdConfig = "BUNKERD_CONFIG"

// DefaultDaemonConfigPath is the default location of the bunkerd daemon
// config file. The --daemon-config flag and BUNKERD_CONFIG override it.
const DefaultDaemonConfigPath = "/etc/bunkerd/config.yaml"

// configPathOverride holds the explicit --config value for the CLI config
// file (highest precedence in configFilePath). It is set by SetConfigPathOverride
// before cobra parses/starts the command tree; tests may reset it with
// ResetConfigPathOverride.
var configPathOverride string

// daemonConfigPathOverride holds the explicit --daemon-config value (highest
// precedence for the daemon config file location). Tests may reset it.
var daemonConfigPathOverride string

// SetConfigPathOverride records the explicit CLI-config-file path given via
// the root --config flag. main calls this before Execute, so every command
// (all of which load the CLI config) sees the same value. An empty or
// whitespace-only value is treated as unset. The override is also consulted
// by DefaultSSHKeyPath, so BUNKER_HOME and --config relocate the agent-key
// directory together with the config file.
func SetConfigPathOverride(path string) {
	configPathOverride = trimToUnset(path)
}

// SetDaemonConfigPathOverride records the explicit daemon-config-file path
// given via the root --daemon-config flag. Same unset rules.
func SetDaemonConfigPathOverride(path string) {
	daemonConfigPathOverride = trimToUnset(path)
}

// ResetConfigPathOverride clears the CLI config path override (test escape
// hatch; production code never resets it).
func ResetConfigPathOverride() {
	configPathOverride = ""
}

// ResetDaemonConfigPathOverride clears the daemon config path override (test
// escape hatch; production code never resets it).
func ResetDaemonConfigPathOverride() {
	daemonConfigPathOverride = ""
}

// trimToUnset collapses an empty or whitespace-only value to "" (unset).
func trimToUnset(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// bunkerStateDir returns the CLI state directory: $BUNKER_HOME when set
// (non-blank), otherwise $HOME/.bunker. The error matches the historical
// os.UserHomeDir failure surface.
func bunkerStateDir() (string, error) {
	if bh := trimToUnset(os.Getenv(envBunkerHome)); bh != "" {
		return bh, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".bunker"), nil
}

// defaultSSHKeyPath is defined in cp.go and resolves keys/ next to
// configFilePath(), so --config and BUNKER_HOME relocate the agent-key
// directory together with the config file.

// daemonConfigPath resolves the daemon config file location for the
// local-file command defaults: --daemon-config flag > $BUNKERD_CONFIG >
// /etc/bunkerd/config.yaml.
func daemonConfigPath() string {
	if daemonConfigPathOverride != "" {
		return daemonConfigPathOverride
	}
	if env := trimToUnset(os.Getenv(envBunkerdConfig)); env != "" {
		return env
	}
	return DefaultDaemonConfigPath
}

// loadDaemonConfig reads and parses the daemon config file for the CLI's
// default-path lookups. The bool result is false when the file does not
// exist or cannot be read/parsed — callers fall back to their documented
// constant default and NEVER surface an error (a broken daemon config must
// not brick local inspection commands).
func loadDaemonConfig() (*config.Config, bool) {
	path := daemonConfigPath()
	if _, err := os.Stat(path); err != nil {
		return nil, false
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, false
	}
	return cfg, true
}

// DefaultAuditLogPath returns the audit-log default for the --path flag of
// audit list/verify/export/status: the daemon config's audit.path when the
// daemon config file exists and parses, otherwise the documented constant
// defaultAuditLogPath. Never fails.
func DefaultAuditLogPath() string {
	if cfg, ok := loadDaemonConfig(); ok {
		if p := trimToUnset(cfg.Audit.Path); p != "" {
			return p
		}
	}
	return defaultAuditLogPath
}

// DefaultRegistryPath returns the registry default for the --path flag of
// registry compact: the daemon config's agent.registry.path when the daemon
// config file exists and parses, otherwise the documented constant
// config.DefaultRegistryPath. Never fails.
func DefaultRegistryPath() string {
	if cfg, ok := loadDaemonConfig(); ok {
		if p := trimToUnset(cfg.Agent.Registry.Path); p != "" {
			return p
		}
	}
	return config.DefaultRegistryPath
}

// defaultRegistryPathFlag mirrors defaultAuditPathFlag (audit.go) for
// `bunker registry compact`: re-resolves the --path DEFAULT from the daemon
// config at RunE time unless the operator set --path explicitly.
func defaultRegistryPathFlag(cmd *cobra.Command, flagName string) string {
	def := DefaultRegistryPath()
	if f := cmd.Flags().Lookup(flagName); f != nil && !f.Changed {
		f.DefValue = def
		f.Value.Set(def)
		return def
	}
	if f := cmd.Flags().Lookup(flagName); f != nil {
		return f.Value.String()
	}
	return def
}
