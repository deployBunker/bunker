// Package config provides configuration loading for bunkerd.
// Reads from /etc/bunkerd/config.yaml with env var overrides.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/deployBunker/bunker/internal/hostsetup"
)

// Config is the top-level bunkerd configuration.
type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	TLS         TLSConfig         `mapstructure:"tls"`
	Auth        AuthConfig        `mapstructure:"auth"`
	Agent       AgentConfig       `mapstructure:"agent"`
	Tunnel      TunnelConfig      `mapstructure:"tunnel"`
	NamedTunnel NamedTunnelConfig `mapstructure:"named_tunnel"`
	Tailscale   TailscaleConfig   `mapstructure:"tailscale"`
	Audit       AuditConfig       `mapstructure:"audit"`
	Containment ContainmentConfig `mapstructure:"containment"`
}

// ContainmentConfig holds the GAP-067 containment-exposure disclosure
// settings. Disclosure is an admin-controlled, HIDDEN-BY-DEFAULT feature:
// when enabled, managed agents honestly disclose that they run in a managed
// sandbox — a BUNKER_SANDBOX=1 env var in every agent session and a
// self-describing marker line after allowed system-info probe output. When
// disabled (the default) behavior is byte-identical to a daemon without the
// feature. See specs/containment-disclosure.md.
type ContainmentConfig struct {
	Disclosure bool `mapstructure:"disclosure"`
}

// ContainmentSandboxEnv is the canonical KEY=VALUE env var injected into
// EVERY agent session (shell exec, raw exec, script exec, detached
// RunAgent) when containment disclosure is enabled (GAP-067). It rides the
// same explicit env injection path as PATH/DOCKER_HOST/TMPDIR and is absent
// when disclosure is disabled. Single definition here — the server and
// agent packages both reference this constant; never duplicate the literal.
const ContainmentSandboxEnv = "BUNKER_SANDBOX=1"

// IsolationTmpDir is the temporary directory every agent process is pointed
// at (GAP-075). It is the plain "/tmp" on purpose: /tmp is what the isolation
// boundary makes private — per SSH session through pam_namespace, and per
// transient unit through systemd PrivateTmp=yes. A per-agent directory
// elsewhere (/run/bunker/<id>/tmp) is NOT a boundary and is no longer
// advertised as TMPDIR. Single definition — the agent and server packages both
// reference this constant; never duplicate the literal.
const IsolationTmpDir = "/tmp"

// ContainmentSandboxEnvKey is the env-var KEY of ContainmentSandboxEnv, for
// callers that must match the key alone (e.g. the detached-run builder
// rejecting agent-supplied overrides of the admin-controlled disclosure
// var). It is the literal prefix of ContainmentSandboxEnv — consistency is
// pinned by TestContainmentSandboxEnvKeyMatchesValue in this package.
const ContainmentSandboxEnvKey = "BUNKER_SANDBOX"

// ServerConfig holds gRPC and REST listener addresses and timeouts.
type ServerConfig struct {
	GRPCAddr       string        `mapstructure:"grpc_addr"`
	RESTAddr       string        `mapstructure:"rest_addr"`
	RequestTimeout time.Duration `mapstructure:"request_timeout"`
}

// TLSConfig holds TLS settings.
type TLSConfig struct {
	Enabled    bool     `mapstructure:"enabled"`
	CertFile   string   `mapstructure:"cert_file"`
	KeyFile    string   `mapstructure:"key_file"`
	AutoTLS    bool     `mapstructure:"auto_tls"`
	SelfSigned bool     `mapstructure:"self_signed"`
	Domain     string   `mapstructure:"domain"`
	MTLS       bool     `mapstructure:"mtls"`
	CAFile     string   `mapstructure:"ca_file"`
	VerifyCN   string   `mapstructure:"verify_cn"`
	Hosts      []string `mapstructure:"hosts"`
	// InsecureDev is the GAP-126 / REQ-T1 explicit opt-in for binding a
	// NON-loopback listener while TLS is disabled. It exists so the insecure
	// configuration is a deliberate, named, auditable decision instead of a
	// silent default: with it false (the default) CheckTLS REFUSES to start on
	// a non-loopback plaintext bind, and with it true the daemon starts, logs a
	// loud INSECURE warning, and stamps every audit record with
	// InsecurePlaintextMarker. It has no effect while tls.enabled is true, and
	// a loopback-only bind never needs it. Never set this on a shared or
	// internet-reachable host.
	InsecureDev bool `mapstructure:"insecure_dev"`
}

// InsecurePlaintextMarker is the audit-record marker stamped on every record
// while the daemon serves a non-loopback plaintext listener under the explicit
// tls.insecure_dev: true opt-in (GAP-126 / REQ-T1). It prefixes the record's
// Summary field, so a reader of the audit trail can never mistake a request
// that arrived over plaintext for one that arrived over TLS. The Record JSON
// field set is deliberately unchanged — the marker rides the existing
// human-readable Summary. Single definition here — internal/audit and the
// startup gate both reference this constant; never duplicate the literal.
const InsecurePlaintextMarker = "[INSECURE-PLAINTEXT]"

// APIKey holds a generated API key with metadata.
type APIKey struct {
	KeyID     string    `mapstructure:"key_id"`
	TokenHash string    `mapstructure:"token_hash"`
	AgentID   string    `mapstructure:"agent_id"`
	CreatedAt time.Time `mapstructure:"created_at"`
	ExpiresAt time.Time `mapstructure:"expires_at"`
}

// AuthConfig holds authentication settings.
//
// GAP-129 (SEC-14 / REQ-I5) secret storage: every credential has three
// accepted sources, resolved by ResolveSecrets in this precedence order
// (inline < file path < env-file). Only the LAST level keeps the secret out
// of the config file entirely:
//
//  1. inline   — auth.token / auth.jwt_secret in the config file (or the
//     BUNKERD_AUTH_TOKEN / BUNKERD_AUTH_JWT_SECRET env vars).
//     Legacy: still supported, but CheckAuth warns about it.
//  2. file path — auth.token_file / auth.jwt_secret_file point at a file
//     holding the secret (mode 0600); the file value wins over an
//     inline value so an operator can move a secret out of the
//     config without deleting the old line in the same edit.
//  3. env-file  — BUNKER_AUTH_TOKEN_FILE / BUNKER_AUTH_JWT_SECRET_FILE name
//     the file. Wins over both of the above. A set-but-unreadable
//     env path is a hard error (fail-before-listen), never a
//     silent fallback to a weaker source.
//
// A missing/unreadable file named by the CONFIG is also a hard error: a
// daemon that silently ignored it would run with a weaker credential than
// the operator asked for.
type AuthConfig struct {
	Enabled   bool          `mapstructure:"enabled"`
	Token     string        `mapstructure:"token"`
	JWTSecret string        `mapstructure:"jwt_secret"`
	JWTTTL    time.Duration `mapstructure:"jwt_ttl"`
	// TokenFile / JWTSecretFile are the config-file level indirection: the
	// path to a file holding the secret (trailing whitespace trimmed).
	// Env-file override: BUNKER_AUTH_TOKEN_FILE / BUNKER_AUTH_JWT_SECRET_FILE.
	TokenFile string `mapstructure:"token_file"`
	// JWTSecretFile — see TokenFile.
	JWTSecretFile string `mapstructure:"jwt_secret_file"`
}

// Env vars for the *_FILE indirection (GAP-129). The BUNKER_ prefix matches
// the daemon's other secret-bearing knobs (BUNKER_ROOTLESS_INSTALLER_CACHE_DIR)
// and keeps the credential path out of the BUNKERD_* config-key namespace.
const (
	// AuthTokenFileEnv names a file holding the master token.
	AuthTokenFileEnv = "BUNKER_AUTH_TOKEN_FILE"
	// AuthJWTSecretFileEnv names a file holding the JWT signing secret.
	AuthJWTSecretFileEnv = "BUNKER_AUTH_JWT_SECRET_FILE"
	// SecretsDirEnv overrides where the daemon persists generated secrets.
	SecretsDirEnv = "BUNKER_SECRETS_DIR"
)

// Secrets-location defaults: the generated jwt_secret file, the file it is
// persisted in, and the permission bits the daemon creates (dir 0700, file
// 0600 — owner-only, never group/world readable).
const (
	// DefaultSecretsDir is where generated secrets are persisted when
	// BUNKER_SECRETS_DIR is unset.
	DefaultSecretsDir = ".config/bunkerd/secrets"
	// JWTSecretFileName is the persisted auto-generated JWT signing secret.
	JWTSecretFileName = "jwt_secret"
	// SecretsDirMode is the mode of the secrets directory (owner rwx only).
	SecretsDirMode os.FileMode = 0o700
	// SecretsFileMode is the mode of each persisted secret file.
	SecretsFileMode os.FileMode = 0o600
	// GeneratedJWTSecretBytes is the size of an auto-generated JWT secret.
	// 32 bytes (64 hex chars) is the HS256 key size.
	GeneratedJWTSecretBytes = 32
)

// SecretsDirOrDefault returns the directory the daemon persists generated
// secrets in: BUNKER_SECRETS_DIR when set, otherwise $HOME/<DefaultSecretsDir>.
// It is a lookup, not a create — the caller creates it 0700 on write. A host
// with no HOME yields the relative default, which fails loudly at write time
// rather than silently scattering secrets into the process CWD.
func SecretsDirOrDefault() string {
	if env := strings.TrimSpace(os.Getenv(SecretsDirEnv)); env != "" {
		return env
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, DefaultSecretsDir)
	}
	return DefaultSecretsDir
}

// AuditConfig holds the daemon-side audit trail settings. When enabled, every
// authenticated RPC appends one JSONL record to Path (mode 0600). The log
// never contains token values.
//
// GAP-073 retention hardening (both opt-in, OFF by default — a config that
// does not set them gets the exact pre-GAP-073 behavior):
//   - ShipTo: remote ship endpoint for rotated segments. Supported schemes:
//     https:// or http:// webhook (segment POSTed as the request body with
//     X-Bunker-Chain-Head set) and syslog://host[:port] (RFC 3164 datagrams
//     over UDP — the lossless copy is the webhook). Empty = off. Shipping is
//     fire-and-forget: a dead endpoint never blocks or fails the audit write.
//   - SealKey: when set, every rotation appends a chained SEAL record to the
//     fresh log carrying HMAC-SHA256(key=SealKey, msg=<sealed chain head>),
//     which lets holders of shipped copies prove a local file was truncated
//     or replaced. Empty = no seal records.
type AuditConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
	ShipTo  string `mapstructure:"ship_to"`
	SealKey string `mapstructure:"seal_key"`
}

// AgentConfig holds agent lifecycle settings.
type AgentConfig struct {
	BaseDataDir                string        `mapstructure:"base_data_dir"`
	SSHDir                     string        `mapstructure:"ssh_dir"`
	PortRangeStart             uint32        `mapstructure:"port_range_start"`
	PortRangeEnd               uint32        `mapstructure:"port_range_end"`
	PortRangePerAgent          uint32        `mapstructure:"port_range_per_agent"`
	MaxAgents                  uint32        `mapstructure:"max_agents"`
	DefaultCPUQuota            float64       `mapstructure:"default_cpu_quota"`
	DefaultMemoryBytes         uint64        `mapstructure:"default_memory_bytes"`
	DefaultDiskBytes           uint64        `mapstructure:"default_disk_bytes"`
	DefaultMaxProcesses        uint64        `mapstructure:"default_max_processes"`
	DefaultMaxOpenFiles        uint64        `mapstructure:"default_max_open_files"`
	DefaultMaxDockerContainers uint32        `mapstructure:"default_max_docker_containers"`
	DefaultTTL                 time.Duration `mapstructure:"default_ttl"`
	// ImageSpec holds the GAP-064 image-customization policy.
	ImageSpec ImageSpecConfig `mapstructure:"image_spec"`
	// RootlessInstallerCacheDir is the host-level directory where downloaded
	// rootless Docker installers are cached between spawns (GAP-091). On a
	// cache hit a fresh-agent spawn skips the ~93MB get.docker.com download
	// entirely. Empty string keeps the legacy behavior: every spawn downloads
	// from the network. Env override: BUNKER_ROOTLESS_INSTALLER_CACHE_DIR.
	RootlessInstallerCacheDir string `mapstructure:"rootless_installer_cache_dir"`
	// Registry holds the GAP-070 durable agent registry settings.
	Registry RegistryConfig `mapstructure:"registry"`
	// Reconciliation holds the GAP-070 startup reconciliation policy.
	Reconciliation ReconciliationConfig `mapstructure:"reconciliation"`
	// Isolation holds the GAP-075 per-agent /tmp + shared-scratch policy.
	Isolation IsolationConfig `mapstructure:"isolation"`
	// DestroyHomePolicy selects what destroy does with an agent's home
	// directory before the Linux user is removed (DF-BUNKER-33):
	// "archive" (the default) tars the home into DestroyArchiveDir and
	// VERIFIES the archive before running `userdel -rf`; "purge" keeps the
	// historical behavior and destroys the home together with the user.
	// Any other value resolves to the default — a typo must never silently
	// re-enable the unrecoverable delete.
	DestroyHomePolicy string `mapstructure:"destroy_home_policy"`
	// DestroyArchiveDir is the host-level directory destroy writes home
	// archives into (one <agent-id>-<UTC timestamp>.tar.gz per destroy,
	// containing everything userdel -rf is about to delete). The daemon
	// (root) creates it on demand as 0700. Empty string keeps the
	// documented default below: an empty value NEVER disarms the archive —
	// destroy_home_policy: purge is the documented way to opt out.
	// Env override: BUNKER_DESTROY_ARCHIVE_DIR (empty = unset).
	DestroyArchiveDir string `mapstructure:"destroy_archive_dir"`
}

// Destroy-home policy values accepted by AgentConfig.DestroyHomePolicy.
const (
	// DestroyPolicyArchive archives an agent's home before the recursive
	// delete and only deletes once the archive is verified. Default.
	DestroyPolicyArchive = "archive"
	// DestroyPolicyPurge deletes the home with the user (legacy behavior).
	DestroyPolicyPurge = "purge"
)

// DefaultDestroyArchiveDir is where agent home archives are written when no
// archive directory is configured. It follows the other /var/backups-style
// host data directories: root-owned, created on demand.
const DefaultDestroyArchiveDir = "/var/backups/bunker"

// DestroyHomePolicyOrDefault returns the effective destroy-home policy.
// Anything other than an explicit "purge" resolves to "archive": the archive
// is the recoverable direction, so a typo, an empty value or a config file
// written before DF-BUNKER-33 can never silently restore the destructive
// default.
func (a *AgentConfig) DestroyHomePolicyOrDefault() string {
	if strings.EqualFold(strings.TrimSpace(a.DestroyHomePolicy), DestroyPolicyPurge) {
		return DestroyPolicyPurge
	}
	return DestroyPolicyArchive
}

// DestroyArchiveDirOrDefault returns the directory home archives are written
// into, falling back to DefaultDestroyArchiveDir for an empty value (see the
// field comment: an empty value never disarms the archive).
func (a *AgentConfig) DestroyArchiveDirOrDefault() string {
	if dir := strings.TrimSpace(a.DestroyArchiveDir); dir != "" {
		return dir
	}
	return DefaultDestroyArchiveDir
}

// IsolationConfig is the GAP-075 isolation policy. Every agent runs with an
// ENFORCED private /tmp (a per-session pam_namespace instance for SSH
// sessions, PrivateTmp=yes for every transient systemd unit). The ONLY
// sanctioned cross-agent exchange point is the shared scratch directory,
// which is group/setgid and bounded per agent.
//
// The private-/tmp half has no toggle: an isolation boundary that can be
// switched off silently is not a boundary. The knobs below say WHERE the
// exchange point lives and HOW MUCH each agent may leave there.
type IsolationConfig struct {
	// AgentGroup is the isolation group every agent joins. It has two jobs:
	// the fail-closed pam_exec precondition requires the membership of every
	// agent session it verifies (ordinary operator sessions are scoped out by
	// name and never reach it), and it owns the shared-scratch tree so peers
	// can read each other's exchanged files. Membership is granted to every
	// agent at spawn whether or not SharedScratchEnabled is set — the group is
	// the isolation identity, not a feature toggle. Default bunker-agents.
	AgentGroup string `mapstructure:"agent_group"`
	// SharedScratchEnabled gates the cross-agent exchange directory. When
	// false, agents keep their private /tmp and simply have no sanctioned
	// way to hand files to one another.
	SharedScratchEnabled bool `mapstructure:"shared_scratch_enabled"`
	// SharedScratchRoot is the exchange directory (root-owned, setgid,
	// group-visible). Default /srv/bunker-share.
	SharedScratchRoot string `mapstructure:"shared_scratch_root"`
	// SharedScratchGroup is accepted as a legacy alias for AgentGroup: the
	// first GAP-075 revision called the (single) group "shared_scratch_group".
	// Prefer agent_group.
	SharedScratchGroup string `mapstructure:"shared_scratch_group"`
	// SharedScratchPerAgentBytes caps ONE agent's scratch directory
	// (kernel-enforced tmpfs size). A scratch that cannot be mounted at the
	// cap is not created at all — the bound is never skipped.
	SharedScratchPerAgentBytes uint64 `mapstructure:"shared_scratch_per_agent_bytes"`
	// PrivateTmpRoot is the pam_namespace instance parent for /tmp. One
	// instance directory per agent holds that agent's private /tmp.
	PrivateTmpRoot string `mapstructure:"private_tmp_root"`
}

// Defaults fills unset isolation fields with the values hostsetup provisions
// and hostsetup.DefaultOptions() expects. Kept in one place so config,
// installer and daemon can never disagree about where the boundary lives.
func (c *IsolationConfig) Defaults() {
	if c.AgentGroup == "" {
		// Legacy alias: an explicit shared_scratch_group wins over the
		// default so an existing deployment keeps its group name.
		if c.SharedScratchGroup != "" {
			c.AgentGroup = c.SharedScratchGroup
		} else {
			c.AgentGroup = hostsetup.DefaultAgentGroup
		}
	}
	if c.SharedScratchGroup == "" {
		c.SharedScratchGroup = c.AgentGroup
	}
	if c.SharedScratchRoot == "" {
		c.SharedScratchRoot = hostsetup.DefaultScratchRoot
	}
	if c.SharedScratchPerAgentBytes == 0 {
		c.SharedScratchPerAgentBytes = hostsetup.DefaultScratchMaxBytes
	}
	if c.PrivateTmpRoot == "" {
		c.PrivateTmpRoot = hostsetup.DefaultTmpInstanceRoot
	}
}

// RegistryConfig is the GAP-070 durable agent lifecycle registry policy: an
// append-only JSONL event log (spawn/heartbeat/destroy) that bunkerd replays
// at startup so agent state survives a daemon restart.
type RegistryConfig struct {
	// Enabled gates durable persistence. When false the daemon behaves
	// exactly as before GAP-070 (in-memory tracker only) and `bunker
	// registry compact` has nothing to compact.
	Enabled bool `mapstructure:"enabled"`
	// Path is the active registry file (mode 0600). Rotated backups are
	// Path.1 … Path.<max_backups>.
	Path string `mapstructure:"path"`
	// MaxBytes caps the active file before it is rotated.
	MaxBytes int64 `mapstructure:"max_bytes"`
	// MaxBackups is the number of rotated files retained.
	MaxBackups int `mapstructure:"max_backups"`
	// KnownIDCap bounds the destroyed-agent ID index that compaction
	// persists (newest kept), which is what keeps a repeated destroy
	// idempotent after compaction.
	KnownIDCap int `mapstructure:"known_id_cap"`
}

// ReconciliationConfig controls what bunkerd does at startup with agents
// found in one store but not the other.
type ReconciliationConfig struct {
	// Mode is "destroy" (default) or "adopt".
	//
	//   destroy — a bunker-* system user that the registry does not know is
	//             removed from the host.
	//   adopt   — such an orphan is re-registered in the tracker and its
	//             exact persisted port reservation is restored, so a later
	//             spawn cannot double-allocate the same ports.
	Mode string `mapstructure:"mode"`
}

// ReconcileModes are the accepted reconciliation.mode values.
const (
	ReconcileModeDestroy = "destroy"
	ReconcileModeAdopt   = "adopt"
)

// Registry defaults. Kept in sync with internal/registry's documented
// defaults by TestRegistryConfigDefaultsMatchRegistryPackage.
const (
	// DefaultRegistryPath is the production registry file.
	DefaultRegistryPath = "/var/lib/bunkerd/agents.jsonl"
	// DefaultRegistryMaxBytes caps the active file at 5 MiB.
	DefaultRegistryMaxBytes int64 = 5 << 20
	// DefaultRegistryMaxBackups is the number of rotated files kept.
	DefaultRegistryMaxBackups = 3
	// DefaultRegistryKnownIDCap bounds the persisted destroyed-ID index.
	DefaultRegistryKnownIDCap = 10000
)

// ImageSpecConfig is server policy for the GAP-064 per-agent image
// customization feature: where customized images are cached and how long a
// single rootless build may take. The validation grammar itself (what a spec
// may contain) is code, not config — see internal/imagespec.
type ImageSpecConfig struct {
	// Enabled gates the feature; when false a spawn carrying an image_spec is
	// rejected with CodeInvalidArgument before any side effect.
	Enabled bool `mapstructure:"enabled"`
	// CacheDir is where canonicalized spec builds are cached on the server
	// (one directory per spec cache key).
	CacheDir string `mapstructure:"cache_dir"`
	// BuildTimeout bounds a single rootless image build.
	BuildTimeout time.Duration `mapstructure:"build_timeout"`
}

// TunnelConfig holds Cloudflare TryCloudflare tunnel settings.
type TunnelConfig struct {
	Enabled        bool          `mapstructure:"enabled"`
	BinaryPath     string        `mapstructure:"binary_path"`
	TunnelPort     uint32        `mapstructure:"tunnel_port"`
	NoAutoupdate   bool          `mapstructure:"no_autoupdate"`
	StartupTimeout time.Duration `mapstructure:"startup_timeout"`
}

// NamedTunnelConfig holds Cloudflare named tunnel settings for custom domain routing.
type NamedTunnelConfig struct {
	Enabled         bool   `mapstructure:"enabled"`
	Name            string `mapstructure:"name"`
	CredentialsFile string `mapstructure:"credentials_file"`
	Domain          string `mapstructure:"domain"`
}

// TailscaleConfig holds Tailscale mesh networking settings for per-agent tailnet IPs.
type TailscaleConfig struct {
	Enabled        bool          `mapstructure:"enabled"`
	BinaryPath     string        `mapstructure:"binary_path"`
	AuthKey        string        `mapstructure:"authkey"`
	StartupTimeout time.Duration `mapstructure:"startup_timeout"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			GRPCAddr:       ":9090",
			RESTAddr:       ":8080",
			RequestTimeout: 300 * time.Second,
		},
		TLS: TLSConfig{
			Enabled:    false,
			SelfSigned: false,
			MTLS:       false,
			CAFile:     "",
			VerifyCN:   "",
			Hosts:      []string{"localhost"},
		},
		Auth: AuthConfig{
			Enabled:   true,
			Token:     "",
			JWTSecret: "",
			JWTTTL:    6 * time.Hour,
		},
		Agent: AgentConfig{
			BaseDataDir:                "/var/lib/bunkerd",
			SSHDir:                     "/etc/bunkerd/ssh",
			PortRangeStart:             10000,
			PortRangeEnd:               19999,
			PortRangePerAgent:          100,
			MaxAgents:                  100,
			DefaultCPUQuota:            2.0,
			DefaultMemoryBytes:         4 * 1024 * 1024 * 1024,  // 4 GiB
			DefaultDiskBytes:           20 * 1024 * 1024 * 1024, // 20 GiB
			DefaultMaxProcesses:        4096,
			DefaultMaxOpenFiles:        65536,
			DefaultMaxDockerContainers: 10,
			DefaultTTL:                 6 * time.Hour,
			ImageSpec: ImageSpecConfig{
				Enabled:      true,
				CacheDir:     "/var/cache/bunkerd/imagespec",
				BuildTimeout: 20 * time.Minute,
			},
			// GAP-091: cache downloaded rootless installers under /var/cache
			// so a fresh-agent spawn no longer depends on get.docker.com
			// throughput. Root-owned mode 0755, one ~93MB file; setting
			// rootless_installer_cache_dir: "" explicitly opts out.
			RootlessInstallerCacheDir: "/var/cache/bunker/rootless-installer",
			// GAP-070: durable registry is ON by default so a daemon
			// restart never forgets its agents. Reconciliation defaults to
			// destroy — an unmanaged bunker-* user is a leftover, not an
			// agent the operator asked to keep.
			Registry: RegistryConfig{
				Enabled:    true,
				Path:       "/var/lib/bunkerd/agents.jsonl",
				MaxBytes:   5 << 20, // 5 MiB
				MaxBackups: 3,
				KnownIDCap: 10000,
			},
			Reconciliation: ReconciliationConfig{
				Mode: ReconcileModeDestroy,
			},
			// DF-BUNKER-33: destroy archives the agent home BEFORE the
			// recursive delete, so a TTL expiry can never again destroy
			// cloned repos (or any other host-owned data an agent held)
			// with no way back. destroy_home_policy: purge restores the
			// historical userdel-only behavior.
			DestroyHomePolicy: DestroyPolicyArchive,
			DestroyArchiveDir: DefaultDestroyArchiveDir,
			// GAP-075: the exchange point is ON by default (agents need a
			// sanctioned way to exchange artifacts) and every directory in it
			// is size-capped, so "on" never means "unbounded".
			Isolation: IsolationConfig{
				AgentGroup:                 hostsetup.DefaultAgentGroup,
				SharedScratchEnabled:       true,
				SharedScratchRoot:          hostsetup.DefaultScratchRoot,
				SharedScratchGroup:         hostsetup.DefaultAgentGroup,
				SharedScratchPerAgentBytes: hostsetup.DefaultScratchMaxBytes,
				PrivateTmpRoot:             hostsetup.DefaultTmpInstanceRoot,
			},
		},
		Tunnel: TunnelConfig{
			Enabled:        true,
			BinaryPath:     "cloudflared",
			TunnelPort:     8080,
			NoAutoupdate:   true,
			StartupTimeout: 30 * time.Second,
		},
		NamedTunnel: NamedTunnelConfig{
			Enabled: false,
		},
		Tailscale: TailscaleConfig{
			Enabled:        false,
			BinaryPath:     "tailscale",
			StartupTimeout: 30 * time.Second,
		},
		Audit: AuditConfig{
			Enabled: true,
			Path:    "/var/log/bunkerd/audit.log",
		},
		// GAP-067: containment disclosure is hidden by default. Enabling it
		// changes observable behavior (env var + probe marker), so it must
		// never be on unless the operator asked for it.
		Containment: ContainmentConfig{
			Disclosure: false,
		},
	}
}

// Load reads config from the specified path, with env var overrides.
// Config keys are mapped to env vars as BUNKERD_SERVER_GRPC_ADDR, etc.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	// Env var mapping: BUNKERD_ prefix, nested with _
	v.SetEnvPrefix("BUNKERD")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Bind specific env vars to config keys
	v.BindEnv("server.grpc_addr")
	v.BindEnv("server.rest_addr")
	v.BindEnv("server.request_timeout")
	v.BindEnv("tls.enabled")
	v.BindEnv("tls.cert_file")
	v.BindEnv("tls.key_file")
	v.BindEnv("tls.auto_tls")
	v.BindEnv("tls.self_signed")
	v.BindEnv("tls.domain")
	v.BindEnv("tls.mtls")
	v.BindEnv("tls.ca_file")
	v.BindEnv("tls.verify_cn")
	v.BindEnv("tls.insecure_dev")
	v.BindEnv("auth.enabled")
	v.BindEnv("auth.token")
	v.BindEnv("auth.jwt_secret")
	v.BindEnv("auth.jwt_ttl")
	// GAP-129 *_FILE indirection. The config-file paths are bound as
	// BUNKERD_AUTH_TOKEN_FILE / BUNKERD_AUTH_JWT_SECRET_FILE; the higher
	// precedence BUNKER_*_FILE vars are read directly in resolveSecret.
	v.BindEnv("auth.token_file")
	v.BindEnv("auth.jwt_secret_file")
	v.BindEnv("agent.base_data_dir")
	v.BindEnv("agent.ssh_dir")
	v.BindEnv("agent.port_range_start")
	v.BindEnv("agent.port_range_end")
	v.BindEnv("agent.port_range_per_agent")
	v.BindEnv("agent.max_agents")
	v.BindEnv("agent.default_cpu_quota")
	v.BindEnv("agent.default_memory_bytes")
	v.BindEnv("agent.default_max_processes")
	v.BindEnv("agent.default_max_open_files")
	v.BindEnv("agent.default_disk_bytes")
	v.BindEnv("agent.default_max_docker_containers")
	v.BindEnv("agent.default_ttl")
	v.BindEnv("agent.rootless_installer_cache_dir")
	v.BindEnv("agent.registry.enabled")
	v.BindEnv("agent.registry.path")
	v.BindEnv("agent.registry.max_bytes")
	v.BindEnv("agent.registry.max_backups")
	v.BindEnv("agent.registry.known_id_cap")
	v.BindEnv("agent.reconciliation.mode")
	v.BindEnv("agent.destroy_home_policy")
	v.BindEnv("agent.destroy_archive_dir")
	v.BindEnv("agent.isolation.agent_group")
	v.BindEnv("agent.isolation.shared_scratch_enabled")
	v.BindEnv("agent.isolation.shared_scratch_root")
	v.BindEnv("agent.isolation.shared_scratch_group")
	v.BindEnv("agent.isolation.shared_scratch_per_agent_bytes")
	v.BindEnv("agent.isolation.private_tmp_root")
	v.BindEnv("tunnel.enabled")
	v.BindEnv("tunnel.binary_path")
	v.BindEnv("tunnel.tunnel_port")
	v.BindEnv("tunnel.no_autoupdate")
	v.BindEnv("tunnel.startup_timeout")
	v.BindEnv("named_tunnel.enabled")
	v.BindEnv("named_tunnel.name")
	v.BindEnv("named_tunnel.credentials_file")
	v.BindEnv("named_tunnel.domain")
	v.BindEnv("tailscale.enabled")
	v.BindEnv("tailscale.binary_path")
	v.BindEnv("tailscale.authkey")
	v.BindEnv("tailscale.startup_timeout")
	v.BindEnv("audit.enabled")
	v.BindEnv("audit.path")
	v.BindEnv("audit.ship_to")
	v.BindEnv("audit.seal_key")
	v.BindEnv("containment.disclosure")

	// Read config file if it exists
	if _, err := os.Stat(path); err == nil {
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// GAP-129: resolve the control-plane credentials from their *_FILE
	// sources BEFORE anything can gate on them (CheckAuth, server.New, the
	// apikey manager). A path that was set but unreadable fails here, i.e.
	// before any listener binds — never as a silently weaker credential.
	if err := cfg.ResolveSecrets(); err != nil {
		return nil, fmt.Errorf("resolve secrets: %w", err)
	}

	return cfg, nil
}

// Validate checks that the configuration is usable.
func (c *Config) Validate() error {
	if c.Server.GRPCAddr == "" {
		return fmt.Errorf("server.grpc_addr is required")
	}
	if c.TLS.Enabled {
		if c.TLS.AutoTLS {
			if c.TLS.Domain == "" {
				return fmt.Errorf("tls.domain is required when auto_tls is enabled")
			}
		} else if c.TLS.SelfSigned {
			if c.TLS.CertFile == "" {
				c.TLS.CertFile = "/etc/bunkerd/tls/cert.pem"
			}
			if c.TLS.KeyFile == "" {
				c.TLS.KeyFile = "/etc/bunkerd/tls/key.pem"
			}
		} else {
			if c.TLS.CertFile == "" {
				return fmt.Errorf("tls.cert_file is required when TLS is enabled without auto_tls or self_signed")
			}
			if c.TLS.KeyFile == "" {
				return fmt.Errorf("tls.key_file is required when TLS is enabled without auto_tls or self_signed")
			}
		}
		if c.TLS.MTLS && c.TLS.CAFile == "" {
			return fmt.Errorf("tls.ca_file is required when mtls is enabled")
		}
	}
	switch c.Agent.Registry.Defaults(); c.Agent.Registry.Enabled {
	case true:
		if c.Agent.Registry.Path == "" {
			return fmt.Errorf("agent.registry.path is required when the registry is enabled")
		}
		if c.Agent.Registry.MaxBytes <= 0 {
			return fmt.Errorf("agent.registry.max_bytes must be > 0")
		}
		if c.Agent.Registry.MaxBackups <= 0 {
			return fmt.Errorf("agent.registry.max_backups must be > 0")
		}
	}
	if err := c.Agent.Reconciliation.Validate(); err != nil {
		return err
	}
	return nil
}

// Defaults fills in zero-valued registry settings so a config file that only
// sets a path still gets the documented caps.
func (r *RegistryConfig) Defaults() {
	if r.Path == "" {
		r.Path = DefaultRegistryPath
	}
	if r.MaxBytes <= 0 {
		r.MaxBytes = DefaultRegistryMaxBytes
	}
	if r.MaxBackups <= 0 {
		r.MaxBackups = DefaultRegistryMaxBackups
	}
	if r.KnownIDCap <= 0 {
		r.KnownIDCap = DefaultRegistryKnownIDCap
	}
}

// Validate normalises and checks the reconciliation mode. An empty mode means
// the documented default (destroy).
func (r *ReconciliationConfig) Validate() error {
	if r.Mode == "" {
		r.Mode = ReconcileModeDestroy
	}
	switch r.Mode {
	case ReconcileModeDestroy, ReconcileModeAdopt:
		return nil
	default:
		return fmt.Errorf("agent.reconciliation.mode must be %q or %q, got %q",
			ReconcileModeDestroy, ReconcileModeAdopt, r.Mode)
	}
}

// ModeOrDestroy returns the effective reconciliation mode.
func (r *ReconciliationConfig) ModeOrDestroy() string {
	if r.Mode == "" {
		return ReconcileModeDestroy
	}
	return r.Mode
}

// readSecretFile reads a secret from path, trimming surrounding whitespace
// (a trailing newline is the normal shape of an operator-written or
// `openssl rand -hex 32 > file` secret) and rejecting an empty file: an empty
// secret file is a misconfiguration, and silently resolving it to "" would
// hand the caller a credential-less daemon that looks configured.
func readSecretFile(label, path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied config/env, not untrusted input
	if err != nil {
		return "", fmt.Errorf("%s: read secret file %q: %w", label, path, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s: secret file %q is empty — write the secret into it (mode 0600) or unset the path", label, path)
	}
	return s, nil
}

// resolveSecret returns the effective value of one credential under the
// documented precedence (inline < config-file path < env-file path).
//
// It reads files and no mutable state, so it is safe to call from Load as
// well as from the explicit ResolveSecrets entry point.
//
//   - inline     — value from the config file / BUNKERD_* env var.
//   - configPath — auth.token_file / auth.jwt_secret_file.
//   - envPath    — BUNKER_AUTH_TOKEN_FILE / BUNKER_AUTH_JWT_SECRET_FILE.
//
// A path that is set but unreadable is an error at every level, including the
// env level: falling back to a weaker source after the operator explicitly
// pointed at a file is how a daemon ends up authenticating with a secret the
// operator no longer intends to use.
func resolveSecret(label, inline, configPath, envPath string) (string, error) {
	if p := strings.TrimSpace(envPath); p != "" {
		return readSecretFile(label, p)
	}
	if p := strings.TrimSpace(configPath); p != "" {
		return readSecretFile(label, p)
	}
	return inline, nil
}

// ResolveSecrets resolves auth.token and auth.jwt_secret from their inline
// values, the *_file config paths, and the BUNKER_AUTH_*_FILE env vars, in
// that precedence order. It mutates the receiver so every later consumer
// (CheckAuth, server.New, the apikey manager) sees the resolved values.
//
// It is called automatically by Load, so a caller that goes through Load
// needs no extra step; it is exported (and idempotent) for callers that build
// a Config by hand — notably tests. Resolving twice is a no-op: the second
// call re-reads the same file into the same field.
func (a *AuthConfig) ResolveSecrets() error {
	token, err := resolveSecret("auth.token", a.Token, a.TokenFile, os.Getenv(AuthTokenFileEnv))
	if err != nil {
		return err
	}
	a.Token = token

	secret, err := resolveSecret("auth.jwt_secret", a.JWTSecret, a.JWTSecretFile, os.Getenv(AuthJWTSecretFileEnv))
	if err != nil {
		return err
	}
	a.JWTSecret = secret
	return nil
}

// ResolveSecrets resolves every credential-bearing field of the config.
// Errors are returned before any listener binds (fail-before-listen).
func (c *Config) ResolveSecrets() error {
	return c.Auth.ResolveSecrets()
}

// EnsureJWTSecret makes auth.jwt_secret available without ever rotating a
// secret that already exists:
//
//  1. a secret from any configured source (inline / *_FILE / env-file) is
//     used as-is — nothing is written;
//  2. otherwise <secrets-dir>/jwt_secret is loaded if it exists (this is what
//     keeps API keys and issued JWTs valid across restarts, and across ticks
//     that re-run the binary);
//  3. only when NO secret exists anywhere is one generated (32 crypto-random
//     bytes, hex), persisted to <secrets-dir>/jwt_secret with mode 0600 (dir
//     0700), read back, and reported in the returned message.
//
// Generation is additionally gated on a credential being present: jwt_secret
// is consumed as an apikey-manager seed only when auth is enabled and a
// static token exists (server.go / service.go), so minting one for an
// auth-disabled or token-less config would persist a secret nothing reads.
// The disabled case returns a "" message and no error — CheckAuth owns the
// refusal.
//
// The returned string is a human-readable notice an operator would want to
// see at boot ("" when nothing happened); each notice names its node and
// reason so a dead record is visible rather than silent.
func (c *Config) EnsureJWTSecret() (string, error) {
	dir := SecretsDirOrDefault()
	path := filepath.Join(dir, JWTSecretFileName)

	if c.Auth.JWTSecret != "" {
		// Source 1: a configured secret. Never rotate it — the apikey
		// manager derives every issued agent key from it.
		if _, err := os.Stat(path); err == nil {
			// A persisted secret exists but the config supplies a
			// different one: the persisted file is inert input, and an
			// operator reading it later would be misled about which key is
			// live. Say so rather than silently ignoring the file.
			if persisted, rerr := readSecretFile("auth.jwt_secret", path); rerr == nil && persisted != c.Auth.JWTSecret {
				return fmt.Sprintf("bunkerd: auth.jwt_secret is configured (%s) and %s exists with a DIFFERENT value — using the configured secret; the persisted file is not in use", c.secretSourceLabel(), path), nil
			}
		}
		return "", nil
	}

	if !c.Auth.Enabled || c.Auth.Token == "" {
		// Nothing consumes a jwt secret here (no apikey manager is built
		// without a token), so do not create one.
		return "", nil
	}

	// Source 2: an already-persisted secret. Signature continuity: existing
	// API keys were derived from this value, so it must be reused verbatim.
	if persisted, err := readSecretFile("auth.jwt_secret", path); err == nil {
		c.Auth.JWTSecret = persisted
		// Record where it came from so CheckAuth does not misreport a
		// file-backed secret as legacy inline storage (and so an operator
		// reading the effective config sees the real location).
		c.Auth.JWTSecretFile = path
		return fmt.Sprintf("bunkerd: loaded auth.jwt_secret from %s", path), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// Present but unreadable/empty: do NOT generate a replacement over
		// it — that would rotate a live secret out from under issued keys.
		return "", fmt.Errorf("refusing to start: %s exists but cannot be read (%w) — fix its permissions (mode 0600, owner-readable) or remove it to have a new secret generated", path, err)
	}

	// Source 3: first boot — generate, persist, load back.
	secret, err := generateJWTSecret()
	if err != nil {
		return "", err
	}
	if err := writeSecretFile(dir, path, secret); err != nil {
		return "", fmt.Errorf("persist generated jwt_secret: %w", err)
	}
	persisted, err := readSecretFile("auth.jwt_secret", path)
	if err != nil {
		return "", fmt.Errorf("reload generated jwt_secret: %w", err)
	}
	if persisted != secret {
		return "", fmt.Errorf("persisted jwt_secret at %s does not match the generated value — refusing to start with an unknown signing key", path)
	}
	c.Auth.JWTSecret = persisted
	c.Auth.JWTSecretFile = path
	return fmt.Sprintf("bunkerd: *** GENERATED a new auth.jwt_secret *** (no secret was configured) and persisted it to %s (mode 0600, dir %s mode 0700) — existing agent API keys remain valid until this file is replaced", path, dir), nil
}

// secretSourceLabel names where the configured jwt_secret came from, for the
// warning paths. The value itself is never included.
func (c *Config) secretSourceLabel() string {
	switch {
	case strings.TrimSpace(os.Getenv(AuthJWTSecretFileEnv)) != "":
		return AuthJWTSecretFileEnv + " file"
	case strings.TrimSpace(c.Auth.JWTSecretFile) != "":
		return "auth.jwt_secret_file " + c.Auth.JWTSecretFile
	default:
		return "auth.jwt_secret in the config file"
	}
}

// generateJWTSecret returns GeneratedJWTSecretBytes crypto-random bytes, hex
// encoded (no dashes, no base64 padding — it round-trips through a file, an
// env var and a YAML scalar without quoting surprises).
func generateJWTSecret() (string, error) {
	buf := make([]byte, GeneratedJWTSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate jwt_secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// writeSecretFile creates dir (mode 0700) and writes value to dir/name with
// mode 0600, writing to a temporary file in the same directory and renaming
// it into place so a crash mid-write can never leave a truncated secret that
// would later be loaded as a short signing key. The temporary file is created
// 0600 before any secret byte is written, so the value is never briefly
// world-readable.
func writeSecretFile(dir, path, value string) error {
	if err := os.MkdirAll(dir, SecretsDirMode); err != nil {
		return fmt.Errorf("create secrets dir %s: %w", dir, err)
	}
	// MkdirAll does not tighten an existing directory (and a pre-existing
	// 0755 dir would otherwise keep advertising secrets as readable). Bring
	// it to 0700 and fail loudly if that is not possible.
	if err := os.Chmod(dir, SecretsDirMode); err != nil {
		return fmt.Errorf("set secrets dir %s mode %#o: %w", dir, SecretsDirMode, err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, SecretsFileMode)
	if err != nil {
		return fmt.Errorf("create temp secret file %s: %w", tmp, err)
	}
	if _, err := f.WriteString(value + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp secret file %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp secret file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// CheckAuth is the startup authentication gate. It returns a non-empty
// warning when authentication is explicitly disabled, and an error when
// authentication is enabled but no credential (static token or JWT secret)
// is configured — the daemon must refuse to start rather than silently
// run unauthenticated.
//
// It also emits the GAP-129 legacy-storage warning when a credential is
// still inline in the config file: inline works, but a config copy, backup
// or `bunker status` reader carries the credential with it, which is exactly
// what the *_FILE indirection exists to avoid.
//
// Secrets must already be resolved (Load does this automatically); a caller
// that hand-builds a Config should call ResolveSecrets first so the file
// sources are counted here.
func (c *Config) CheckAuth() (string, error) {
	if !c.Auth.Enabled {
		return "bunkerd: *** WARNING: AUTH DISABLED *** — running WITHOUT authentication; any client that can reach this server can spawn/destroy agents. Set auth.enabled: true and auth.token in the config file to enable authentication.", nil
	}
	if c.Auth.Token == "" && c.Auth.JWTSecret == "" {
		return "", fmt.Errorf("auth.enabled is true but neither auth.token nor auth.jwt_secret is set — set one in the config file, or explicitly set auth.enabled: false to run without authentication")
	}
	if inline := c.inlineSecrets(); len(inline) > 0 {
		return fmt.Sprintf("bunkerd: *** WARNING: legacy secret storage *** — %s is set inline in the config file; the config file is copied, backed up and read by tooling, so the credential travels with it. Move it to a file: %s=\"<path>\" (file mode 0600, e.g. under %s) or auth.token_file/auth.jwt_secret_file. Precedence: inline < file path < env-file.",
			strings.Join(inline, ", "), AuthTokenFileEnv, SecretsDirOrDefault()), nil
	}
	return "", nil
}

// inlineSecrets names the credential fields that are configured inline
// (neither overridden by a config *_file path nor by an env-file). Only the
// field NAMES are returned — never a value.
//
// Configuration is read as "inline" when the field is non-empty, regardless
// of whether a *_file path also resolved it: ResolveSecrets overwrites the
// inline field with the file's value, so a file-backed config looks
// identical here. The source env/config path is re-checked to tell them
// apart.
func (c *Config) inlineSecrets() []string {
	var out []string
	if c.Auth.Token != "" && strings.TrimSpace(os.Getenv(AuthTokenFileEnv)) == "" && strings.TrimSpace(c.Auth.TokenFile) == "" {
		out = append(out, "auth.token")
	}
	if c.Auth.JWTSecret != "" && strings.TrimSpace(os.Getenv(AuthJWTSecretFileEnv)) == "" && strings.TrimSpace(c.Auth.JWTSecretFile) == "" {
		out = append(out, "auth.jwt_secret")
	}
	return out
}

// CheckTLS is the startup transport gate (GAP-126 / REQ-T1). It mirrors
// CheckAuth: it returns a non-empty warning when the daemon is about to serve
// plaintext on a NON-loopback listener under the explicit tls.insecure_dev
// opt-in, and an error — the daemon must refuse to start — when a non-loopback
// listener would carry plaintext without that opt-in.
//
// The device is the one an evaluation fails on: an admin-capable RPC plane in
// cleartext. Loopback-only binds stay allowed silently (they are not reachable
// off-host), and TLS on makes the gate a no-op.
//
// grpcAddr/restAddr are the effective listen addresses; the daemon passes the
// configured server.grpc_addr / server.rest_addr so the gate checks exactly
// what Run is about to bind. Callers must treat a nil error with a non-empty
// warning as "started, but loudly insecure".
func (c *Config) CheckTLS(grpcAddr, restAddr string) (string, error) {
	if c.TLS.Enabled {
		return "", nil
	}

	// REST is optional: an empty rest_addr (or one aliasing the gRPC address,
	// which Run skips as a duplicate listener) is not a second bind.
	addrs := []string{grpcAddr}
	if restAddr != "" && restAddr != grpcAddr {
		addrs = append(addrs, restAddr)
	}

	nonLoopback := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if ok, _ := IsLoopbackAddr(addr); !ok {
			nonLoopback = append(nonLoopback, addr)
		}
	}
	if len(nonLoopback) == 0 {
		return "", nil
	}

	if !c.TLS.InsecureDev {
		return "", fmt.Errorf("refusing to bind non-loopback plaintext listener %s: "+
			"set tls.enabled: true to serve TLS, or explicitly set tls.insecure_dev: true "+
			"to run WITHOUT TLS on a reachable address (loudly warned and marked in the audit trail)",
			strings.Join(nonLoopback, ", "))
	}

	return fmt.Sprintf("%s *** INSECURE: serving plaintext on non-loopback listener %s "+
		"(tls.enabled is false and tls.insecure_dev is true). Any client that can reach this "+
		"address controls the RPC plane; every audit record is marked %s. Disable tls.insecure_dev "+
		"and enable TLS for anything reachable beyond this host. ***",
		"bunkerd:", strings.Join(nonLoopback, ", "), InsecurePlaintextMarker), nil
}

// IsLoopbackAddr reports whether a listener address string binds only the
// loopback interface. It is the classification CheckTLS decides on, exported so
// the daemon and its tests share one definition.
//
// Rules, in order:
//   - "" is not a bind at all: reported as NOT loopback (fail closed).
//   - the host part is taken with net.SplitHostPort; when the string does not
//     parse as host:port (e.g. a bare "8080") the whole string is used as the
//     host, so a bare numeric port is treated as an address, not a pass.
//   - an empty or wildcard host (":8080", "0.0.0.0:8080", "[::]:8080") means
//     EVERY interface, so it is NON-loopback by definition.
//   - "localhost" and any host whose IP is in 127.0.0.0/8 or is ::1 are
//     loopback.
//   - anything else (a routable IP, a hostname) is non-loopback: an unknown
//     name is refused rather than trusted.
//
// The returned string is the resolved host when one was determined, for error
// messages.
func IsLoopbackAddr(addr string) (bool, string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false, ""
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	// Empty host (":8080") or an explicit wildcard binds every interface.
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return false, host
	}
	if strings.EqualFold(host, "localhost") {
		return true, host
	}
	// Bracketed literals like "[::1]" only survive the fallback path; strip
	// them before parsing.
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false, host // a name we cannot resolve: non-loopback (fail closed)
	}
	return ip.IsLoopback(), host
}

// InsecurePlaintextActive reports whether this configuration will serve
// plaintext on a NON-loopback listener — i.e. TLS is disabled, the explicit
// tls.insecure_dev opt-in is set, and at least one configured listener binds
// beyond loopback.
//
// It is the single predicate behind the two halves of REQ-T1: the daemon uses
// it to decide whether to mark audit records, and CheckTLS is defined so that a
// true result is exactly the case in which CheckTLS returns a warning and no
// error. A loopback-only insecure_dev config is NOT "insecure plaintext" (the
// listener is not reachable off-host), and neither is a TLS config.
func (c *Config) InsecurePlaintextActive() bool {
	if c.TLS.Enabled || !c.TLS.InsecureDev {
		return false
	}
	warn, err := c.CheckTLS(c.Server.GRPCAddr, c.Server.RESTAddr)
	return err == nil && warn != ""
}
