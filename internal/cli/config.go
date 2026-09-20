// Package cli provides the bunker CLI configuration, command definitions, and
// the server registry used by the `bunker connect` command.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"go.yaml.in/yaml/v3"
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
	// TLSMode records the trust decision made at `bunker connect` time:
	// "system" (verify against the system root store), "self-signed" (verify
	// against CertPin) or "insecure" (skip verification). Empty means "no
	// explicit mode" — the historical behaviour, where a non-empty CertPin
	// still pins and TLSInsecure still skips. Optional in YAML, so configs
	// written before GAP-127 keep working unchanged.
	TLSMode string `mapstructure:"tls_mode" yaml:"tls_mode,omitempty"`
	// CertPin is the sha256 (lowercase hex) of the DER of the server's leaf
	// certificate — the trust anchor for a self-signed daemon (GAP-127). It is
	// established by trust-on-first-use on the first `bunker connect --tls
	// self-signed` and must match on every later connection. Optional in YAML.
	CertPin string `mapstructure:"cert_pin" yaml:"cert_pin,omitempty"`
	// CertPinSetAt records when the pin was established (RFC3339 UTC), for
	// auditability: an old pin that suddenly changes is the interesting case.
	CertPinSetAt string `mapstructure:"cert_pin_set_at" yaml:"cert_pin_set_at,omitempty"`
	ConnectedAt  string `mapstructure:"connected_at" yaml:"connected_at"`
}

// configFilePath returns the path to the CLI config file, resolved from the
// root --config flag, $BUNKER_HOME, or $HOME/.bunker/config.yaml (the
// historical default). See paths.go for the resolution rules.
func configFilePath() (string, error) {
	if configPathOverride != "" {
		return configPathOverride, nil
	}
	dir, err := bunkerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// LoadCLIConfig reads the CLI configuration from ~/.bunker/config.yaml.
// Returns a default-initialised config when the file does not exist.
// The config is read with direct YAML I/O (not viper) so that server-name
// map keys keep their original case: viper lowercases every map key, which
// broke lookups for mixed-case hostnames (DF-BUNKER-1).
func LoadCLIConfig() (*CLIConfig, error) {
	cfgPath, err := configFilePath()
	if err != nil {
		return nil, err
	}

	cfg := &CLIConfig{
		Servers: make(map[string]ServerEntry),
	}

	if data, err := os.ReadFile(cfgPath); err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("unmarshal CLI config %s: %w", cfgPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read CLI config %s: %w", cfgPath, err)
	}

	if cfg.Servers == nil {
		cfg.Servers = make(map[string]ServerEntry)
	}
	return cfg, nil
}

// SaveCLIConfig writes the CLI configuration to ~/.bunker/config.yaml,
// creating the directory if needed. The file is written 0600 via direct
// YAML I/O so map keys (server names) keep their original case.
func SaveCLIConfig(cfg *CLIConfig) error {
	cfgPath, err := configFilePath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal CLI config: %w", err)
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		return fmt.Errorf("write CLI config: %w", err)
	}
	return nil
}

// ConnectOptions are the inputs to a server registration. They mirror the
// `bunker connect` flags, including the GAP-127 TLS trust decision.
type ConnectOptions struct {
	// Name is the server alias; empty means "use the hostname from ServerInfo".
	Name string
	// URL is the bunkerd base URL (https:// for TLS, http:// for loopback dev).
	URL string
	// Token is the bearer token sent on registration.
	Token string
	// TLSMode is the trust decision. Empty means "leave the entry's existing
	// decision alone" (a re-connect without --tls does not silently change how
	// the entry is verified).
	TLSMode TLSMode
	// Insecure is the explicit --tls-insecure opt-out.
	Insecure bool
	// AcceptCert replaces an existing pin with the certificate observed now.
	// Without it, a changed certificate is a refusal.
	AcceptCert bool
}

// ConnectServer dials a bunkerd server and returns its ServerInfo.
// The token-bearing entry form is ConnectServerWithEntry.
func ConnectServer(url, token string, tlsInsecure bool) (*v1.ServerInfoResponse, error) {
	return ConnectServerWithEntry(ServerEntry{URL: url, Token: token, TLSInsecure: tlsInsecure})
}

// ConnectServerWithEntry dials a bunkerd server described by entry — applying
// the entry's trust decision (system roots, pinned certificate, or the explicit
// insecure opt-out) — and returns its ServerInfo. An unusable trust
// configuration is returned as an error, never downgraded to a
// verification-free dial.
func ConnectServerWithEntry(entry ServerEntry) (*v1.ServerInfoResponse, error) {
	client, err := newBunkerdClientChecked(entry, 10*time.Second)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := connect.NewRequest(&v1.ServerInfoRequest{})
	if entry.Token != "" {
		req.Header().Set("Authorization", "Bearer "+entry.Token)
	}

	resp, err := client.ServerInfo(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w%s", entry.URL, err, tlsHelpHint(err, entry.URL))
	}
	return resp.Msg, nil
}

// RegisterServer connects to a bunkerd server, registers it in the CLI config,
// and saves the config to disk. If name is empty, the hostname from the
// ServerInfo response is used.
func RegisterServer(name, url, token string, tlsInsecure bool) error {
	opts := ConnectOptions{Name: name, URL: url, Token: token, Insecure: tlsInsecure}
	if tlsInsecure {
		opts.TLSMode = TLSModeInsecure
	}
	return RegisterServerWithOptions(opts)
}

// RegisterServerWithOptions is RegisterServer with the full trust-decision
// surface. For a self-signed daemon it performs trust-on-first-use: the leaf
// certificate is observed over a real handshake, the fingerprint is printed
// loudly, and it is stored on the entry so every later command verifies against
// it (GAP-127). A changed certificate is a refusal unless AcceptCert is set.
func RegisterServerWithOptions(opts ConnectOptions) error {
	entry := ServerEntry{
		Name:        opts.Name,
		URL:         opts.URL,
		Token:       opts.Token,
		TLSInsecure: opts.Insecure,
		TLSMode:     string(opts.TLSMode),
		ConnectedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// A pin and "skip verification" are mutually exclusive decisions; refuse
	// the combination before any network activity.
	if opts.Insecure && opts.TLSMode == TLSModeSelfSigned {
		return fmt.Errorf("contradictory TLS configuration: --tls-insecure disables verification while --tls %s requires it — pass one or the other", TLSModeSelfSigned)
	}

	if opts.TLSMode == TLSModeSelfSigned || opts.AcceptCert {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(opts.URL)), "https://") {
			return fmt.Errorf("--tls %s needs an https:// server URL (got %q) — a plain-HTTP loopback daemon needs no trust decision", TLSModeSelfSigned, opts.URL)
		}
		observed, known, err := CheckServerCertificate(entry, opts.AcceptCert)
		if err != nil {
			var fetchErr *CertFetchError
			if errors.As(err, &fetchErr) && known != nil && known.CertPin != "" && isUnreachableClass(fetchErr.Class) {
				// The certificate could not be re-observed right now, but the
				// entry already carries a pin: keep it rather than dropping the
				// trust anchor on a transient network failure. The connection
				// below still has to satisfy the pin.
				fmt.Fprintf(os.Stderr, "\n!! WARNING: could not re-read the certificate of %s (%s).\n"+
					"!! Keeping the previously pinned certificate %s; the connection below must still satisfy it.\n\n",
					opts.URL, fetchErr.Class, FormatCertPin(known.CertPin))
				entry.CertPin = known.CertPin
				entry.CertPinSetAt = known.CertPinSetAt
			} else {
				return err
			}
		} else {
			entry.CertPin = observed
			entry.CertPinSetAt = time.Now().UTC().Format(time.RFC3339)
			// Make the pin usable for the registration dial itself, before it
			// is persisted: the dial must verify against what we just observed.
			StoreCertPin(opts.URL, observed)
			printTrustOnFirstUse(known, opts, observed)
		}
	}

	info, err := ConnectServerWithEntry(entry)
	if err != nil {
		return err
	}

	if entry.Name == "" {
		entry.Name = info.Hostname
	}
	if entry.Name == "" {
		entry.Name = entry.URL
	}
	entry.TLSMode = string(normalizeTLSMode(opts, entry))

	cfg, err := LoadCLIConfig()
	if err != nil {
		return fmt.Errorf("load CLI config: %w", err)
	}

	cfg.Servers[entry.Name] = entry
	// Make the first-registered server the active one if none is set.
	if cfg.ActiveServer == "" {
		cfg.ActiveServer = entry.Name
	}

	if err := SaveCLIConfig(cfg); err != nil {
		return fmt.Errorf("save CLI config: %w", err)
	}

	fmt.Printf("Connected to %s (%s)\n", info.Hostname, info.Version)
	fmt.Printf("  Agents: %d/%d\n", info.AgentCount, info.MaxAgents)
	fmt.Printf("  Uptime: %ds\n", info.UptimeSeconds)
	printTrustSummary(cfg.Servers[entry.Name])
	fmt.Printf("  Server registered as %q\n", entry.Name)
	return nil
}

// normalizeTLSMode keeps the entry's recorded mode consistent with what the
// registration actually did: an explicit mode wins, an existing pin means
// self-signed, and an insecure opt-out means insecure. A plain-HTTP URL that
// makes no TLS decision at all records NO mode, so the entry does not claim a
// trust posture it does not have.
func normalizeTLSMode(opts ConnectOptions, entry ServerEntry) TLSMode {
	switch {
	case opts.Insecure:
		return TLSModeInsecure
	case opts.TLSMode != "":
		return opts.TLSMode
	case entry.CertPin != "":
		return TLSModeSelfSigned
	case strings.HasPrefix(strings.ToLower(strings.TrimSpace(opts.URL)), "https://"):
		return TLSModeSystem
	default:
		return ""
	}
}

// printTrustOnFirstUse emits the LOUD first-use warning naming the fingerprint
// the operator is about to trust. It goes to stderr so it is visible in a
// terminal and survives any stdout capture, and it is never suppressed on a
// first use.
func printTrustOnFirstUse(known *ServerEntry, opts ConnectOptions, observed string) {
	if known != nil && known.CertPin != "" {
		return // a re-connect against an already-pinned entry is not a first use
	}
	fmt.Fprintf(os.Stderr, `
================================ TRUST ON FIRST USE ================================
You are about to trust the certificate presented by %s for the first time.

  fingerprint      %s
  stored as        cert_pin in ~/.bunker/config.yaml

There is no certificate authority behind this certificate: the trust comes from
accepting THIS fingerprint now, exactly once, like an SSH host key. Every later
connection verifies against it, and a different certificate will be refused.

Verify the fingerprint out of band if you can (on the daemon host:
  openssl x509 -in <tls.cert_file> -noout -fingerprint -sha256 ).
If you cannot, connect over a channel you already trust the first time.
====================================================================================

`, opts.URL, FormatCertPin(observed))
}

// printTrustSummary reports, on stdout, how the registered entry will verify the
// server from now on — so the security posture of every entry is visible at the
// moment it is created.
func printTrustSummary(entry ServerEntry) {
	switch {
	case entry.CertPin != "":
		fmt.Printf("  TLS: pinned certificate %s\n", FormatCertPin(entry.CertPin))
		fmt.Printf("       (a different certificate will be refused; re-pin with "+
			"'bunker connect --tls self-signed --accept-cert %s')\n", entry.URL)
	case entry.TLSInsecure:
		fmt.Printf("  TLS: VERIFICATION DISABLED (--tls-insecure) — do not use against an untrusted network\n")
	case entry.TLSMode == string(TLSModeSystem):
		fmt.Printf("  TLS: system root store (tls_mode: %s)\n", TLSModeSystem)
	}
}
