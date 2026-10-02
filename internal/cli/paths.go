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
//  2. an environment variable (BUNKER_HOME, BUNKERD_CONFIG,
//     BUNKER_CONFIG_HOME)
//  3. the per-user config dir (${XDG_CONFIG_HOME:-~/.config}/bunker), or —
//     while a config still lives there — the legacy ~/.bunker state dir,
//     which is adopted into the config dir (never moved) on the first run
//  4. the documented system constants (/etc/bunkerd/config.yaml and the
//     daemon's /var/log/bunkerd + /var/lib/bunkerd state)
//
// GAP-181: resolution never consults the process CWD, so a binary run from
// any folder resolves the SAME paths and writes NOTHING into the folder it
// was launched from. An empty or whitespace-only flag or env value is
// treated as UNSET, so `BUNKER_HOME="" bunker list` behaves exactly like
// `bunker list`.

// envBunkerHome is the environment variable that relocates the bunker CLI
// state directory (CLI config at $BUNKER_HOME/config.yaml, agent keys under
// $BUNKER_HOME/keys/). When it is set it overrides BOTH config-dir tiers —
// it is the operator naming the whole state tree explicitly. When it is
// unset the state directory is the per-user config dir (legacy: ~/.bunker).
const envBunkerHome = "BUNKER_HOME"

// EnvBunkerConfigHome relocates ONLY the per-user config dir (config.yaml,
// agent keys). It is the flag/env-tier override of
// ${XDG_CONFIG_HOME:-~/.config}/bunker, for hosts where the XDG location is
// not the wanted one. Exported so the paths diagnostic and tests can name it
// without duplicating the spelling.
const EnvBunkerConfigHome = "BUNKER_CONFIG_HOME"

// envBunkerdConfig is the environment variable that names the DAEMON config
// file the local-file commands consult for their default paths. It is the
// env-tier equivalent of the root --daemon-config flag.
const envBunkerdConfig = "BUNKERD_CONFIG"

// DefaultDaemonConfigPath is the default location of the bunkerd daemon
// config file. The --daemon-config flag and BUNKERD_CONFIG override it.
const DefaultDaemonConfigPath = "/etc/bunkerd/config.yaml"

// DefaultBunkerConfigDir is the CLI's per-user config dir RELATIVE to the
// XDG base: ${XDG_CONFIG_HOME:-~/.config}/bunker. GAP-181 makes it the
// documented home of the CLI's persistent state (config.yaml, keys/).
const DefaultBunkerConfigDir = "bunker"

// DefaultLegacyBunkerConfigDir is the LEGACY per-user state dir relative to
// $HOME. A config found here is ADOPTED into the config dir (copied, not
// moved — the legacy file is left exactly as it was) and every later
// resolution uses the adopted copy; the legacy dir is a read alias only
// while no config exists in the config dir.
const DefaultLegacyBunkerConfigDir = ".bunker"

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

// bunkerConfigBaseDir returns the per-user config dir WITHOUT the legacy
// fallback: $BUNKER_CONFIG_HOME/bunker when BUNKER_CONFIG_HOME is set
// (non-blank), otherwise ${XDG_CONFIG_HOME:-~/.config}/bunker. Both vars
// name a BASE — the bunker dir is appended — so an override relocates the
// whole config dir, not just its parent. This is the DOCUMENTED location —
// the paths diagnostic prints it with the rule that produced it, and the
// no-cwd-artifacts guarantee is tested against it.
func bunkerConfigBaseDir() (string, error) {
	base := ""
	if xh := trimToUnset(os.Getenv(EnvBunkerConfigHome)); xh != "" {
		base = xh
	} else {
		xdg, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine config directory: %w", err)
		}
		base = xdg
	}
	return filepath.Join(base, DefaultBunkerConfigDir), nil
}

// legacyBunkerStateDir returns the LEGACY per-user state dir:
// $HOME/.bunker. It stays a READ alias for an existing legacy config (and
// the adoption source) until the operator removes it; it is never created.
func legacyBunkerStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, DefaultLegacyBunkerConfigDir), nil
}

// migrateLegacyStateDir adopts an existing legacy ~/.bunker/config.yaml into
// the per-user config dir: COPY (never move), leaving the legacy file
// byte-identical on disk so an old binary (or a downgrade) keeps working
// unchanged. The config dir is created 0700, the copy is written 0600, and
// the rename is atomic so a crash mid-adoption leaves either the old or the
// new file, never a truncated one. An existing config-dir file is NEVER
// overwritten — the config dir wins once it has a config of its own. All
// failures are soft (report only): a broken adoption must not brick the
// command the operator actually ran.
func migrateLegacyStateDir(legacyPath, cfgDir string) error {
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return fmt.Errorf("read legacy config %s: %w", legacyPath, err)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return fmt.Errorf("create config dir %s: %w", cfgDir, err)
	}
	dst := filepath.Join(cfgDir, "config.yaml")
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("config %s already exists; legacy %s left untouched", dst, legacyPath)
	}
	tmp, err := os.CreateTemp(cfgDir, ".config.yaml-*")
	if err != nil {
		return fmt.Errorf("stage adoption of %s: %w", legacyPath, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write adopted config %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write adopted config %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod adopted config %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("adopt %s -> %s: %w", legacyPath, dst, err)
	}
	return nil
}

// bunkerStateDir returns the CLI state directory: $BUNKER_HOME when set
// (non-blank) — that tier relocates the WHOLE state tree, config and keys.
// Otherwise the per-user config dir
// (${XDG_CONFIG_HOME:-~/.config}/bunker, $BUNKER_CONFIG_HOME overriding the
// base): when it has no config.yaml yet but the LEGACY ~/.bunker/config.yaml
// exists, the legacy config is adopted into the config dir first (copied,
// never moved — see migrateLegacyStateDir) so the documented location
// becomes authoritative. Until an adoption (or a first write) creates it,
// the legacy dir is read as the state dir so pre-GAP-181 deployments keep
// working unchanged. The error matches the historical os.UserHomeDir
// failure surface.
func bunkerStateDir() (string, error) {
	if bh := trimToUnset(os.Getenv(envBunkerHome)); bh != "" {
		return bh, nil
	}
	cfgDir, err := bunkerConfigBaseDir()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "config.yaml")); err == nil {
		return cfgDir, nil
	}
	legacyDir, lerr := legacyBunkerStateDir()
	if lerr != nil {
		return cfgDir, nil // no usable HOME either: resolve to the documented dir
	}
	legacyPath := filepath.Join(legacyDir, "config.yaml")
	if _, err := os.Stat(legacyPath); err == nil {
		if merr := migrateLegacyStateDir(legacyPath, cfgDir); merr != nil {
			// Soft: report and keep reading the legacy location.
			recordLegacyMigrationError(merr)
			return legacyDir, nil
		}
		recordLegacyMigration(legacyPath, filepath.Join(cfgDir, "config.yaml"))
		return cfgDir, nil
	}
	if _, err := os.Stat(legacyDir); err == nil {
		// Legacy dir present but no config in it (fresh operator with an old
		// keys/ tree): keep resolving there until a config exists, so the
		// keys/ directory does not split across two trees.
		return legacyDir, nil
	}
	return cfgDir, nil
}

// defaultSSHKeyPath is defined in cp.go and resolves keys/ next to
// configFilePath(), so BUNKER_HOME and --config relocate the agent-key
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
