package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/deployBunker/bunker/internal/config"
)

// PathRule says WHICH resolution rule produced a path. The vocabulary is the
// documented precedence chain (GAP-181), highest precedence first in every
// listing:
//
//	flag        — an explicit --config / --path / --token-file flag named it
//	env         — an environment variable named it (BUNKER_HOME,
//	              BUNKER_CONFIG_HOME, BUNKERD_CONFIG, BUNKER_AUTH_*_FILE,
//	              BUNKER_SECRETS_DIR)
//	config      — a loaded daemon config file named it
//	xdg         — the per-user config dir ${XDG_CONFIG_HOME:-~/.config}/bunker
//	legacy      — the legacy ~/.bunker state dir (read alias / adoption source)
//	system      — a documented system path (/etc/bunkerd, /var/log/bunkerd,
//	              /var/lib/bunkerd)
type PathRule string

const (
	PathRuleFlag   PathRule = "flag"
	PathRuleEnv    PathRule = "env"
	PathRuleConfig PathRule = "config"
	PathRuleXDG    PathRule = "xdg"
	PathRuleLegacy PathRule = "legacy"
	PathRuleSystem PathRule = "system"
)

// String renders the rule for the diagnostic output.
func (r PathRule) String() string { return string(r) }

// PathEntry is one resolved secret/state path in the `bunker paths` /
// `bunkerd --show-paths` listing: WHERE the file lives, the mode it carries
// (or is created with) and WHY that location was chosen. A missing file is
// still an entry — the diagnostic reports the location that WOULD be used —
// but the leak check only inspects entries whose Exists is true.
type PathEntry struct {
	Component string   // which binary/subsystem owns it ("cli", "bunkerd", "audit", ...)
	Name      string   // what the file is ("config.yaml", "auth token", ...)
	Path      string   // resolved ABSOLUTE location (never a secret VALUE)
	Mode      string   // actual mode when present, intended mode otherwise
	Rule      PathRule // which resolution rule produced it
	Exists    bool     // whether the file is on disk right now
	ValueKind string   // "secret" when the file content is a credential
}

// trimSecret normalizes a secret value for substring comparison: trailing
// whitespace/newlines are the normal shape of an operator-written secret
// file, and a newline-containing value cannot appear in single-line
// diagnostic output anyway.
func trimSecret(s string) string {
	return strings.TrimSpace(s)
}

// ResolvedPaths is the full secret/state path surface of one machine, as the
// diagnostic prints it and as the fail-closed sweep checks it. Build it with
// ResolvePaths (CLI) or ResolveDaemonPaths (daemon).
type ResolvedPaths struct {
	// CLI-side
	CLIConfig      PathEntry // config.yaml (server registry; carries per-host tokens)
	SSHKeysDir     PathEntry // keys/ directory (per-agent SSH private keys)
	CLITokenSource PathEntry // where BUNKER_TOKEN / --token currently comes from
	CLITokenValue  string    // the current token value, or "" — NEVER printed
	// Daemon-side
	AuthTokenFile PathEntry // master auth token (auth.token_file / env-file)
	JWTSecretFile PathEntry // generated/loaded jwt_secret
	AuditLog      PathEntry // audit JSONL log (hash-chained, token-free)
	AuditShip     PathEntry // audit ship-state sidecar (<log>.shipstate)
	Registry      PathEntry // agents.jsonl registry
	DaemonConfig  PathEntry // daemon config.yaml
	SecretsDir    PathEntry // generated-secrets dir (dir mode 0700)
}

// readIfPresent returns the file's content (trimmed), "" when absent. Read
// errors are surfaced: a secret we cannot read cannot be asserted absent
// from the diagnostic output.
func readIfPresent(path string) (string, error) {
	if !isRealPath(path) {
		return "", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // resolved operator-configured path, not untrusted input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return trimSecret(string(b)), nil
}

// modeOf returns the actual file mode in octal, or the intended mode with a
// "missing" marker when absent.
func modeOf(path string, intended os.FileMode) string {
	if isRealPath(path) {
		if fi, err := os.Stat(path); err == nil {
			return fmt.Sprintf("%04o", fi.Mode().Perm())
		}
	}
	return fmt.Sprintf("%04o (intended, missing)", intended)
}

// resolveCLIToken answers WHERE the per-host token comes from right now:
// entry token (saved by `bunker connect`) > BUNKER_TOKEN env > unset. The
// source label is what the diagnostic prints; the value is carried on the
// result struct for the leak check only.
func resolveCLIToken() (PathEntry, string) {
	cfgPath := mustConfigFilePath()
	cfg, err := LoadCLIConfig()
	if err == nil && cfg != nil {
		if entry, ok := cfg.Servers[cfg.ActiveServer]; ok && entry.Token != "" {
			return PathEntry{
				Component: "cli",
				Name:      fmt.Sprintf("per-host token for %q (server entry)", cfg.ActiveServer),
				Path:      cfgPath,
				Mode:      modeOf(cfgPath, 0o600),
				Rule:      configRuleForCLIConfig(),
				Exists:    true,
				ValueKind: "secret",
			}, entry.Token
		}
	}
	if tok := os.Getenv("BUNKER_TOKEN"); tok != "" {
		return PathEntry{
			Component: "cli",
			Name:      "per-host token (BUNKER_TOKEN environment variable)",
			Path:      "(environment, not a file)",
			Mode:      "-",
			Rule:      PathRuleEnv,
			Exists:    true,
			ValueKind: "secret",
		}, tok
	}
	return PathEntry{
		Component: "cli",
		Name:      "per-host token (server entry in config.yaml)",
		Path:      cfgPath,
		Mode:      modeOf(cfgPath, 0o600),
		Rule:      configRuleForCLIConfig(),
		Exists:    false,
		ValueKind: "secret",
	}, ""
}

// configRuleForCLIConfig reports which tier produced the CLI config path:
// flag when overridden, env when BUNKER_HOME / BUNKER_CONFIG_HOME relocated
// the tree, else xdg (the documented per-user config dir; the legacy alias
// is folded into the same entry via the adopted path).
func configRuleForCLIConfig() PathRule {
	if configPathOverride != "" {
		return PathRuleFlag
	}
	if trimToUnset(os.Getenv(envBunkerHome)) != "" || trimToUnset(os.Getenv(EnvBunkerConfigHome)) != "" {
		return PathRuleEnv
	}
	return PathRuleXDG
}

func mustConfigFilePath() string {
	p, err := configFilePath()
	if err != nil {
		return "(unresolved: " + err.Error() + ")"
	}
	return p
}

// ResolvePaths resolves the CLI-side path surface.
func ResolvePaths() *ResolvedPaths {
	cfgPath := mustConfigFilePath()
	res := &ResolvedPaths{}
	res.CLIConfig = PathEntry{
		Component: "cli",
		Name:      "config.yaml (server registry, per-host tokens)",
		Path:      cfgPath,
		Mode:      modeOf(cfgPath, 0o600),
		Rule:      configRuleForCLIConfig(),
		Exists:    fileExists(cfgPath),
	}
	keysDir := filepath.Join(filepath.Dir(cfgPath), "keys")
	res.SSHKeysDir = PathEntry{
		Component: "cli",
		Name:      "agent SSH keys directory (keys/<agent-id>, mode 0600)",
		Path:      keysDir,
		Mode:      modeOf(keysDir, 0o700),
		Rule:      configRuleForCLIConfig(),
		Exists:    dirExists(keysDir),
	}
	res.CLITokenSource, res.CLITokenValue = resolveCLIToken()
	return res
}

// ResolveDaemonPaths resolves the daemon-side path surface from the daemon
// config (already loaded and secret-resolved by the caller). The rule for
// each entry reflects WHERE the value came from: config file fields are
// "config", *_FILE env indirections are "env", the compiled-in defaults are
// "system".
func ResolveDaemonPaths(cfg *config.Config, daemonCfgPath string) *ResolvedPaths {
	res := ResolvePaths()

	// Auth token: the *_FILE indirections win over the config field in
	// resolveSecret, so the diagnostic names the file the daemon actually
	// reads. Nothing here ever prints the token itself.
	tokenRule := PathRuleConfig
	tokenPath := ""
	if p := strings.TrimSpace(cfg.Auth.TokenFile); p != "" {
		tokenPath = p
	} else if p := strings.TrimSpace(os.Getenv(config.AuthTokenFileEnv)); p != "" {
		tokenPath = p
		tokenRule = PathRuleEnv
	} else if tok := cfg.Auth.Token; tok != "" {
		tokenPath = "(inline in " + daemonCfgPath + " — legacy storage; move to " + config.AuthTokenFileEnv + " or auth.token_file)"
	} else {
		tokenPath = "(unset — the daemon refuses to start with auth enabled)"
	}
	res.AuthTokenFile = PathEntry{
		Component: "bunkerd",
		Name:      "auth token (master API credential)",
		Path:      tokenPath,
		Mode:      modeOf(tokenPath, 0o600),
		Rule:      tokenRule,
		Exists:    isRealPath(tokenPath) && fileExists(tokenPath),
		ValueKind: "secret",
	}

	// JWT secret: auth.jwt_secret_file > BUNKER_AUTH_JWT_SECRET_FILE >
	// the generated/persisted <secrets-dir>/jwt_secret (EnsureJWTSecret
	// records it in JWTSecretFile), else inline/unset.
	jwtRule := PathRuleConfig
	jwtPath := ""
	if p := strings.TrimSpace(cfg.Auth.JWTSecretFile); p != "" {
		jwtPath = p
	} else if p := strings.TrimSpace(os.Getenv(config.AuthJWTSecretFileEnv)); p != "" {
		jwtPath = p
		jwtRule = PathRuleEnv
	} else if tok := cfg.Auth.JWTSecret; tok != "" {
		jwtPath = "(inline in " + daemonCfgPath + " — legacy storage; move to " + config.AuthJWTSecretFileEnv + " or auth.jwt_secret_file)"
	} else {
		jwtPath = "(unset — generated on first boot)"
	}
	res.JWTSecretFile = PathEntry{
		Component: "bunkerd",
		Name:      "auth.jwt_secret (JWT signing secret)",
		Path:      jwtPath,
		Mode:      modeOf(jwtPath, config.SecretsFileMode),
		Rule:      jwtRule,
		Exists:    isRealPath(jwtPath) && fileExists(jwtPath),
		ValueKind: "secret",
	}

	// Secrets dir: the same resolution EnsureJWTSecret uses. Resolve the
	// source kind so the listing can say env vs config vs ambient home.
	dir := cfg.SecretsDirOrDefault()
	secretsRule := PathRuleConfig
	if _, _, kind, _ := cfg.ResolveSecretsLocation(); kind == config.SecretsSourceEnv {
		secretsRule = PathRuleEnv
	}
	res.SecretsDir = PathEntry{
		Component: "bunkerd",
		Name:      "generated secrets directory (" + config.JWTSecretFileName + ", dir mode 0700)",
		Path:      dir,
		Mode:      modeOf(dir, config.SecretsDirMode),
		Rule:      secretsRule,
		Exists:    dirExists(dir),
	}

	// Audit log + shipstate sidecar: config field, else the documented
	// system default.
	auditPath := strings.TrimSpace(cfg.Audit.Path)
	auditRule := PathRuleConfig
	if auditPath == "" {
		auditPath = defaultAuditLogPath
		auditRule = PathRuleSystem
	}
	res.AuditLog = PathEntry{
		Component: "audit",
		Name:      "audit log (append-only JSONL, hash-chained; token-free)",
		Path:      auditPath,
		Mode:      modeOf(auditPath, 0o600),
		Rule:      auditRule,
		Exists:    fileExists(auditPath),
	}
	res.AuditShip = PathEntry{
		Component: "audit",
		Name:      "audit ship-state sidecar (retention queue)",
		Path:      auditPath + ".shipstate",
		Mode:      modeOf(auditPath+".shipstate", 0o600),
		Rule:      auditRule,
		Exists:    fileExists(auditPath + ".shipstate"),
	}

	// Registry: config field, else the documented system default.
	regPath := strings.TrimSpace(cfg.Agent.Registry.Path)
	regRule := PathRuleConfig
	if regPath == "" {
		regPath = config.DefaultRegistryPath
		regRule = PathRuleSystem
	}
	res.Registry = PathEntry{
		Component: "bunkerd",
		Name:      "agent registry (agents.jsonl)",
		Path:      regPath,
		Mode:      modeOf(regPath, 0o600),
		Rule:      regRule,
		Exists:    fileExists(regPath),
	}

	res.DaemonConfig = PathEntry{
		Component: "bunkerd",
		Name:      "daemon config file",
		Path:      daemonCfgPath,
		Mode:      modeOf(daemonCfgPath, 0o600),
		Rule:      daemonConfigRule(),
		Exists:    fileExists(daemonCfgPath),
	}
	return res
}

// daemonConfigRule labels where the daemon config path came from: flag >
// env > the documented system path.
func daemonConfigRule() PathRule {
	if daemonConfigPathOverride != "" {
		return PathRuleFlag
	}
	if trimToUnset(os.Getenv(envBunkerdConfig)) != "" {
		return PathRuleEnv
	}
	return PathRuleSystem
}

// isRealPath reports whether p is a checkable filesystem location (the
// diagnostic carries placeholder strings like "(inline in ...)" too).
func isRealPath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "(")
}

func fileExists(p string) bool {
	if !isRealPath(p) {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	if !isRealPath(p) {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// PrintPaths renders the resolved path surface. Locations and modes only —
// the function takes *ResolvedPaths, which carries no secret values, and
// the one secret-valued field (CLITokenValue) is read exclusively by
// VerifyNoLeaks.
func PrintPaths(w io.Writer, res *ResolvedPaths, header string) {
	if header != "" {
		fmt.Fprintln(w, header)
	}
	entries := []PathEntry{
		res.CLIConfig,
		res.CLITokenSource,
		res.SSHKeysDir,
		res.AuthTokenFile,
		res.JWTSecretFile,
		res.SecretsDir,
		res.DaemonConfig,
		res.AuditLog,
		res.AuditShip,
		res.Registry,
	}
	fmt.Fprintf(w, "%-10s %-46s %-80s %-22s %s\n", "COMPONENT", "NAME", "PATH", "MODE", "RULE")
	for _, e := range entries {
		fmt.Fprintf(w, "%-10s %-46s %-80s %-22s %s\n", e.Component, trunc(e.Name, 46), trunc(e.Path, 80), e.Mode, e.Rule)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Precedence: flag > env > per-user config dir (${XDG_CONFIG_HOME:-~/.config}/bunker) > legacy ~/.bunker (read alias) > system defaults (/etc/bunkerd, /var/log/bunkerd, /var/lib/bunkerd).")
	fmt.Fprintln(w, "Resolution never consults the process working directory (GAP-181): no file is ever created or read relative to CWD.")
	fmt.Fprintln(w, "Locations and modes only — secret VALUES are never printed (see docs/backup-set.md).")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// ASCII-only tail: %-Ns pads by BYTES, so a multi-byte ellipsis would
	// shift every later column left and break fixed-width parsing.
	return s[:n-3] + "..."
}

// VerifyNoLeaks asserts that no known secret VALUE appears in out. It is the
// engine of the diagnostic-leak test: the token file, the jwt_secret file
// and the current CLI token are read and checked against the rendered
// output. Unreadable secret files are an error — a secret we cannot read
// cannot be asserted absent.
func VerifyNoLeaks(res *ResolvedPaths, out string) error {
	checks := []struct {
		path string
		val  func() (string, error)
	}{
		{res.AuthTokenFile.Path, func() (string, error) { return readIfPresent(res.AuthTokenFile.Path) }},
		{res.JWTSecretFile.Path, func() (string, error) { return readIfPresent(res.JWTSecretFile.Path) }},
		{res.CLITokenSource.Path, func() (string, error) {
			if res.CLITokenValue == "" {
				return "", nil
			}
			return res.CLITokenValue, nil
		}},
	}
	for _, c := range checks {
		val, err := c.val()
		if err != nil {
			return fmt.Errorf("leak check: read secret location %s: %w", c.path, err)
		}
		if val == "" {
			continue
		}
		if strings.Contains(out, val) {
			return fmt.Errorf("leak check: diagnostic output contains the secret value stored at %s", c.path)
		}
	}
	return nil
}

// EnforceAll is the fail-closed sweep: every listed path must exist (and be
// a file, for file entries) before the caller proceeds. The FIRST failure is
// returned as an error naming the exact path searched — never a generic "no
// token" and never a silent default. `bunker paths --enforce` runs it; the
// daemon's equivalent gates stay where they are (CheckAuth / EnsureJWTSecret
// / audit open), so enforcement here is an additive operator surface.
func EnforceAll(res *ResolvedPaths) error {
	checks := []struct {
		what  string
		entry PathEntry
	}{
		{"bunkerd auth token", res.AuthTokenFile},
		{"bunkerd jwt_secret", res.JWTSecretFile},
		{"CLI per-host token config", res.CLIConfig},
		{"agent SSH keys directory", res.SSHKeysDir},
		{"audit log", res.AuditLog},
		{"audit ship-state", res.AuditShip},
		{"agent registry", res.Registry},
		{"secrets directory", res.SecretsDir},
	}
	for _, c := range checks {
		if !isRealPath(c.entry.Path) {
			continue // placeholder (unset/inline markers) — the owning gate owns that refusal
		}
		if _, err := os.Stat(c.entry.Path); err != nil {
			return fmt.Errorf("%s: required path not found: %s (resolved via rule %s) — restore it from the backup set (docs/backup-set.md) or re-run the component's setup",
				c.what, c.entry.Path, c.entry.Rule)
		}
	}
	return nil
}
