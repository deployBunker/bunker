package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DF-BUNKER-95: a scratch daemon that repointed ports/registry/ssh_dir but
// left agent.base_data_dir at the default reads ANOTHER daemon's
// <base_data_dir>/instance file, inherits its instance identity, and then
// classifies that daemon's live agents as its own orphans during the boot
// sweep — destroying them. The gate under test refuses that boot.
//
// These tests drive the pure gate (detectForeignInstanceCollision) with a
// scratch directory standing in for the default agent.base_data_dir, so the
// refusal is provable without touching the host's real /var/lib/bunkerd. The
// exported method's delegation to that scratch-free form is pinned separately
// by TestDetectForeignInstanceCollisionDefaultsAndDelegation.

// foreignTree seeds dir the way a daemon that has been RUNNING from it leaves
// it: the durable registry, the instance identity file, and the
// generated-secrets directory. It returns the three paths.
func foreignTree(t *testing.T, dir string) (registry, instance, secrets string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, secretsDirName), SecretsDirMode); err != nil {
		t.Fatalf("seed secrets dir: %v", err)
	}
	registry = filepath.Join(dir, filepath.Base(DefaultRegistryPath))
	if err := os.WriteFile(registry, []byte(`{"agent_id":"prod-agent","status":"running"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	instance = filepath.Join(dir, InstanceIdentityFileName)
	if err := os.WriteFile(instance, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("seed instance identity: %v", err)
	}
	return registry, instance, filepath.Join(dir, secretsDirName)
}

// TestDetectForeignInstanceCollision covers the three acceptance scenarios and
// the boundary shapes between them. Each case builds its own config; defaultDir
// is the scratch tree standing in for the default agent.base_data_dir.
func TestDetectForeignInstanceCollision(t *testing.T) {
	tests := []struct {
		name string
		// build returns the config under test. It may seed state under
		// defaultDir to model an occupied default tree.
		build   func(t *testing.T, defaultDir string) *Config
		wantErr bool
	}{
		{
			// Acceptance 2: the operator gave this daemon its own tree. Even
			// with a fully occupied default tree there is nothing shared.
			name: "own base_data_dir, foreign tree present (acceptance 2)",
			build: func(t *testing.T, defaultDir string) *Config {
				foreignTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-gap093")
				cfg.Agent.Registry.Path = filepath.Join(cfg.Agent.BaseDataDir, "agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			// Acceptance 3: default dir, nothing there yet — first boot.
			name: "default base_data_dir, empty tree (acceptance 3)",
			build: func(t *testing.T, defaultDir string) *Config {
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			// Acceptance 3, partial state: the identity file alone is written
			// by the very misconfigured boot, so it is not proof of another
			// daemon.
			name: "default base_data_dir, instance file only",
			build: func(t *testing.T, defaultDir string) *Config {
				if err := os.WriteFile(filepath.Join(defaultDir, InstanceIdentityFileName), []byte("deadbeef\n"), 0o600); err != nil {
					t.Fatalf("seed instance: %v", err)
				}
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			name: "default base_data_dir, registry+instance but no secrets dir",
			build: func(t *testing.T, defaultDir string) *Config {
				registry, instance, secrets := foreignTree(t, defaultDir)
				if err := os.RemoveAll(secrets); err != nil {
					t.Fatalf("remove secrets dir: %v", err)
				}
				_ = registry
				_ = instance
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			name: "default base_data_dir, registry+secrets but no instance file",
			build: func(t *testing.T, defaultDir string) *Config {
				_, instance, _ := foreignTree(t, defaultDir)
				if err := os.Remove(instance); err != nil {
					t.Fatalf("remove instance file: %v", err)
				}
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			// Acceptance 1: the reported bug's config shape — default
			// base_data_dir, registry repointed elsewhere, and a complete
			// foreign tree already there.
			name: "default base_data_dir, registry elsewhere, full foreign tree (acceptance 1)",
			build: func(t *testing.T, defaultDir string) *Config {
				foreignTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: true,
		},
		{
			// The ordinary production restart: this daemon's whole state tree
			// IS the default tree, so the tree is its own and there is no
			// foreign daemon to collide with. Refusing here would brick prod.
			name: "default base_data_dir, own registry inside it (production restart)",
			build: func(t *testing.T, defaultDir string) *Config {
				foreignTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))
				return cfg
			},
			wantErr: false,
		},
		{
			name: "default base_data_dir, own registry in a custom subpath of it",
			build: func(t *testing.T, defaultDir string) *Config {
				foreignTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, "state", "registry.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			// A hand-built Config may leave the registry path unset: it
			// resolves to the documented production registry, which is NOT
			// inside the scratch default dir, so the tree there is foreign.
			name: "default base_data_dir, registry path unset resolves to the production registry",
			build: func(t *testing.T, defaultDir string) *Config {
				foreignTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = ""
				return cfg
			},
			wantErr: true,
		},
		{
			// A config naming no tree cannot collide with one.
			name: "empty base_data_dir",
			build: func(t *testing.T, defaultDir string) *Config {
				foreignTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = ""
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			// Cosmetic spellings of the same directory must still be gated.
			name: "trailing-slash spelling of the default dir",
			build: func(t *testing.T, defaultDir string) *Config {
				foreignTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir + "/"
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: true,
		},
		{
			// A prefix is NOT the same directory: "/x/default-other" must not
			// read as "/x/default".
			name: "sibling dir sharing the default dir as a prefix",
			build: func(t *testing.T, defaultDir string) *Config {
				foreignTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir + "-other"
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearSecretEnv(t)
			defaultDir := t.TempDir()
			cfg := tc.build(t, defaultDir)

			err := detectForeignInstanceCollision(defaultDir, cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("gate returned nil, want a foreign-instance refusal")
				}
				var collision *ForeignInstanceCollisionError
				if !errors.As(err, &collision) {
					t.Fatalf("error type = %T, want *ForeignInstanceCollisionError (%v)", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("gate returned %v, want nil", err)
			}
		})
	}
}

// TestDetectForeignInstanceCollisionErrorNamesEveryPath pins acceptance 4: the
// refusal must name the conflict (all three paths) and the one-line fix.
func TestDetectForeignInstanceCollisionErrorNamesEveryPath(t *testing.T) {
	clearSecretEnv(t)
	defaultDir := t.TempDir()
	registry, instance, secrets := foreignTree(t, defaultDir)

	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = defaultDir
	cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")

	err := detectForeignInstanceCollision(defaultDir, cfg)
	if err == nil {
		t.Fatal("gate returned nil, want a refusal for a complete foreign tree")
	}

	var collision *ForeignInstanceCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("error type = %T, want *ForeignInstanceCollisionError", err)
	}
	if collision.BaseDataDir != defaultDir || collision.RegistryPath != registry ||
		collision.InstancePath != instance || collision.SecretsDir != secrets {
		t.Errorf("error fields = %+v, want base=%s registry=%s instance=%s secrets=%s",
			collision, defaultDir, registry, instance, secrets)
	}

	msg := err.Error()
	for _, want := range []string{
		defaultDir,                // the colliding base_data_dir
		"registry=" + registry,    // the foreign registry
		"instance=" + instance,    // the identity file that would be adopted
		"secrets=" + secrets,      // the foreign secrets dir
		"agent.base_data_dir",     // the config key to change
		"instance identity",       // what would be inherited
		"destroying them",         // the consequence
		"Fix:",                    // the remedy, spelled out
		"set agent.base_data_dir", // and exactly how to apply it
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not contain %q", msg, want)
		}
	}
}

// TestDetectForeignInstanceCollisionSecretsEnvDoesNotHide covers the reason the
// gate looks at the DEFAULT tree's secrets dir and not this config's resolved
// one: BUNKER_SECRETS_DIR pointing this daemon's secrets somewhere else says
// nothing about what already exists at the default location.
func TestDetectForeignInstanceCollisionSecretsEnvDoesNotHide(t *testing.T) {
	clearSecretEnv(t)
	defaultDir := t.TempDir()
	foreignTree(t, defaultDir)

	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = defaultDir
	cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")

	// This daemon's OWN secrets dir, elsewhere and empty.
	t.Setenv(SecretsDirEnv, filepath.Join(t.TempDir(), "own-secrets"))

	if err := detectForeignInstanceCollision(defaultDir, cfg); err == nil {
		t.Fatal("gate returned nil, want a refusal — BUNKER_SECRETS_DIR must not hide the foreign tree")
	}
}

// TestDetectForeignInstanceCollisionIsReadOnly pins the property the ordering
// of the boot gate depends on: the probe never creates, writes or removes
// anything, so calling it before the agent manager cannot itself claim an
// identity or seed a tree. The foreign identity file must come back byte- and
// mode-identical.
func TestDetectForeignInstanceCollisionIsReadOnly(t *testing.T) {
	clearSecretEnv(t)
	defaultDir := t.TempDir()
	registry, instance, secrets := foreignTree(t, defaultDir)

	before := map[string]string{}
	entries, err := os.ReadDir(defaultDir)
	if err != nil {
		t.Fatalf("read default dir: %v", err)
	}
	for _, e := range entries {
		before[e.Name()] = ""
	}
	instBefore, err := os.ReadFile(instance)
	if err != nil {
		t.Fatalf("read instance file: %v", err)
	}
	infoBefore, err := os.Stat(instance)
	if err != nil {
		t.Fatalf("stat instance file: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = defaultDir
	cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")

	// Twice: the verdict must be stable and side-effect free.
	for i := 0; i < 2; i++ {
		if err := detectForeignInstanceCollision(defaultDir, cfg); err == nil {
			t.Fatalf("call %d: gate returned nil, want a refusal", i+1)
		}
	}

	entries, err = os.ReadDir(defaultDir)
	if err != nil {
		t.Fatalf("re-read default dir: %v", err)
	}
	after := map[string]string{}
	for _, e := range entries {
		after[e.Name()] = ""
	}
	if len(before) != len(after) {
		t.Errorf("directory entries changed: before=%v after=%v", before, after)
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("gate CREATED %s in the foreign tree; it must be read-only", name)
		}
	}
	instAfter, err := os.ReadFile(instance)
	if err != nil {
		t.Fatalf("re-read instance file: %v", err)
	}
	if string(instAfter) != string(instBefore) {
		t.Errorf("gate rewrote the foreign instance identity: %q -> %q", instBefore, instAfter)
	}
	infoAfter, err := os.Stat(instance)
	if err != nil {
		t.Fatalf("re-stat instance file: %v", err)
	}
	if infoAfter.Mode() != infoBefore.Mode() {
		t.Errorf("instance mode changed: %v -> %v", infoBefore.Mode(), infoAfter.Mode())
	}
	// The seeded paths must still be exactly what the gate reported it saw.
	for _, p := range []string{registry, instance, secrets} {
		if !statePathExists(p) {
			t.Errorf("gate removed %s", p)
		}
	}
}

// TestDetectForeignInstanceCollisionDefaultsAndDelegation pins the constants the
// gate shares with the rest of the config package and the exported method's
// production semantics that CAN be proven on any host: a config that owns the
// default tree is never refused, because the gate returns before it stats
// anything (so this test is host-independent even though /var/lib/bunkerd is
// root-owned).
func TestDetectForeignInstanceCollisionDefaultsAndDelegation(t *testing.T) {
	clearSecretEnv(t)

	if DefaultAgentBaseDataDir != "/var/lib/bunkerd" {
		t.Errorf("DefaultAgentBaseDataDir = %q, want /var/lib/bunkerd", DefaultAgentBaseDataDir)
	}
	if got := filepath.Dir(DefaultRegistryPath); got != DefaultAgentBaseDataDir {
		t.Errorf("filepath.Dir(DefaultRegistryPath) = %q, want the default base data dir %q", got, DefaultAgentBaseDataDir)
	}
	// The twin of internal/agent's instanceIDFile: the identity file the
	// agent manager adopts and the boot gate detects MUST be the same file.
	if InstanceIdentityFileName != "instance" {
		t.Errorf("InstanceIdentityFileName = %q, want \"instance\" (internal/agent's instanceIDFile)", InstanceIdentityFileName)
	}
	if secretsDirName != "secrets" {
		t.Errorf("secretsDirName = %q, want \"secrets\"", secretsDirName)
	}

	// A config that owns its tree: default base_data_dir AND the production
	// registry inside it — the ordinary production restart.
	prod := DefaultConfig()
	prod.Agent.BaseDataDir = DefaultAgentBaseDataDir
	prod.Agent.Registry.Path = DefaultRegistryPath
	if err := prod.DetectForeignInstanceCollision(); err != nil {
		t.Errorf("production restart refused: %v", err)
	}

	// A config with its own tree is never refused (acceptance 2), including
	// through the exported method.
	own := DefaultConfig()
	own.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
	own.Agent.Registry.Path = filepath.Join(own.Agent.BaseDataDir, "agents.jsonl")
	if err := own.DetectForeignInstanceCollision(); err != nil {
		t.Errorf("config with its own base_data_dir refused: %v", err)
	}

	// The empty-registry case must resolve to the documented default rather
	// than silently skipping the rule-2 containment test.
	empty := DefaultConfig()
	empty.Agent.BaseDataDir = t.TempDir()
	empty.Agent.Registry.Path = ""
	if err := empty.DetectForeignInstanceCollision(); err != nil {
		t.Errorf("empty registry path with a scratch base_data_dir refused: %v", err)
	}
}

// TestDetectForeignInstanceCollisionRefusesNothingWhenUnreadable pins the
// fail-open half of the gate: when the default tree cannot be observed (a
// non-root daemon facing a root-owned /var/lib/bunkerd, or a path that simply
// is not there), the gate reports nil rather than assuming a foreign tree. The
// positive case here is a path that does not exist at all; the permission case
// is exercised by TestDetectForeignInstanceCollisionUnreadableDefaultDir below.
func TestDetectForeignInstanceCollisionRefusesNothingWhenUnreadable(t *testing.T) {
	clearSecretEnv(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = missing
	cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")

	if err := detectForeignInstanceCollision(missing, cfg); err != nil {
		t.Errorf("gate refused a default dir that does not exist: %v", err)
	}
}

// TestDetectForeignInstanceCollisionUnreadableDefaultDir proves the gate fails
// OPEN (nil) when it cannot stat the tree: the checks may only refuse on
// positive evidence. Root ("we can see everything") skips, since the point of
// the case is the EACCES path.
func TestDetectForeignInstanceCollisionUnreadableDefaultDir(t *testing.T) {
	clearSecretEnv(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-0000 dir is still readable to the owner of the check")
	}
	defaultDir := t.TempDir()
	foreignTree(t, defaultDir)
	if err := os.Chmod(defaultDir, 0o000); err != nil {
		t.Fatalf("chmod default dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(defaultDir, 0o700) })

	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = defaultDir
	cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")

	if err := detectForeignInstanceCollision(defaultDir, cfg); err != nil {
		t.Errorf("gate refused on an UNREADABLE tree (must fail open): %v", err)
	}
}

// TestBootRefusesForeignInstanceIdentity is the criterion-named entry point for
// DF-BUNKER-95 acceptance 1: with agent.base_data_dir at the default dir, this
// daemon's registry elsewhere, and a foreign daemon's state tree (registry +
// instance identity + secrets) already in that default dir, boot is REFUSED
// with a *ForeignInstanceCollisionError.
//
// It drives the gate through the injectable default-dir seam rather than the
// exported method, because the exported method compares against the host's real
// /var/lib/bunkerd — root-owned on every deployed host and never seeded by a
// test. The seam takes the default dir as an argument, so this proves the same
// predicate hermetically; TestDetectForeignInstanceCollisionDefaultsAndDelegation
// pins the exported method's delegation to it.
func TestBootRefusesForeignInstanceIdentity(t *testing.T) {
	clearSecretEnv(t)

	root := t.TempDir()
	// The directory standing in for the default agent.base_data_dir, seeded the
	// way a daemon that has been running from it leaves it.
	defaultDir := filepath.Join(root, "var", "lib", "bunkerd")
	foreignTree(t, defaultDir)

	// This daemon's own registry lives elsewhere, so the tree at the default
	// location is not this daemon's tree.
	cfg := &Config{Agent: AgentConfig{
		BaseDataDir: defaultDir,
		Registry:    RegistryConfig{Path: filepath.Join(root, "scratch", "agents.jsonl")},
	}}

	err := detectForeignInstanceCollision(defaultDir, cfg)
	if err == nil {
		t.Fatal("gate returned nil, want a foreign-instance refusal")
	}
	var collision *ForeignInstanceCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("error type = %T, want *ForeignInstanceCollisionError", err)
	}
	if want := filepath.Join(defaultDir, InstanceIdentityFileName); collision.InstancePath != want {
		t.Errorf("refusal names instance=%s, want the identity file boot would have adopted (%s)",
			collision.InstancePath, want)
	}
	// The hazard the refusal must state: the identity it would have inherited,
	// and the agents that would then be destroyed as its own orphans.
	for _, want := range []string{"instance identity", "destroying them"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q does not name %q", err.Error(), want)
		}
	}
}
