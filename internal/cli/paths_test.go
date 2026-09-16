package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
)

// setenvForTest sets or unsets an environment variable for the duration of
// the test (t.Setenv cannot unset). Restores the previous state at cleanup.
func setenvForTest(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if value == "" {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	} else if err := os.Setenv(key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// resetPathOverrides clears both override setters for the duration of the
// test so one test can never leak an override into another.
func resetPathOverrides(t *testing.T) {
	t.Helper()
	ResetConfigPathOverride()
	ResetDaemonConfigPathOverride()
	t.Cleanup(func() {
		ResetConfigPathOverride()
		ResetDaemonConfigPathOverride()
	})
}

// writeDaemonConfig writes a minimal daemon config YAML with a custom
// audit.path / agent.registry.path and returns its path.
func writeDaemonConfig(t *testing.T, auditPath, registryPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	yaml := "server:\n  grpc_addr: \":9090\"\n"
	if auditPath != "" {
		yaml += "audit:\n  enabled: true\n  path: " + auditPath + "\n"
	}
	if registryPath != "" {
		yaml += "agent:\n  registry:\n    enabled: true\n    path: " + registryPath + "\n"
	}
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestConfigFilePath_Precedence pins the CLI config path resolution rules:
// --config > BUNKER_HOME > $HOME/.bunker/config.yaml, with empty or
// whitespace-only values treated as unset. The default row is the A5
// guarantee: byte-identical to $HOME/.bunker/config.yaml.
func TestConfigFilePath_Precedence(t *testing.T) {
	home := t.TempDir()
	bhDir := t.TempDir()
	cfgFile := filepath.Join(t.TempDir(), "elsewhere", "bunker.yaml")

	tests := []struct {
		name        string
		bunkerHome  string // "" = unset the variable
		flagValue   string // "" = no override (reset)
		want        string
		wantDefault bool // want must equal $HOME/.bunker/config.yaml exactly
	}{
		{
			name:        "default is HOME/.bunker/config.yaml",
			want:        filepath.Join(home, ".bunker", "config.yaml"),
			wantDefault: true,
		},
		{
			name:       "BUNKER_HOME moves config.yaml",
			bunkerHome: bhDir,
			want:       filepath.Join(bhDir, "config.yaml"),
		},
		{
			name:       "flag wins over BUNKER_HOME",
			bunkerHome: bhDir,
			flagValue:  cfgFile,
			want:       cfgFile,
		},
		{
			name:       "blank BUNKER_HOME treated as unset",
			bunkerHome: "   ",
			want:       filepath.Join(home, ".bunker", "config.yaml"),
		},
		{
			name:      "blank flag treated as unset (falls to BUNKER_HOME)",
			flagValue: "   ",
			want:      filepath.Join(home, ".bunker", "config.yaml"),
		},
		{
			name:       "blank flag treated as unset (falls to BUNKER_HOME)",
			bunkerHome: bhDir,
			flagValue:  "   ",
			want:       filepath.Join(bhDir, "config.yaml"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			setenvForTest(t, "BUNKER_HOME", tt.bunkerHome)
			resetPathOverrides(t)
			SetConfigPathOverride(tt.flagValue)

			got, err := configFilePath()
			if err != nil {
				t.Fatalf("configFilePath: %v", err)
			}
			if got != tt.want {
				t.Errorf("configFilePath = %q, want %q", got, tt.want)
			}
			if tt.wantDefault {
				// A5: byte-identical to the historical path builder.
				if want := filepath.Join(home, ".bunker", "config.yaml"); got != want {
					t.Errorf("default changed: got %q, want byte-identical %q", got, want)
				}
			}
		})
	}
}

// TestBunkerHomeMovesKeysDir proves the rule "BUNKER_HOME moves BOTH the
// CLI config file and the key-dir derivation": defaultSSHKeyPath (cp.go)
// resolves keys/ next to configFilePath(), so the whole CLI state follows
// the override (spawn saves keys there; ssh/cp/tunnel/mount resolve them
// back). An explicit --config relocates the key dir the same way.
func TestBunkerHomeMovesKeysDir(t *testing.T) {
	home := t.TempDir()
	bhDir := t.TempDir()
	cfgFile := filepath.Join(t.TempDir(), "conf", "bunker.yaml")

	t.Setenv("HOME", home)
	resetPathOverrides(t)

	// 1. BUNKER_HOME moves both.
	setenvForTest(t, "BUNKER_HOME", bhDir)
	gotCfg, err := configFilePath()
	if err != nil {
		t.Fatalf("configFilePath: %v", err)
	}
	if want := filepath.Join(bhDir, "config.yaml"); gotCfg != want {
		t.Errorf("BUNKER_HOME config path = %q, want %q", gotCfg, want)
	}
	gotKey, err := defaultSSHKeyPath("agent1")
	if err != nil {
		t.Fatalf("defaultSSHKeyPath: %v", err)
	}
	if want := filepath.Join(bhDir, "keys", "agent1"); gotKey != want {
		t.Errorf("BUNKER_HOME key path = %q, want %q", gotKey, want)
	}

	// 2. --config relocates the key dir too (highest precedence).
	SetConfigPathOverride(cfgFile)
	gotKey, err = defaultSSHKeyPath("agent1")
	if err != nil {
		t.Fatalf("defaultSSHKeyPath: %v", err)
	}
	if want := filepath.Join(filepath.Dir(cfgFile), "keys", "agent1"); gotKey != want {
		t.Errorf("--config key path = %q, want %q", gotKey, want)
	}
}

// TestUseCommand_WritesConfigAtOverrideLocation is the COMMAND-LEVEL proof:
// it executes a real command from the cobra tree (`bunker use beta`) with
// the --config override set and proves the config was READ from and WRITTEN
// to the overridden location (use rewrites active_server on save), with
// nothing touching the real $HOME default.
func TestUseCommand_WritesConfigAtOverrideLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setenvForTest(t, "BUNKER_HOME", "") // unset
	resetPathOverrides(t)

	cfgFile := filepath.Join(t.TempDir(), "override", "bunker.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgFile), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const initial = `servers:
  alpha:
    name: alpha
    url: http://alpha:9090
    token: t1
  beta:
    name: beta
    url: https://beta.example.com
    token: t2
active_server: alpha
`
	if err := os.WriteFile(cfgFile, []byte(initial), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	SetConfigPathOverride(cfgFile)

	cmd := NewUseCommand()
	cmd.SetArgs([]string{"beta"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("use beta: %v", err)
	}

	data, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read overridden config: %v", err)
	}
	if !strings.Contains(string(data), "active_server: beta") {
		t.Errorf("override config not rewritten by `use beta` (active_server missing):\n%s", data)
	}

	// LoadCLIConfig must resolve through the same override.
	cfg, err := LoadCLIConfig()
	if err != nil {
		t.Fatalf("LoadCLIConfig: %v", err)
	}
	if cfg.ActiveServer != "beta" {
		t.Errorf("ActiveServer = %q, want beta", cfg.ActiveServer)
	}

	// Nothing leaked into the real default location.
	if _, err := os.Stat(filepath.Join(home, ".bunker", "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("default-location config exists after overridden run (err=%v) — leak", err)
	}
}

// TestAuditDefaultPathFollowsDaemonConfig proves rule 2b at both layers:
// the resolver (DefaultAuditLogPath) and a real command run (audit list
// without --path reads the daemon-configured log). Also proves the fallback
// rows (absent / invalid daemon config) return the documented constant
// silently, and that an explicit --path wins over the daemon config.
func TestAuditDefaultPathFollowsDaemonConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setenvForTest(t, "BUNKER_HOME", "") // unset
	setenvForTest(t, "BUNKERD_CONFIG", "")
	resetPathOverrides(t)

	logPath := newAuditQueryLog(t, 2)
	daemonCfg := writeDaemonConfig(t, logPath, "")

	t.Run("resolver returns daemon-configured path", func(t *testing.T) {
		SetDaemonConfigPathOverride(daemonCfg)
		if got := DefaultAuditLogPath(); got != logPath {
			t.Errorf("DefaultAuditLogPath = %q, want daemon-configured %q", got, logPath)
		}
	})

	t.Run("audit list follows daemon config without --path", func(t *testing.T) {
		SetDaemonConfigPathOverride(daemonCfg)
		out, err := auditCmd(t, "list")
		if err != nil {
			t.Fatalf("audit list (daemon-config default): %v", err)
		}
		if !strings.Contains(out, "Total: 2 records") {
			t.Errorf("audit list read the wrong log, stdout:\n%s", out)
		}
	})

	t.Run("env BUNKERD_CONFIG tier", func(t *testing.T) {
		setenvForTest(t, "BUNKERD_CONFIG", daemonCfg)
		if got := DefaultAuditLogPath(); got != logPath {
			t.Errorf("DefaultAuditLogPath (env tier) = %q, want %q", got, logPath)
		}
	})

	t.Run("flag tier beats env tier", func(t *testing.T) {
		otherLog := newAuditQueryLog(t, 1)
		otherCfg := writeDaemonConfig(t, otherLog, "")
		setenvForTest(t, "BUNKERD_CONFIG", daemonCfg)
		SetDaemonConfigPathOverride(otherCfg)
		if got := DefaultAuditLogPath(); got != otherLog {
			t.Errorf("DefaultAuditLogPath = %q, want flag-tier %q", got, otherLog)
		}
	})

	t.Run("absent daemon config falls back to documented constant", func(t *testing.T) {
		SetDaemonConfigPathOverride(filepath.Join(t.TempDir(), "missing.yaml"))
		if got := DefaultAuditLogPath(); got != defaultAuditLogPath {
			t.Errorf("DefaultAuditLogPath = %q, want fallback %q", got, defaultAuditLogPath)
		}
	})

	t.Run("invalid daemon config falls back silently", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.yaml")
		if err := os.WriteFile(bad, []byte("audit: [unclosed\n  bad: :::yaml"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		SetDaemonConfigPathOverride(bad) // must not error or panic
		if got := DefaultAuditLogPath(); got != defaultAuditLogPath {
			t.Errorf("DefaultAuditLogPath = %q, want fallback %q", got, defaultAuditLogPath)
		}
	})

	t.Run("explicit --path wins over daemon config", func(t *testing.T) {
		otherLog := newAuditQueryLog(t, 1)
		SetDaemonConfigPathOverride(daemonCfg) // daemon says: 2-record log
		out, err := auditCmd(t, "list", "--path", otherLog)
		if err != nil {
			t.Fatalf("audit list --path: %v", err)
		}
		if !strings.Contains(out, "Total: 1 records") {
			t.Errorf("explicit --path was overridden by daemon config, stdout:\n%s", out)
		}
	})
}

// TestAuditVerifyDefaultPathFollowsDaemonConfig proves rule 2b for the
// verify subcommand: `bunker audit verify` with no --path verifies the
// daemon-configured log.
func TestAuditVerifyDefaultPathFollowsDaemonConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setenvForTest(t, "BUNKERD_CONFIG", "")
	resetPathOverrides(t)

	logPath := newAuditQueryLog(t, 2)
	SetDaemonConfigPathOverride(writeDaemonConfig(t, logPath, ""))

	out, err := auditCmd(t, "verify")
	if err != nil {
		t.Fatalf("audit verify (daemon-config default): %v", err)
	}
	if !strings.Contains(out, logPath) || !strings.Contains(out, "OK (2 records)") {
		t.Errorf("verify output = %q, want %q OK (2 records)", out, logPath)
	}
}

// TestRegistryDefaultPathFollowsDaemonConfig proves rule 2c at the resolver
// layer and via a real `registry compact` run without --path. The fallback
// rows are proven at the resolver layer (executing compact against the
// real /var/lib default would write system state from a test).
func TestRegistryDefaultPathFollowsDaemonConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setenvForTest(t, "BUNKER_HOME", "") // unset
	setenvForTest(t, "BUNKERD_CONFIG", "")
	resetPathOverrides(t)

	regPath := filepath.Join(t.TempDir(), "agents.jsonl")
	seedRegistry(t, regPath, 3)
	daemonCfg := writeDaemonConfig(t, "", regPath)

	t.Run("resolver returns daemon-configured path", func(t *testing.T) {
		SetDaemonConfigPathOverride(daemonCfg)
		if got := DefaultRegistryPath(); got != regPath {
			t.Errorf("DefaultRegistryPath = %q, want %q", got, regPath)
		}
	})

	t.Run("registry compact follows daemon config without --path", func(t *testing.T) {
		SetDaemonConfigPathOverride(daemonCfg)
		cmd := NewRegistryCommand()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"compact"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("registry compact (daemon-config default): %v (output %q)", err, out.String())
		}
		if !strings.Contains(out.String(), "registry compacted: "+regPath) {
			t.Errorf("compact ran against the wrong registry, output:\n%s", out.String())
		}
	})

	t.Run("absent daemon config falls back to documented constant", func(t *testing.T) {
		SetDaemonConfigPathOverride(filepath.Join(t.TempDir(), "missing.yaml"))
		if got := DefaultRegistryPath(); got != config.DefaultRegistryPath {
			t.Errorf("DefaultRegistryPath = %q, want fallback %q", got, config.DefaultRegistryPath)
		}
	})

	t.Run("invalid daemon config falls back silently", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.yaml")
		if err := os.WriteFile(bad, []byte("agent:\n  registry: [oops"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		SetDaemonConfigPathOverride(bad)
		if got := DefaultRegistryPath(); got != config.DefaultRegistryPath {
			t.Errorf("DefaultRegistryPath = %q, want fallback %q", got, config.DefaultRegistryPath)
		}
	})

	t.Run("explicit --path wins over daemon config", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "other.jsonl")
		seedRegistry(t, other, 1)
		SetDaemonConfigPathOverride(daemonCfg)
		cmd := NewRegistryCommand()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"compact", "--path", other})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("registry compact --path: %v (output %q)", err, out.String())
		}
		if !strings.Contains(out.String(), "registry compacted: "+other) {
			t.Errorf("explicit --path was overridden by daemon config, output:\n%s", out.String())
		}
	})
}

// TestDaemonConfigEnvVarBlankMeansUnset pins the blank-value rule for
// BUNKERD_CONFIG (mirrors the --config/BUNKER_HOME blank rule).
func TestDaemonConfigEnvVarBlankMeansUnset(t *testing.T) {
	resetPathOverrides(t)
	setenvForTest(t, "BUNKERD_CONFIG", "   ") // blank = unset
	if got, want := daemonConfigPath(), DefaultDaemonConfigPath; got != want {
		t.Errorf("daemonConfigPath = %q, want default %q", got, want)
	}
}

// TestDefaultAuditLogPath_Constant pins the documented fallback constant
// (A5-adjacent): with no daemon config reachable, the audit/registry
// defaults are exactly today's constants.
func TestDefaultAuditLogPath_Constant(t *testing.T) {
	resetPathOverrides(t)
	setenvForTest(t, "BUNKERD_CONFIG", "")
	SetDaemonConfigPathOverride(filepath.Join(t.TempDir(), "missing.yaml"))
	if got := DefaultAuditLogPath(); got != "/var/log/bunkerd/audit.log" {
		t.Errorf("DefaultAuditLogPath = %q, want /var/log/bunkerd/audit.log", got)
	}
}
