package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// TestRegistryCommandSurface pins the CLI shape: a `registry` group with a
// `compact` subcommand that takes --path and --dry-run.
func TestRegistryCommandSurface(t *testing.T) {
	cmd := NewRegistryCommand()
	if cmd.Use != "registry" {
		t.Errorf("Use = %q, want registry", cmd.Use)
	}
	var compactFound bool
	for _, sub := range cmd.Commands() {
		if sub.Name() == "compact" {
			compactFound = true
			if sub.Flags().Lookup("path") == nil {
				t.Error("compact must expose --path")
			}
			if sub.Flags().Lookup("dry-run") == nil {
				t.Error("compact must expose --dry-run")
			}
		}
	}
	if !compactFound {
		t.Fatal("registry must have a compact subcommand")
	}
	if defaultRegistryPath != config.DefaultRegistryPath {
		t.Errorf("CLI default registry path %q != config %q", defaultRegistryPath, config.DefaultRegistryPath)
	}
	if defaultRegistryPath != "/var/lib/bunkerd/agents.jsonl" {
		t.Errorf("default registry path = %q, want /var/lib/bunkerd/agents.jsonl", defaultRegistryPath)
	}
}

// seedRegistry writes a log with N spawns, a heartbeat and a destroy.
func seedRegistry(t *testing.T, path string, spawns int) {
	t.Helper()
	s, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	for i := 0; i < spawns; i++ {
		id := "agent-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		if err := s.AppendSpawn(&registry.Record{
			AgentID:   id,
			Status:    "running",
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("AppendSpawn: %v", err)
		}
		_ = s.AppendHeartbeat(id, time.Now().Add(2*time.Hour), "running")
	}
	if spawns > 0 {
		_ = s.AppendDestroy("agent-aa")
	}
}

// TestRegistryCompactRewritesAndReports is the operator-facing contract:
// before/after counts printed, file rewritten to one record per live agent.
func TestRegistryCompactRewritesAndReports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	seedRegistry(t, path, 10)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cmd := NewRegistryCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"compact", "--path", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("compact: %v (output %q)", err, out.String())
	}

	got := out.String()
	for _, want := range []string{"registry compacted:", "events:", "live agents:", "known destroyed ids:"} {
		if !strings.Contains(got, want) {
			t.Errorf("compact output missing %q:\n%s", want, got)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) >= len(before) {
		t.Errorf("compaction did not shrink the registry: %d -> %d bytes", len(before), len(after))
	}
	// The compacted file must keep the daemon's private mode.
	if err := ensureFileModeIsPrivate(path); err != nil {
		t.Errorf("compacted registry mode: %v", err)
	}

	// The rewritten file must replay to the same live/known sets.
	s, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if s.LiveCount() != 9 {
		t.Errorf("live after compact = %d, want 9 (10 spawned, 1 destroyed)", s.LiveCount())
	}
	if !s.Known("agent-aa") {
		t.Error("destroyed ID lost across compaction — idempotent destroy would break")
	}
}

// TestRegistryCompactDryRunWritesNothing proves --dry-run is read-only.
func TestRegistryCompactDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	seedRegistry(t, path, 4)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cmd := NewRegistryCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"compact", "--path", path, "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("compact --dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("dry-run output should say so:\n%s", out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("--dry-run modified the registry")
	}
}

// TestRegistryCompactExactPortMetadataSurvives shows compaction preserves the
// fields adopt needs to restore an exact reservation.
func TestRegistryCompactExactPortMetadataSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.jsonl")
	s, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	rec := &registry.Record{
		AgentID: "keep-ports", Status: "running", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		PortStart: 12300, PortEnd: 12399, SSHKeyPath: "/etc/bunkerd/ssh/keep-ports",
	}
	if err := s.AppendSpawn(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHeartbeat("keep-ports", time.Now().Add(3*time.Hour), "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	replayed, err := registry.Open(registry.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer replayed.Close()
	got := replayed.Get("keep-ports")
	if got == nil {
		t.Fatal("agent lost across compaction")
	}
	if got.PortStart != 12300 || got.PortEnd != 12399 {
		t.Errorf("ports = %d-%d, want 12300-12399 after compaction", got.PortStart, got.PortEnd)
	}
	if got.SSHKeyPath != "/etc/bunkerd/ssh/keep-ports" {
		t.Errorf("ssh key path = %q, want it preserved", got.SSHKeyPath)
	}
}

// TestRegistryCompactKeepsTrackerMetadata guards the allocator/registry pairing
// used by adopt (a literal port range from the compacted file).
func TestRegistryCompactKeepsTrackerMetadata(t *testing.T) {
	pa, err := resource.NewPortAllocator(10000, 19999, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := pa.Restore("restored", 12300, 12399); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if start, end, ok := pa.AllocatedRange("restored"); !ok || start != 12300 || end != 12399 {
		t.Fatalf("AllocatedRange = %d-%d (ok=%v)", start, end, ok)
	}
}

// --- DF-BUNKER-58: registry list read surface + unknown-subcommand exit ---

// TestRegistryListSurface pins the new read surface: a `list` subcommand
// with --path, next to the existing compact.
func TestRegistryListSurface(t *testing.T) {
	cmd := NewRegistryCommand()
	var listFound bool
	for _, sub := range cmd.Commands() {
		if sub.Name() == "list" {
			listFound = true
			if sub.Flags().Lookup("path") == nil {
				t.Error("list must expose --path")
			}
		}
	}
	if !listFound {
		t.Fatal("registry must have a list subcommand")
	}
}

// TestRegistryListOutput is the operator-facing read contract, table-driven:
// every row builds its fixture through the store's OWN append API (never
// hand-written JSONL) and asserts the printed lines.
func TestRegistryListOutput(t *testing.T) {
	seedNoTimestamps := func(t *testing.T, path string) {
		t.Helper()
		s, err := registry.Open(registry.Options{Path: path})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { t.Log("seeding store closed:", s.Close()) }()
		// Status left empty: replay folds it to the store's "running"
		// default; zero CreatedAt/ExpiresAt render as "-".
		if err := s.AppendSpawn(&registry.Record{AgentID: "no-times"}); err != nil {
			t.Fatalf("AppendSpawn: %v", err)
		}
	}

	tests := []struct {
		name            string
		spawns          int
		seed            func(t *testing.T, path string)
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:   "one current-state record per live agent plus count",
			spawns: 3,
			wantContains: []string{
				"agent-ba", "agent-ca", "2 live agent(s)",
			},
			wantNotContains: []string{
				"agent-aa", // destroyed: not live, must not be listed
			},
		},
		{
			name:   "empty registry is a successful read",
			spawns: 0,
			wantContains: []string{
				"0 live agent(s)",
				"created empty by this read", // Open fabricated the missing file; say so
			},
		},
		{
			name: "zero timestamps render as dash, status folded to running",
			seed: seedNoTimestamps,
			wantContains: []string{
				"no-times", "status=running", "created=-", "expires=-", "1 live agent(s)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.jsonl")
			if tt.seed != nil {
				tt.seed(t, path)
			} else if tt.spawns > 0 {
				seedRegistry(t, path, tt.spawns)
			}

			cmd := NewRegistryCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"list", "--path", path})
			err := cmd.Execute()
			if err != nil {
				t.Fatalf("list: %v (output %q)", err, out.String())
			}
			got := out.String()
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("list output missing %q:\n%s", want, got)
				}
			}
			for _, no := range tt.wantNotContains {
				if strings.Contains(got, no) {
					t.Errorf("list output must not contain %q:\n%s", no, got)
				}
			}
		})
	}
}

// TestRegistryListUnknownSubcommandErrors mirrors the env group's
// unknown-subcommand contract: a typo'd subcommand returns an ERROR naming
// the known ones (root sets SilenceErrors, so a returned error exits
// non-zero on the real binary), while the bare group still prints help and
// succeeds.
func TestRegistryListUnknownSubcommandErrors(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantErr   bool
		wantInErr string
	}{
		{
			name:      "typo subcommand errors naming the known ones",
			args:      []string{"bogus"},
			wantErr:   true,
			wantInErr: `unknown registry subcommand "bogus" (want: list, compact)`,
		},
		{
			name:      "near-miss subcommand errors too",
			args:      []string{"lst"},
			wantErr:   true,
			wantInErr: `unknown registry subcommand "lst"`,
		},
		{
			name:    "bare group prints help and succeeds",
			args:    []string{},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewRegistryCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("args %v: want error, got nil (output %q)", tt.args, out.String())
				}
				if !strings.Contains(err.Error(), tt.wantInErr) {
					t.Errorf("error %q missing %q", err.Error(), tt.wantInErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("args %v: want success, got %v", tt.args, err)
			}
		})
	}
}
