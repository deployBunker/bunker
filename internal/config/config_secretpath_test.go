package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- REV-BUNKER-SECRET-PATH: deterministic, config-derived jwt_secret ---- //
// --- location; the ambient-HOME fallback never silently adopts a secret. -- //

// secretsTestEnv pins the same environment surface clearSecretEnv covers and
// repoints HOME at a scratch dir the test controls, so an ambient HOME
// fallback resolves somewhere observable instead of into the developer's or
// CI user's real $HOME/.config (which the refactor must never read).
func secretsTestEnv(t *testing.T) {
	t.Helper()
	clearSecretEnv(t)
	t.Setenv("HOME", t.TempDir())
}

// baseDataCfg builds a DefaultConfig whose agent tree is rooted at dir, the
// way a config file with agent.base_data_dir set loads.
func baseDataCfg(dir string) *Config {
	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = dir
	cfg.Auth.Token = "static-token"
	return cfg
}

// TestEnsureJWTSecret_ConfiguredStateDirOwnsTheSecret is acceptance criterion
// 1: a config that names a state/data dir (no explicit secrets path) must
// generate INSIDE that state tree — never $HOME/.config/... — and a restart
// must load the secret back from the same path.
func TestEnsureJWTSecret_ConfiguredStateDirOwnsTheSecret(t *testing.T) {
	secretsTestEnv(t)
	data := t.TempDir()

	// An ambient secret ALREADY exists at the legacy HOME location: the
	// config-derived location must win regardless, because the config names
	// a state tree and the ambient file is outside it.
	ambientHome := filepath.Join(t.TempDir(), DefaultSecretsDir)
	if err := os.MkdirAll(ambientHome, 0o700); err != nil {
		t.Fatal(err)
	}
	ambient := writeSecret(t, ambientHome, JWTSecretFileName, "ambient-secret-from-real-home")

	cfg := baseDataCfg(data)
	notice, err := cfg.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("EnsureJWTSecret() error = %v", err)
	}
	if !strings.Contains(notice, "GENERATED") {
		t.Errorf("first-boot notice = %q, want it to say a secret was generated", notice)
	}

	wantDir := filepath.Join(data, "secrets")
	wantPath := filepath.Join(wantDir, JWTSecretFileName)
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("secret was not generated inside the configured state dir (%s): %v", wantPath, err)
	}
	if _, err := os.Stat(ambient); err != nil {
		t.Fatalf("ambient control file vanished: %v", err)
	}
	if raw, rerr := os.ReadFile(ambient); rerr != nil || string(raw) != "ambient-secret-from-real-home" {
		t.Errorf("the pre-existing ambient secret was modified: %q (%v)", raw, rerr)
	}

	// Restart: same value from the same state-tree path (no rotation), and
	// no NEW generation anywhere else.
	generated := cfg.Auth.JWTSecret
	cfg2 := baseDataCfg(data)
	notice2, err := cfg2.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("second EnsureJWTSecret() error = %v", err)
	}
	if cfg2.Auth.JWTSecret != generated {
		t.Errorf("restart produced a DIFFERENT jwt_secret (%q vs %q) — issued keys would break", cfg2.Auth.JWTSecret, generated)
	}
	if strings.Contains(notice2, "GENERATED") {
		t.Errorf("second boot regenerated a secret: %q", notice2)
	}
	if cfg2.Auth.JWTSecretFile != wantPath {
		t.Errorf("restart loaded the secret from %q, want the state-tree path %q", cfg2.Auth.JWTSecretFile, wantPath)
	}
	if !strings.Contains(notice2, wantDir) {
		t.Errorf("restart notice = %q, want it to name the resolved secrets dir %s", notice2, wantDir)
	}
}

// TestEnsureJWTSecret_NeverSilentlyAdoptsAmbientSecret is acceptance criterion
// 2, the defect pin (reviewer's Run B): a config that names a state dir must
// NOT silently adopt a pre-existing secret at the ambient HOME location. The
// daemon generates INSIDE the tree, leaves the ambient file untouched, and the
// notice NAMES the outside-tree file and says it is not in use (the
// no-tree arm of this contract, where the daemon refuses outright, is pinned
// by TestEnsureJWTSecret_AmbientPreexistingSecretWithoutTreeRefuses).
func TestEnsureJWTSecret_NeverSilentlyAdoptsAmbientSecret(t *testing.T) {
	secretsTestEnv(t)
	data := t.TempDir()

	// AmbientHome is derived from the SAME HOME secretsTestEnv pinned, so
	// outsideTreeNote's os.UserHomeDir lookup sees the planted file.
	ambientDir := filepath.Join(os.Getenv("HOME"), DefaultSecretsDir)
	if err := os.MkdirAll(ambientDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ambient := writeSecret(t, ambientDir, JWTSecretFileName, "stale-ambient-secret")

	cfg := baseDataCfg(data)
	notice, err := cfg.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("EnsureJWTSecret() error = %v", err)
	}
	// The state-tree secret exists and is NOT the ambient one.
	generated := filepath.Join(data, "secrets", JWTSecretFileName)
	raw, rerr := os.ReadFile(generated)
	if rerr != nil {
		t.Fatalf("no secret was generated inside the configured state tree: %v", rerr)
	}
	if string(raw) == "stale-ambient-secret" {
		t.Error("the ambient secret was adopted as the daemon identity — the configured state tree must win")
	}
	if cfg.Auth.JWTSecret != strings.TrimSpace(string(raw)) {
		t.Errorf("Auth.JWTSecret %q does not match the state-tree file %q", cfg.Auth.JWTSecret, raw)
	}
	// The ambient file is untouched...
	if after, aerr := os.ReadFile(ambient); aerr != nil || string(after) != "stale-ambient-secret" {
		t.Errorf("the ambient secret file was modified: %q (%v)", after, aerr)
	}
	// ...and the notice names the outside-tree condition and the path.
	if !strings.Contains(notice, ambient) {
		t.Errorf("notice %q does not name the outside-tree secret path %s", notice, ambient)
	}
	if !strings.Contains(notice, "NOT in use") {
		t.Errorf("notice %q does not say the outside-tree secret is not in use", notice)
	}
	if !strings.Contains(notice, SecretsDirEnv) {
		t.Errorf("notice %q does not name the opt-in key (%s) for adopting it", notice, SecretsDirEnv)
	}

	// Restart determinism: the SAME state-tree secret loads back — the
	// identity no longer depends on what sits in $HOME.
	cfg2 := baseDataCfg(data)
	notice2, err := cfg2.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("second EnsureJWTSecret() error = %v", err)
	}
	if cfg2.Auth.JWTSecret != cfg.Auth.JWTSecret {
		t.Errorf("restart produced a DIFFERENT jwt_secret — issued keys would break")
	}
	if strings.Contains(notice2, "GENERATED") {
		t.Errorf("second boot regenerated a secret: %q", notice2)
	}
}

// TestEnsureJWTSecret_AmbientPreexistingSecretWithoutTreeRefuses is the
// fail-closed arm of acceptance criterion 2: a config naming NO state tree
// whose ambient location already holds a secret must REFUSE — adopting it
// silently is the Run B identity flip, and generating over it would rotate a
// live signing key. The refusal names the resolved path and the keys that
// make the location explicit.
func TestEnsureJWTSecret_AmbientPreexistingSecretWithoutTreeRefuses(t *testing.T) {
	secretsTestEnv(t)

	ambientDir := filepath.Join(os.Getenv("HOME"), DefaultSecretsDir)
	if err := os.MkdirAll(ambientDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSecret(t, ambientDir, JWTSecretFileName, "whose-secret-is-this")

	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = ""
	cfg.Agent.Registry.Path = ""
	cfg.Auth.Token = "static-token"

	notice, err := cfg.EnsureJWTSecret()
	if err == nil {
		t.Fatalf("EnsureJWTSecret() silently adopted an ambient secret (notice %q)", notice)
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("error %q does not refuse to start", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(ambientDir, JWTSecretFileName)) {
		t.Errorf("error %q does not name the resolved ambient path", err)
	}
	if !strings.Contains(err.Error(), SecretsDirEnv) {
		t.Errorf("error %q does not name the env key (%s) that would make the location explicit", err, SecretsDirEnv)
	}
	if cfg.Auth.JWTSecret != "" {
		t.Errorf("config was seeded with a secret despite the refusal: %q", cfg.Auth.JWTSecret)
	}
	if _, serr := os.Stat(filepath.Join(ambientDir, "jwt_secret.tmp")); !os.IsNotExist(serr) {
		t.Errorf("the ambient secret file was overwritten: %v", serr)
	}
}

// TestEnsureJWTSecret_AmbientFallbackStillWorksWithLoudWarn is acceptance
// criterion 3: a config that names NO state tree (every path empty) keeps the
// documented legacy behavior — generate/load under $HOME/.config/bunkerd/
// secrets — but the returned notices now NAME the resolved location and warn
// that the path came from the environment rather than the config.
func TestEnsureJWTSecret_AmbientFallbackStillWorksWithLoudWarn(t *testing.T) {
	secretsTestEnv(t)
	home := os.Getenv("HOME")

	// The ambient environment must not leak a config tree into the ambient
	// fallback either: DefaultConfig carries /var/lib/bunkerd, which on a
	// deployed host holds a real state tree — blank it so this test pins
	// the NO-tree contract exactly.
	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = ""
	cfg.Agent.Registry.Path = ""
	cfg.Auth.Token = "static-token"
	notice, err := cfg.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("EnsureJWTSecret() error = %v", err)
	}
	wantDir := filepath.Join(home, DefaultSecretsDir)
	if !strings.Contains(notice, "GENERATED") {
		t.Errorf("first-boot notice = %q, want it to say a secret was generated", notice)
	}
	if !strings.Contains(notice, wantDir) {
		t.Errorf("first-boot notice = %q, want it to name the resolved secrets dir %s", notice, wantDir)
	}
	if !strings.Contains(notice, SecretsDirEnv) {
		t.Errorf("first-boot notice = %q, want the ambient-HOME WARN to name the key to set (%s)", notice, SecretsDirEnv)
	}

	// ...and a restart is the explicit opt-in path: the ambient location is
	// never silently re-adopted, so pinning BUNKER_SECRETS_DIR at the SAME
	// dir loads the SAME value (behavior preserved through opt-in — the
	// unopted restart refuses, pinned by
	// TestEnsureJWTSecret_AmbientPreexistingSecretWithoutTreeRefuses).
	generated := cfg.Auth.JWTSecret
	t.Setenv(SecretsDirEnv, wantDir)
	cfg2 := DefaultConfig()
	cfg2.Agent.BaseDataDir = ""
	cfg2.Agent.Registry.Path = ""
	cfg2.Auth.Token = "static-token"
	notice2, err := cfg2.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("second EnsureJWTSecret() error = %v", err)
	}
	if cfg2.Auth.JWTSecret != generated {
		t.Errorf("restart produced a DIFFERENT jwt_secret (%q vs %q)", cfg2.Auth.JWTSecret, generated)
	}
	if strings.Contains(notice2, "GENERATED") {
		t.Errorf("second boot regenerated a secret: %q", notice2)
	}
	if strings.Contains(notice2, "WARNING") {
		t.Errorf("an explicitly pinned location must not carry the ambient-HOME warning: %q", notice2)
	}
}

// TestEnsureJWTSecret_UnreadableConfiguredSecretIsFatal: the never-rotate
// semantics survive the location change — a non-empty persisted secret at the
// CONFIGURED location that cannot be read still refuses startup instead of
// generating a replacement over a live signing key (same contract as the
// legacy location, pinned by TestEnsureJWTSecret_UnreadablePersistedSecretIsFatal).
func TestEnsureJWTSecret_UnreadableConfiguredSecretIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	secretsTestEnv(t)
	data := t.TempDir()

	secretsDir := filepath.Join(data, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeSecret(t, secretsDir, JWTSecretFileName, "existing-secret")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	cfg := baseDataCfg(data)
	notice, err := cfg.EnsureJWTSecret()
	if err == nil {
		t.Fatalf("EnsureJWTSecret() = nil error for an unreadable persisted secret (notice %q)", notice)
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("error %q does not refuse to start", err)
	}
	if cfg.Auth.JWTSecret != "" {
		t.Errorf("config was seeded with a generated secret despite the failure: %q", cfg.Auth.JWTSecret)
	}
}

// TestEnsureJWTSecret_EnvStillBeatsStateDir pins the precedence at the top of
// the chain: an explicit BUNKER_SECRETS_DIR still outranks the config-derived
// location (an operator who names the env var asked for exactly that path).
func TestEnsureJWTSecret_EnvStillBeatsStateDir(t *testing.T) {
	secretsTestEnv(t)
	data := t.TempDir()
	envDir := filepath.Join(t.TempDir(), "explicit-env-secrets")
	t.Setenv(SecretsDirEnv, envDir)

	cfg := baseDataCfg(data)
	notice, err := cfg.EnsureJWTSecret()
	if err != nil {
		t.Fatalf("EnsureJWTSecret() error = %v", err)
	}
	if _, serr := os.Stat(filepath.Join(envDir, JWTSecretFileName)); serr != nil {
		t.Fatalf("BUNKER_SECRETS_DIR no longer wins: %v (notice %q)", serr, notice)
	}
	if _, serr := os.Stat(filepath.Join(data, "secrets", JWTSecretFileName)); !os.IsNotExist(serr) {
		t.Errorf("a secret leaked into the state dir despite the explicit env override: %v", serr)
	}
}

// TestResolveSecretsLocation_Kinds is the precedence table for WHY a secrets
// dir was chosen: env beats the configured tree, the tree beats ambient HOME,
// and only the ambient arm carries the operator-facing hint keys.
func TestResolveSecretsLocation_Kinds(t *testing.T) {
	clearSecretEnv(t)

	tests := []struct {
		name        string
		env         string
		baseDataDir string
		registry    string
		wantKind    SecretsSourceKind
		wantHints   bool
	}{
		{
			name:     "env wins over everything",
			env:      "/explicit/secrets",
			wantKind: SecretsSourceEnv,
		},
		{
			name:        "configured base_data_dir is config-derived",
			baseDataDir: "/var/lib/bunkerd",
			wantKind:    SecretsSourceConfig,
		},
		{
			name:     "configured registry path alone is config-derived",
			registry: "/data/agents.jsonl",
			wantKind: SecretsSourceConfig,
		},
		{
			name:      "no env and no tree is the ambient fallback",
			wantKind:  SecretsSourceHome,
			wantHints: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(SecretsDirEnv, tc.env)
			}
			cfg := DefaultConfig()
			cfg.Agent.BaseDataDir = tc.baseDataDir
			cfg.Agent.Registry.Path = tc.registry

			dir, path, kind, hints := cfg.ResolveSecretsLocation()
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if want := cfg.SecretsDirOrDefault(); dir != want {
				t.Errorf("dir = %q, want the resolver's %q", dir, want)
			}
			if want := filepath.Join(dir, JWTSecretFileName); path != want {
				t.Errorf("path = %q, want %q", path, want)
			}
			if tc.wantHints && len(hints) == 0 {
				t.Error("ambient arm carries no hint keys — the WARN would have nothing to name")
			}
			if !tc.wantHints && len(hints) != 0 {
				t.Errorf("hints = %v, want none for a config/env-derived location", hints)
			}
		})
	}
}

// TestSecretsDirOrDefaultIsPure pins the REV-BUNKER-SECRET-PATH contract on
// the resolver itself: with no env and no configured tree it is still the
// documented ambient default (legacy configs keep working), and with a
// configured tree the secrets dir is derived from that tree — os.UserHomeDir
// is never consulted while a tree is present. HOME is repointed at a scratch
// dir so any ambient leak fails the test instead of touching a real HOME.
func TestSecretsDirOrDefaultIsPure(t *testing.T) {
	secretsTestEnv(t)
	home := os.Getenv("HOME")

	if got, want := SecretsDirOrDefault("", ""), filepath.Join(home, DefaultSecretsDir); got != want {
		t.Errorf("SecretsDirOrDefault(\"\", \"\") = %q, want the documented ambient default %q", got, want)
	}

	data := t.TempDir()
	want := filepath.Join(data, "secrets")
	if got := SecretsDirOrDefault(data, ""); got != want {
		t.Errorf("SecretsDirOrDefault(<data>, \"\") = %q, want %q", got, want)
	}

	// A registry path but no base_data_dir still yields a config-derived dir
	// (the parent of the registry file + /secrets).
	regParent := t.TempDir()
	wantReg := filepath.Join(regParent, "secrets")
	if got := SecretsDirOrDefault("", filepath.Join(regParent, "agents.jsonl")); got != wantReg {
		t.Errorf("SecretsDirOrDefault(\"\", <registry>) = %q, want %q", got, wantReg)
	}

	// HOME pointing anywhere else must not change a config-derived result.
	t.Setenv("HOME", t.TempDir())
	if got := SecretsDirOrDefault(data, ""); got != want {
		t.Errorf("config-derived dir changed with HOME: %q, want %q", got, want)
	}

	// No HOME at all and no tree: the relative legacy default, which fails
	// loudly at write time instead of scattering secrets into the CWD.
	t.Setenv("HOME", "")
	if got, want := SecretsDirOrDefault("", ""), DefaultSecretsDir; got != want {
		t.Errorf("SecretsDirOrDefault(\"\", \"\") with no HOME = %q, want %q", got, want)
	}
	_ = home
}

// TestEnsureJWTSecret_RejectsUnusableStateTree: an explicitly configured but
// unusable state tree must fail loudly at the secrets step rather than fall
// back to the ambient HOME location (a silent fallback would scatter the
// signing key outside the tree the operator named).
func TestEnsureJWTSecret_RejectsUnusableStateTree(t *testing.T) {
	secretsTestEnv(t)
	data := t.TempDir()
	// A regular FILE where the state dir should be: deriving/creating a
	// secrets dir under it cannot succeed.
	blocked := filepath.Join(data, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := baseDataCfg(blocked)
	_, err := cfg.EnsureJWTSecret()
	if err == nil {
		t.Fatal("EnsureJWTSecret() = nil error for an unusable state tree, want a hard error")
	}
	if strings.Contains(err.Error(), os.Getenv("HOME")) {
		t.Errorf("error %q leaked the ambient HOME fallback", err)
	}
	if _, serr := os.Stat(filepath.Join(os.Getenv("HOME"), DefaultSecretsDir)); !os.IsNotExist(serr) {
		t.Errorf("the unusable tree fell back to the ambient HOME location: %v", serr)
	}
}

// TestEnsureJWTSecret_CorruptStateTreeFailsClosed guards the is-under-tree
// predicate itself: an EvalSymlinks failure outside the not-exist case (here,
// a path component that is not a directory) must be treated as NOT under the
// tree — refusing, never defaulting to adopt — rather than silently comparing
// raw strings.
func TestEnsureJWTSecret_CorruptStateTreeFailsClosed(t *testing.T) {
	secretsTestEnv(t)
	data := t.TempDir()

	// base_data_dir contains a regular file where a directory is needed, so
	// EvalSymlinks of the candidate path fails with ENOTDIR — not
	// ErrNotExist — on every platform.
	blocked := filepath.Join(data, "file", "nested")
	if err := os.WriteFile(filepath.Join(data, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := baseDataCfg(blocked)
	cfg.Agent.Registry.Path = ""

	// The daemon never built a state tree here, so there is nothing to
	// adopt and nothing to generate into: either outcome is acceptable ONLY
	// if it is loud. Pin the loud half: never a silent adopt-and-continue.
	notice, err := cfg.EnsureJWTSecret()
	if err == nil && notice == "" {
		t.Fatal("EnsureJWTSecret() silently succeeded in an unresolvable state tree")
	}
	if cfg.Auth.JWTSecret != "" {
		t.Errorf("config was seeded with a secret despite the unresolvable tree: %q", cfg.Auth.JWTSecret)
	}
}

// TestEnsureJWTSecret_RefusalDoesNotMaskUnreadableError keeps the two refusal
// classes distinct: when BOTH an ambient secret exists AND the configured
// location is unreadable, the unreadable-persisted-secret error (the one an
// operator can fix by chmod) must win over the ambient-adoption refusal.
func TestEnsureJWTSecret_RefusalDoesNotMaskUnreadableError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	secretsTestEnv(t)
	data := t.TempDir()

	// Ambient secret present (would trigger the outside-tree refusal)...
	ambientHome := filepath.Join(t.TempDir(), DefaultSecretsDir)
	if err := os.MkdirAll(ambientHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSecret(t, ambientHome, JWTSecretFileName, "ambient")
	// ...but the CONFIGURED location holds an unreadable non-empty secret.
	secretsDir := filepath.Join(data, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unreadable := writeSecret(t, secretsDir, JWTSecretFileName, "unreadable-secret")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}

	cfg := baseDataCfg(data)
	_, err := cfg.EnsureJWTSecret()
	if err == nil {
		t.Fatal("EnsureJWTSecret() = nil error, want the unreadable-secret refusal")
	}
	if !strings.Contains(err.Error(), "exists but cannot be read") {
		t.Errorf("error %q is not the unreadable-persisted-secret error", err)
	}
	if errors.Is(err, errAmbientSecretOutsideStateTree{}) {
		t.Errorf("the ambient refusal masked the actionable unreadable-file error: %v", err)
	}
}

// TestEnsureJWTSecret_UnreadableRefusalNamesOverrideKeys (DF-BUNKER-88): the
// unreadable-secret refusal keeps its fail-closed behavior but its MESSAGE
// must work for the case that produces it in the field — a non-root daemon
// resolving the same defaults as an existing root daemon and meeting the
// root-owned jwt_secret under /var/lib/bunkerd. "Fix its permissions" is
// impossible there, so the message must ALSO name every working escape hatch
// (the same keys ResolveSecretsLocation advertises via hintKeys): the
// BUNKER_SECRETS_DIR env override and the agent.base_data_dir /
// agent.registry.path config keys — while keeping the root-case advice.
func TestEnsureJWTSecret_UnreadableRefusalNamesOverrideKeys(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}

	tests := []struct {
		name    string
		useEnv  bool // point SecretsDirEnv at the tree instead of base_data_dir
		regPath bool
	}{
		{name: "env override", useEnv: true},
		{name: "base_data_dir override"},
		{name: "registry_path override", regPath: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretsTestEnv(t)
			data := t.TempDir()

			cfg := baseDataCfg(data)
			switch {
			case tt.useEnv:
				t.Setenv(SecretsDirEnv, filepath.Join(data, "env-secrets"))
			case tt.regPath:
				cfg.Agent.BaseDataDir = ""
				cfg.Agent.Registry.Path = filepath.Join(data, "agents.jsonl")
			}

			secretsDir := cfg.SecretsDirOrDefault()
			if err := os.MkdirAll(secretsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			unreadable := writeSecret(t, secretsDir, JWTSecretFileName, "root-daemons-secret")
			if err := os.Chmod(unreadable, 0o000); err != nil {
				t.Fatal(err)
			}

			_, err := cfg.EnsureJWTSecret()
			if err == nil {
				t.Fatal("EnsureJWTSecret() = nil error, want the unreadable-secret refusal (must stay fail-closed)")
			}
			if !strings.Contains(err.Error(), "exists but cannot be read") {
				t.Errorf("error %q is not the unreadable-persisted-secret error", err)
			}
			// The new, additive escape hatches — every location knob a
			// non-root operator can actually use.
			if !strings.Contains(err.Error(), SecretsDirEnv) {
				t.Errorf("error %q does not name the %s env override", err, SecretsDirEnv)
			}
			if !strings.Contains(err.Error(), "agent.base_data_dir") {
				t.Errorf("error %q does not name the agent.base_data_dir override", err)
			}
			// The existing root-case advice survives (additive change only).
			if !strings.Contains(err.Error(), "mode 0600") {
				t.Errorf("error %q no longer carries the root-case permissions advice", err)
			}
			// Fail-closed is unchanged: nothing was seeded, nothing rotated.
			if cfg.Auth.JWTSecret != "" {
				t.Errorf("config was seeded with a secret despite the refusal: %q", cfg.Auth.JWTSecret)
			}
		})
	}
}
