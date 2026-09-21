package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DF-BUNKER-33: the destroy-home policy is the config half of the
// archive-before-delete rule. The DEFAULT must be archive (the recoverable
// direction), the ONLY way to the destructive purge path is the literal
// "purge", and any typo/empty/stale value resolves to archive — a typo must
// never silently re-enable the unrecoverable delete. An empty archive dir
// NEVER disarms the archive either: it falls back to /var/backups/bunker.

// TestDestroyHomePolicyOrDefault pins the fail-closed resolution table.
func TestDestroyHomePolicyOrDefault(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty resolves to archive", "", DestroyPolicyArchive},
		{"explicit archive", "archive", DestroyPolicyArchive},
		{"purge is the only destructive spelling", "purge", DestroyPolicyPurge},
		{"typo never reaches purge", "purg", DestroyPolicyArchive},
		{"random value never reaches purge", "yes-delete-it", DestroyPolicyArchive},
		{"case-insensitive purge", "PURGE", DestroyPolicyPurge},
		{"whitespace-wrapped purge", "  purge  ", DestroyPolicyPurge},
		{"whitespace-wrapped archive", " archive ", DestroyPolicyArchive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a AgentConfig
			a.DestroyHomePolicy = tt.raw
			if got := a.DestroyHomePolicyOrDefault(); got != tt.want {
				t.Errorf("DestroyHomePolicyOrDefault(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestDestroyArchiveDirOrDefault: empty falls back to the documented
// default; whitespace-only is treated as empty; a real value passes
// through untouched (needed for tests and exotic deployments).
func TestDestroyArchiveDirOrDefault(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty falls back", "", DefaultDestroyArchiveDir},
		{"whitespace falls back", "   ", DefaultDestroyArchiveDir},
		{"value passes through", "/tmp/my-archives", "/tmp/my-archives"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a AgentConfig
			a.DestroyArchiveDir = tt.raw
			if got := a.DestroyArchiveDirOrDefault(); got != tt.want {
				t.Errorf("DestroyArchiveDirOrDefault(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestDefaultConfig_DestroyHomeDefaults: a fresh DefaultConfig() ships the
// archive policy — the incident default — and the /var/backups/bunker
// archive dir.
func TestDefaultConfig_DestroyHomeDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.Agent.DestroyHomePolicyOrDefault(); got != DestroyPolicyArchive {
		t.Errorf("default policy = %q, want archive", got)
	}
	if cfg.Agent.DestroyArchiveDir != DefaultDestroyArchiveDir {
		t.Errorf("default archive dir = %q, want %q", cfg.Agent.DestroyArchiveDir, DefaultDestroyArchiveDir)
	}
}

// TestLoad_DestroyHomeFromFile proves the YAML keys reach the struct: an
// operator must be able to set destroy_home_policy: purge (the documented
// legacy opt-out) and a custom archive dir from bunkerd.yaml.
func TestLoad_DestroyHomeFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bunkerd.yaml")
	body := "agent:\n  destroy_home_policy: purge\n  destroy_archive_dir: /srv/bunker-archives\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Agent.DestroyHomePolicyOrDefault(); got != DestroyPolicyPurge {
		t.Errorf("policy from file = %q, want purge", got)
	}
	if cfg.Agent.DestroyArchiveDir != "/srv/bunker-archives" {
		t.Errorf("archive dir from file = %q", cfg.Agent.DestroyArchiveDir)
	}
}

// TestLoad_DestroyHomeEnvOverride proves the BindEnv wiring: the env var
// (BUNKERD_AGENT_DESTROY_HOME_POLICY) overrides the file for policy and
// archive dir, matching how every other agent.* key resolves.
func TestLoad_DestroyHomeEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bunkerd.yaml")
	body := "agent:\n  destroy_home_policy: purge\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUNKERD_AGENT_DESTROY_HOME_POLICY", "archive")
	t.Setenv("BUNKERD_AGENT_DESTROY_ARCHIVE_DIR", "/env/archives")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Agent.DestroyHomePolicyOrDefault(); got != DestroyPolicyArchive {
		t.Errorf("policy after env override = %q, want archive", got)
	}
	if cfg.Agent.DestroyArchiveDir != "/env/archives" {
		t.Errorf("archive dir after env override = %q, want /env/archives", cfg.Agent.DestroyArchiveDir)
	}
}

// TestDestroyPolicyConstantsAreStable guards the literal vocabulary: the
// YAML-facing values are a public contract with operator configs and the
// daemon docs; renaming them silently breaks every purge opt-out.
func TestDestroyPolicyConstantsAreStable(t *testing.T) {
	if DestroyPolicyArchive != "archive" || DestroyPolicyPurge != "purge" {
		t.Errorf("policy constants drifted: archive=%q purge=%q", DestroyPolicyArchive, DestroyPolicyPurge)
	}
	if !strings.HasPrefix(DefaultDestroyArchiveDir, "/var/backups") {
		t.Errorf("default archive dir moved off /var/backups: %q", DefaultDestroyArchiveDir)
	}
}

// INFRA-BACKUP-01: destroy_archive_keep is the retention bound for the
// destroy-time home archive dir. The tricky resolution rule: UNSET must
// default to 20 (the emergency-cleanup precedent) while an EXPLICIT 0 must
// disable pruning entirely — the distinction lives in the viper default +
// env binding, not in ordinary Go zero-value logic.
func TestDestroyArchiveKeepOrZero(t *testing.T) {
	tests := []struct {
		name string
		raw  int
		want int
	}{
		{"unset struct field passes through (viper supplies 20 earlier)", 0, 0},
		{"default value passes through", DefaultDestroyArchiveKeep, DefaultDestroyArchiveKeep},
		{"custom keep passes through", 5, 5},
		{"negative treated as disabled", -3, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a AgentConfig
			a.DestroyArchiveKeep = tt.raw
			if got := a.DestroyArchiveKeepOrZero(); got != tt.want {
				t.Errorf("DestroyArchiveKeepOrZero(%d) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

// TestDefaultConfig_DestroyArchiveKeepDefault: a fresh DefaultConfig()
// ships the keep-20 retention default.
func TestDefaultConfig_DestroyArchiveKeepDefault(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Agent.DestroyArchiveKeep != DefaultDestroyArchiveKeep {
		t.Errorf("default destroy_archive_keep = %d, want %d", cfg.Agent.DestroyArchiveKeep, DefaultDestroyArchiveKeep)
	}
	if DefaultDestroyArchiveKeep != 20 {
		t.Errorf("DefaultDestroyArchiveKeep drifted: %d, want 20 (incident precedent)", DefaultDestroyArchiveKeep)
	}
}

// TestLoad_DestroyArchiveKeepUnsetDefaultsTo20: a config file with NO
// destroy_archive_keep key resolves to 20 inside Load — the unset-vs-0
// distinction is real at the Load boundary.
func TestLoad_DestroyArchiveKeepUnsetDefaultsTo20(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bunkerd.yaml")
	body := "agent:\n  destroy_home_policy: purge\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Agent.DestroyArchiveKeep; got != 20 {
		t.Errorf("unset destroy_archive_keep = %d, want 20", got)
	}
}

// TestLoad_DestroyArchiveKeepExplicitZeroDisables: an explicit
// destroy_archive_keep: 0 in YAML must reach the struct as 0 (pruning
// disabled), NOT be converted back to the default.
func TestLoad_DestroyArchiveKeepExplicitZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bunkerd.yaml")
	body := "agent:\n  destroy_archive_keep: 0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Agent.DestroyArchiveKeep; got != 0 {
		t.Errorf("explicit 0 destroy_archive_keep = %d, want 0 (explicit opt-out)", got)
	}
}

// TestLoad_DestroyArchiveKeepEnvOverride proves the BindEnv wiring for
// both new knobs: BUNKERD_AGENT_DESTROY_ARCHIVE_KEEP overrides the file
// and BUNKERD_AGENT_DESTROY_ARCHIVE_MAX_BYTES reaches the struct.
func TestLoad_DestroyArchiveKeepEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bunkerd.yaml")
	body := "agent:\n  destroy_archive_keep: 3\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUNKERD_AGENT_DESTROY_ARCHIVE_KEEP", "7")
	t.Setenv("BUNKERD_AGENT_DESTROY_ARCHIVE_MAX_BYTES", "1073741824")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Agent.DestroyArchiveKeep; got != 7 {
		t.Errorf("destroy_archive_keep after env override = %d, want 7", got)
	}
	if got := cfg.Agent.DestroyArchiveMaxBytes; got != 1073741824 {
		t.Errorf("destroy_archive_max_bytes after env override = %d, want 1073741824", got)
	}
}
