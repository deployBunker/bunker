// Package config provides configuration loading for bunkerd.
// Reads from /etc/bunkerd/config.yaml with env var overrides.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
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
}

// APIKey holds a generated API key with metadata.
type APIKey struct {
	KeyID     string    `mapstructure:"key_id"`
	TokenHash string    `mapstructure:"token_hash"`
	AgentID   string    `mapstructure:"agent_id"`
	CreatedAt time.Time `mapstructure:"created_at"`
	ExpiresAt time.Time `mapstructure:"expires_at"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	Enabled   bool          `mapstructure:"enabled"`
	Token     string        `mapstructure:"token"`
	JWTSecret string        `mapstructure:"jwt_secret"`
	JWTTTL    time.Duration `mapstructure:"jwt_ttl"`
}

// AuditConfig holds the daemon-side audit trail settings. When enabled, every
// authenticated RPC appends one JSONL record to Path (mode 0600). The log
// never contains token values.
type AuditConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
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
	// Registry holds the GAP-070 durable agent registry settings.
	Registry RegistryConfig `mapstructure:"registry"`
	// Reconciliation holds the GAP-070 startup reconciliation policy.
	Reconciliation ReconciliationConfig `mapstructure:"reconciliation"`
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
	v.BindEnv("auth.enabled")
	v.BindEnv("auth.token")
	v.BindEnv("auth.jwt_secret")
	v.BindEnv("auth.jwt_ttl")
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
	v.BindEnv("agent.registry.enabled")
	v.BindEnv("agent.registry.path")
	v.BindEnv("agent.registry.max_bytes")
	v.BindEnv("agent.registry.max_backups")
	v.BindEnv("agent.registry.known_id_cap")
	v.BindEnv("agent.reconciliation.mode")
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

// CheckAuth is the startup authentication gate. It returns a non-empty
// warning when authentication is explicitly disabled, and an error when
// authentication is enabled but no credential (static token or JWT secret)
// is configured — the daemon must refuse to start rather than silently
// run unauthenticated.
func (c *Config) CheckAuth() (string, error) {
	if !c.Auth.Enabled {
		return "bunkerd: *** WARNING: AUTH DISABLED *** — running WITHOUT authentication; any client that can reach this server can spawn/destroy agents. Set auth.enabled: true and auth.token in the config file to enable authentication.", nil
	}
	if c.Auth.Token == "" && c.Auth.JWTSecret == "" {
		return "", fmt.Errorf("auth.enabled is true but neither auth.token nor auth.jwt_secret is set — set one in the config file, or explicitly set auth.enabled: false to run without authentication")
	}
	return "", nil
}
