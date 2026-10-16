package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DF-BUNKER-102: the mirror of the DF-BUNKER-95 collision. A scratch daemon
// that took its OWN agent.base_data_dir but left agent.registry.path unset
// (or pointed it into the default tree) opens ANOTHER daemon's registry as
// its own, replays it, and reconciles the foreign agents — observed live as a
// restore + destroy of production agents within 60s of boot. The gate under
// test refuses that boot.
//
// These tests drive the pure gate (detectBorrowedRegistryCollision) with a
// scratch directory standing in for the default agent.base_data_dir, so the
// refusal is provable without touching the host's real /var/lib/bunkerd. The
// exported method's delegation is pinned separately by
// TestDetectBorrowedRegistryCollisionDefaultsAndDelegation.

// owningTree seeds dir the way a daemon that has been RUNNING from the
// default tree leaves it — the two pieces of evidence the gate requires: the
// durable registry file and the instance identity file. It returns both paths.
func owningTree(t *testing.T, dir string) (registry, instance string) {
	t.Helper()
	registry = filepath.Join(dir, filepath.Base(DefaultRegistryPath))
	if err := os.WriteFile(registry, []byte(`{"agent_id":"prod-agent","status":"running"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	instance = filepath.Join(dir, InstanceIdentityFileName)
	if err := os.WriteFile(instance, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("seed instance identity: %v", err)
	}
	return registry, instance
}

// TestDetectBorrowedRegistryCollision covers the refusal shape and every safe
// shape the gate must allow. Each case builds its own config; defaultDir is
// the scratch tree standing in for the default agent.base_data_dir.
func TestDetectBorrowedRegistryCollision(t *testing.T) {
	tests := []struct {
		name string
		// build returns the config under test. It may seed state under
		// defaultDir to model an occupied default tree.
		build   func(t *testing.T, defaultDir string) *Config
		wantErr bool
	}{
		{
			// The reported incident's config shape, made hermetic: the
			// daemon has its own base_data_dir and its registry resolves
			// into the default tree, which already holds a foreign
			// daemon's registry + instance identity.
			name: "custom base_data_dir, registry in default tree, complete default tree",
			build: func(t *testing.T, defaultDir string) *Config {
				owningTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))
				return cfg
			},
			wantErr: true,
		},
		{
			// Cosmetic spellings of the borrowed registry must still gate:
			// pathWithinDir normalises both sides.
			name: "custom base_data_dir, registry in default tree via . path element",
			build: func(t *testing.T, defaultDir string) *Config {
				owningTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, ".", filepath.Base(DefaultRegistryPath))
				return cfg
			},
			wantErr: true,
		},
		{
			// A trailing slash on the custom base must not read as the
			// default (or break the gate).
			name: "custom base_data_dir with trailing slash, registry in default tree",
			build: func(t *testing.T, defaultDir string) *Config {
				owningTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch") + "/"
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))
				return cfg
			},
			wantErr: true,
		},
		{
			// The ordinary production restart: base_data_dir IS the default
			// and the registry is inside it. That shape belongs to the
			// DF-BUNKER-95 gate; this one must stay silent even with a
			// complete tree in place, or it would brick prod twice over.
			name: "default base_data_dir, own registry inside it (production restart)",
			build: func(t *testing.T, defaultDir string) *Config {
				owningTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = defaultDir
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))
				return cfg
			},
			wantErr: false,
		},
		{
			// Acceptance: the operator pointed BOTH keys into the daemon's
			// own tree. The default tree's registry is not this daemon's to
			// open, and nothing this daemon resolves touches it.
			name: "custom base_data_dir, registry inside the custom base",
			build: func(t *testing.T, defaultDir string) *Config {
				owningTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
				cfg.Agent.Registry.Path = filepath.Join(cfg.Agent.BaseDataDir, "agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			// A registry outside BOTH trees is equally safe.
			name: "custom base_data_dir, registry elsewhere entirely",
			build: func(t *testing.T, defaultDir string) *Config {
				owningTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
				cfg.Agent.Registry.Path = filepath.Join(t.TempDir(), "scratch-agents.jsonl")
				return cfg
			},
			wantErr: false,
		},
		{
			// Default tree absent: an unset/failed registry in the default
			// tree with nobody home is a first boot, not a borrow.
			name: "custom base_data_dir, registry in default tree, default tree MISSING",
			build: func(t *testing.T, defaultDir string) *Config {
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))
				return cfg
			},
			wantErr: false,
		},
		{
			// Partial evidence: the registry file alone is not proof of an
			// owning daemon — the instance identity is required too.
			name: "custom base_data_dir, registry in default tree, PARTIAL tree (registry only)",
			build: func(t *testing.T, defaultDir string) *Config {
				if err := os.MkdirAll(defaultDir, 0o755); err != nil {
					t.Fatalf("mkdir default dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath)), []byte("{}\n"), 0o600); err != nil {
					t.Fatalf("seed registry: %v", err)
				}
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))
				return cfg
			},
			wantErr: false,
		},
		{
			// The complementary partial: the instance file alone (written by
			// the very first boot) is not proof either.
			name: "custom base_data_dir, registry in default tree, instance file only",
			build: func(t *testing.T, defaultDir string) *Config {
				if err := os.MkdirAll(defaultDir, 0o755); err != nil {
					t.Fatalf("mkdir default dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(defaultDir, InstanceIdentityFileName), []byte("deadbeef\n"), 0o600); err != nil {
					t.Fatalf("seed instance: %v", err)
				}
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))
				return cfg
			},
			wantErr: false,
		},
		{
			// A config naming no tree cannot contrast one against another.
			name: "empty base_data_dir",
			build: func(t *testing.T, defaultDir string) *Config {
				owningTree(t, defaultDir)
				cfg := DefaultConfig()
				cfg.Agent.BaseDataDir = ""
				cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))
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

			err := detectBorrowedRegistryCollision(defaultDir, cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("gate returned nil, want a borrowed-registry refusal")
				}
				var collision *BorrowedRegistryCollisionError
				if !errors.As(err, &collision) {
					t.Fatalf("error type = %T, want *BorrowedRegistryCollisionError (%v)", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("gate returned %v, want nil", err)
			}
		})
	}
}

// TestDetectBorrowedRegistryCollisionErrorNamesEveryPath pins the message
// contract: the refusal names the borrowed registry, the tree that owns it,
// the instance file that proves ownership, this daemon's own base dir, and
// the one-line fix — and stays free of the "refusing to start: " prefix,
// which the call sites add exactly once.
func TestDetectBorrowedRegistryCollisionErrorNamesEveryPath(t *testing.T) {
	clearSecretEnv(t)
	defaultDir := t.TempDir()
	registry, instance := owningTree(t, defaultDir)

	ownBase := filepath.Join(t.TempDir(), "bunkerd-scratch")
	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = ownBase
	cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))

	err := detectBorrowedRegistryCollision(defaultDir, cfg)
	if err == nil {
		t.Fatal("gate returned nil, want a refusal for a complete default tree")
	}

	var collision *BorrowedRegistryCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("error type = %T, want *BorrowedRegistryCollisionError", err)
	}
	if collision.BorrowedRegistryPath != registry {
		t.Errorf("refusal names registry=%s, want the borrowed file (%s)", collision.BorrowedRegistryPath, registry)
	}
	if collision.OwningBaseDataDir != defaultDir {
		t.Errorf("refusal names owning base=%s, want the default dir (%s)", collision.OwningBaseDataDir, defaultDir)
	}
	if collision.InstancePath != instance {
		t.Errorf("refusal names instance=%s, want the identity file proving ownership (%s)", collision.InstancePath, instance)
	}
	if collision.OwnBaseDataDir != filepath.Clean(ownBase) {
		t.Errorf("refusal names own base=%s, want this daemon's own dir (%s)", collision.OwnBaseDataDir, filepath.Clean(ownBase))
	}

	msg := err.Error()
	for _, want := range []string{
		registry,                  // the path that would be replayed
		defaultDir,                // the tree that owns it
		instance,                  // the ownership evidence
		"Fix:",                    // the remedy, spelled out
		"set agent.registry.path", // and exactly how to apply it
		"agent.base_data_dir",     // ...relative to which dir
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "refusing to start:") {
		t.Errorf("error message %q carries the call-site prefix; the gate must not add it (the call sites add it once)", msg)
	}
	if want := filepath.Join(filepath.Clean(ownBase), filepath.Base(DefaultRegistryPath)); !strings.Contains(msg, want) {
		t.Errorf("error message %q does not suggest the in-tree fix path %q", msg, want)
	}
}

// TestDetectBorrowedRegistryCollisionIsReadOnly pins the property the boot
// ordering depends on: the probe never creates, writes or removes anything,
// so calling it before EnsureJWTSecret and before the agent manager cannot
// itself claim an identity or seed a tree. The borrowed tree must come back
// byte- and mode-identical, and the verdict must be stable across calls.
func TestDetectBorrowedRegistryCollisionIsReadOnly(t *testing.T) {
	clearSecretEnv(t)
	defaultDir := t.TempDir()
	registry, instance := owningTree(t, defaultDir)

	snap := func(path string) (string, os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data), info.Mode()
	}
	regBefore, regModeBefore := snap(registry)
	instBefore, instModeBefore := snap(instance)
	entriesBefore, err := os.ReadDir(defaultDir)
	if err != nil {
		t.Fatalf("read default dir: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
	cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))

	// Twice: the verdict must be stable and side-effect free.
	for i := 0; i < 2; i++ {
		if err := detectBorrowedRegistryCollision(defaultDir, cfg); err == nil {
			t.Fatalf("call %d: gate returned nil, want a refusal", i+1)
		}
	}

	regAfter, regModeAfter := snap(registry)
	instAfter, instModeAfter := snap(instance)
	if regBefore != regAfter || regModeBefore != regModeAfter {
		t.Errorf("registry file changed: %q/%v -> %q/%v", regBefore, regModeBefore, regAfter, regModeAfter)
	}
	if instBefore != instAfter || instModeBefore != instModeAfter {
		t.Errorf("instance file changed: %q/%v -> %q/%v", instBefore, instModeBefore, instAfter, instModeAfter)
	}
	entriesAfter, err := os.ReadDir(defaultDir)
	if err != nil {
		t.Fatalf("re-read default dir: %v", err)
	}
	if len(entriesBefore) != len(entriesAfter) {
		t.Errorf("default dir entries %d -> %d: the read-only probe created or removed files",
			len(entriesBefore), len(entriesAfter))
	}
}

// TestDetectBorrowedRegistryCollisionUnreadableDefaultDir proves the gate
// fails OPEN (nil) when it cannot stat the tree: it may only refuse on
// positive evidence. Root ("we can see everything") skips, since the point of
// the case is the EACCES path.
func TestDetectBorrowedRegistryCollisionUnreadableDefaultDir(t *testing.T) {
	clearSecretEnv(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-0000 dir is still readable to the owner of the check")
	}
	defaultDir := t.TempDir()
	owningTree(t, defaultDir)
	if err := os.Chmod(defaultDir, 0o000); err != nil {
		t.Fatalf("chmod default dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(defaultDir, 0o700) })

	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
	cfg.Agent.Registry.Path = filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))

	if err := detectBorrowedRegistryCollision(defaultDir, cfg); err != nil {
		t.Errorf("gate refused on an UNREADABLE tree (must fail open): %v", err)
	}
}

// TestDetectBorrowedRegistryCollisionUnsetRegistryFallsBackToDefault covers
// the exact incident mechanism: agent.registry.path left EMPTY. The fallback
// resolves to DefaultRegistryPath — which the constants define as
// <DefaultAgentBaseDataDir>/agents.jsonl, so against the REAL default dir the
// incident shape is structurally identical to the "registry in default tree"
// refusal case proven above. Against a SCRATCH default dir the fallback
// resolves OUTSIDE it, so the gate must allow (and must not panic): pinned
// here. The refusal itself is then provable hermetically only through the
// explicit-registry path, which the table's first case covers.
func TestDetectBorrowedRegistryCollisionUnsetRegistryFallsBackToDefault(t *testing.T) {
	clearSecretEnv(t)

	// The constants make the incident shape structural: the compiled default
	// registry IS a file inside the compiled default tree.
	if want := filepath.Join(DefaultAgentBaseDataDir, filepath.Base(DefaultRegistryPath)); DefaultRegistryPath != want {
		t.Fatalf("DefaultRegistryPath = %q, want %q (the borrowed-registry gate's containment rule relies on this)", DefaultRegistryPath, want)
	}

	// resolveOwnRegistryPath: empty falls back to the documented default;
	// a set path (even padded) is used as-is, trimmed.
	if got := resolveOwnRegistryPath(&Config{Agent: AgentConfig{Registry: RegistryConfig{Path: "  "}}}); got != DefaultRegistryPath {
		t.Errorf("empty registry path resolved to %q, want the DefaultRegistryPath %q", got, DefaultRegistryPath)
	}
	own := filepath.Join(t.TempDir(), "agents.jsonl")
	if got := resolveOwnRegistryPath(&Config{Agent: AgentConfig{Registry: RegistryConfig{Path: " " + own + " "}}}); got != own {
		t.Errorf("set registry path resolved to %q, want the trimmed configured value %q", got, own)
	}

	// With a scratch default dir the fallback resolves outside it: allow.
	defaultDir := t.TempDir()
	owningTree(t, defaultDir)
	cfg := DefaultConfig()
	cfg.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
	cfg.Agent.Registry.Path = ""
	if err := detectBorrowedRegistryCollision(defaultDir, cfg); err != nil {
		t.Errorf("empty registry path with a scratch default dir refused: %v", err)
	}
}

// TestDetectBorrowedRegistryCollisionDefaultsAndDelegation pins the exported
// method's delegation to the pure gate. Only shapes whose verdict cannot
// depend on the host's real /var/lib/bunkerd state are asserted here.
func TestDetectBorrowedRegistryCollisionDefaultsAndDelegation(t *testing.T) {
	clearSecretEnv(t)

	// A config that owns both keys is never refused, including through the
	// exported method (containment fails regardless of host state).
	own := DefaultConfig()
	own.Agent.BaseDataDir = filepath.Join(t.TempDir(), "bunkerd-scratch")
	own.Agent.Registry.Path = filepath.Join(own.Agent.BaseDataDir, "agents.jsonl")
	if err := own.DetectBorrowedRegistryCollision(); err != nil {
		t.Errorf("config with its own base_data_dir and registry refused: %v", err)
	}

	// A config naming no tree is never refused by this gate.
	empty := DefaultConfig()
	empty.Agent.BaseDataDir = ""
	if err := empty.DetectBorrowedRegistryCollision(); err != nil {
		t.Errorf("config with an empty base_data_dir refused: %v", err)
	}
}

// TestBootRefusesBorrowedRegistry is the criterion-named entry point for
// DF-BUNKER-102: with the daemon's OWN base_data_dir but a registry resolving
// into a complete default tree, boot is REFUSED and the error surfaces as
// "refusing to start: ..." — the exact wrapper both call sites apply.
func TestBootRefusesBorrowedRegistry(t *testing.T) {
	clearSecretEnv(t)

	root := t.TempDir()
	// The directory standing in for the default agent.base_data_dir, seeded
	// the way a daemon that has been running from it leaves it.
	defaultDir := filepath.Join(root, "var", "lib", "bunkerd")
	if err := os.MkdirAll(defaultDir, 0o755); err != nil {
		t.Fatalf("mkdir default dir: %v", err)
	}
	owningTree(t, defaultDir)

	// This daemon has its own tree but (the incident shape) its registry
	// resolves into the default tree.
	cfg := &Config{Agent: AgentConfig{
		BaseDataDir: filepath.Join(root, "scratch", "bunkerd"),
		Registry:    RegistryConfig{Path: filepath.Join(defaultDir, filepath.Base(DefaultRegistryPath))},
	}}

	err := detectBorrowedRegistryCollision(defaultDir, cfg)
	if err == nil {
		t.Fatal("gate returned nil, want a borrowed-registry refusal")
	}
	var collision *BorrowedRegistryCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("error type = %T, want *BorrowedRegistryCollisionError", err)
	}

	// The surface both call sites produce: the same fmt.Errorf wrap the
	// DF-BUNKER-95 gate gets, applied once.
	wrapped := fmt.Errorf("refusing to start: %w", err)
	if !strings.HasPrefix(wrapped.Error(), "refusing to start: ") {
		t.Fatalf("wrapped error %q does not surface as 'refusing to start: ...'", wrapped.Error())
	}
	for _, want := range []string{"Fix:", "set agent.registry.path"} {
		if !strings.Contains(wrapped.Error(), want) {
			t.Errorf("refusal %q does not name %q", wrapped.Error(), want)
		}
	}
}
