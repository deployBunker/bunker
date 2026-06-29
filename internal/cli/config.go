// Package cli provides the bunker CLI configuration, command definitions, and
// the server registry used by the `bunker connect` command.
package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/spf13/viper"
)

// CLIConfig is the on-disk configuration for the bunker CLI.
// It holds registered server entries and the currently active server.
type CLIConfig struct {
	Servers      map[string]ServerEntry `mapstructure:"servers" yaml:"servers"`
	ActiveServer string                 `mapstructure:"active_server" yaml:"active_server"`
}

// ServerEntry describes a single bunkerd server that has been registered
// via `bunker connect`.
type ServerEntry struct {
	Name        string `mapstructure:"name" yaml:"name"`
	URL         string `mapstructure:"url" yaml:"url"`
	Token       string `mapstructure:"token" yaml:"token"`
	TLSInsecure bool   `mapstructure:"tls_insecure" yaml:"tls_insecure"`
	ConnectedAt string `mapstructure:"connected_at" yaml:"connected_at"`
}

// configFilePath returns the path to ~/.bunker/config.yaml.
func configFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".bunker", "config.yaml"), nil
}

// LoadCLIConfig reads the CLI configuration from ~/.bunker/config.yaml.
// Returns a default-initialised config when the file does not exist.
func LoadCLIConfig() (*CLIConfig, error) {
	cfgPath, err := configFilePath()
	if err != nil {
		return nil, err
	}

	cfg := &CLIConfig{
		Servers: make(map[string]ServerEntry),
	}

	v := viper.New()
	v.SetConfigFile(cfgPath)
	v.SetConfigType("yaml")

	if _, err := os.Stat(cfgPath); err == nil {
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read CLI config %s: %w", cfgPath, err)
		}
		if err := v.Unmarshal(cfg); err != nil {
			return nil, fmt.Errorf("unmarshal CLI config: %w", err)
		}
	}

	if cfg.Servers == nil {
		cfg.Servers = make(map[string]ServerEntry)
	}
	return cfg, nil
}

// SaveCLIConfig writes the CLI configuration to ~/.bunker/config.yaml,
// creating the directory if needed.
func SaveCLIConfig(cfg *CLIConfig) error {
	cfgPath, err := configFilePath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	v := viper.New()
	v.SetConfigFile(cfgPath)
	v.SetConfigType("yaml")

	// Populate viper with the config values.
	v.Set("servers", cfg.Servers)
	v.Set("active_server", cfg.ActiveServer)

	if err := v.WriteConfigAs(cfgPath); err != nil {
		return fmt.Errorf("write CLI config: %w", err)
	}
	return nil
}

// ConnectServer dials a bunkerd server and returns its ServerInfo.
func ConnectServer(url, token string, tlsInsecure bool) (*v1.ServerInfoResponse, error) {
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}
	if tlsInsecure {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
	}

	client := bunkerv1connect.NewBunkerdClient(httpClient, url)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := connect.NewRequest(&v1.ServerInfoRequest{})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}

	resp, err := client.ServerInfo(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", url, err)
	}
	return resp.Msg, nil
}

// RegisterServer connects to a bunkerd server, registers it in the CLI config,
// and saves the config to disk. If name is empty, the hostname from the
// ServerInfo response is used.
func RegisterServer(name, url, token string, tlsInsecure bool) error {
	info, err := ConnectServer(url, token, tlsInsecure)
	if err != nil {
		return err
	}

	if name == "" {
		name = info.Hostname
	}
	if name == "" {
		name = url
	}

	cfg, err := LoadCLIConfig()
	if err != nil {
		return fmt.Errorf("load CLI config: %w", err)
	}

	entry := ServerEntry{
		Name:        name,
		URL:         url,
		Token:       token,
		TLSInsecure: tlsInsecure,
		ConnectedAt: time.Now().UTC().Format(time.RFC3339),
	}

	cfg.Servers[name] = entry
	// Make the first-registered server the active one if none is set.
	if cfg.ActiveServer == "" {
		cfg.ActiveServer = name
	}

	if err := SaveCLIConfig(cfg); err != nil {
		return fmt.Errorf("save CLI config: %w", err)
	}

	fmt.Printf("Connected to %s (%s)\n", info.Hostname, info.Version)
	fmt.Printf("  Agents: %d/%d\n", info.AgentCount, info.MaxAgents)
	fmt.Printf("  Uptime: %ds\n", info.UptimeSeconds)
	fmt.Printf("  Server registered as %q\n", name)
	return nil
}
