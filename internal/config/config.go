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
}

// ServerConfig holds gRPC and REST listener addresses.
type ServerConfig struct {
	GRPCAddr string `mapstructure:"grpc_addr"`
	RESTAddr string `mapstructure:"rest_addr"`
}

// TLSConfig holds TLS settings.
type TLSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
	AutoTLS  bool   `mapstructure:"auto_tls"`
	Domain   string `mapstructure:"domain"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	Token     string `mapstructure:"token"`
	JWTSecret string `mapstructure:"jwt_secret"`
}

// AgentConfig holds agent lifecycle settings.
type AgentConfig struct {
	BaseDataDir        string  `mapstructure:"base_data_dir"`
	SSHDir             string  `mapstructure:"ssh_dir"`
	PortRangeStart     uint32  `mapstructure:"port_range_start"`
	PortRangeEnd       uint32  `mapstructure:"port_range_end"`
	PortRangePerAgent  uint32  `mapstructure:"port_range_per_agent"`
	MaxAgents          uint32  `mapstructure:"max_agents"`
	DefaultCPUQuota    float64 `mapstructure:"default_cpu_quota"`
	DefaultMemoryBytes uint64  `mapstructure:"default_memory_bytes"`
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
			GRPCAddr: ":9090",
			RESTAddr: ":8080",
		},
		TLS: TLSConfig{
			Enabled: false,
		},
		Auth: AuthConfig{
			Enabled: false,
		},
		Agent: AgentConfig{
			BaseDataDir:        "/var/lib/bunkerd",
			SSHDir:             "/etc/bunkerd/ssh",
			PortRangeStart:     10000,
			PortRangeEnd:       10100,
			PortRangePerAgent:  10,
			MaxAgents:          100,
			DefaultCPUQuota:    2.0,
			DefaultMemoryBytes: 4 * 1024 * 1024 * 1024, // 4 GiB
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
	v.BindEnv("tls.enabled")
	v.BindEnv("tls.cert_file")
	v.BindEnv("tls.key_file")
	v.BindEnv("tls.auto_tls")
	v.BindEnv("tls.domain")
	v.BindEnv("auth.enabled")
	v.BindEnv("auth.token")
	v.BindEnv("auth.jwt_secret")
	v.BindEnv("agent.base_data_dir")
	v.BindEnv("agent.ssh_dir")
	v.BindEnv("agent.port_range_start")
	v.BindEnv("agent.port_range_end")
	v.BindEnv("agent.port_range_per_agent")
	v.BindEnv("agent.max_agents")
	v.BindEnv("agent.default_cpu_quota")
	v.BindEnv("agent.default_memory_bytes")
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
		if !c.TLS.AutoTLS {
			if c.TLS.CertFile == "" {
				return fmt.Errorf("tls.cert_file is required when TLS is enabled without auto_tls")
			}
			if c.TLS.KeyFile == "" {
				return fmt.Errorf("tls.key_file is required when TLS is enabled without auto_tls")
			}
		}
		if c.TLS.AutoTLS && c.TLS.Domain == "" {
			return fmt.Errorf("tls.domain is required when auto_tls is enabled")
		}
	}
	return nil
}
