// Package config provides configuration loading for bunkerd.
// Reads from /etc/bunkerd/config.yaml with env var overrides.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Config is the top-level bunkerd configuration.
type Config struct {
	Server ServerConfig `mapstructure:"server"`
	TLS    TLSConfig    `mapstructure:"tls"`
	Auth   AuthConfig   `mapstructure:"auth"`
	Agent  AgentConfig  `mapstructure:"agent"`
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
	MaxAgents          uint32  `mapstructure:"max_agents"`
	DefaultCPUQuota    float64 `mapstructure:"default_cpu_quota"`
	DefaultMemoryBytes uint64  `mapstructure:"default_memory_bytes"`
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
			MaxAgents:          100,
			DefaultCPUQuota:    2.0,
			DefaultMemoryBytes: 4 * 1024 * 1024 * 1024, // 4 GiB
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
	v.BindEnv("agent.max_agents")
	v.BindEnv("agent.default_cpu_quota")
	v.BindEnv("agent.default_memory_bytes")

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
