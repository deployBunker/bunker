package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// INT-CI-042: the spawn keyfile path is deterministic
// (/tmp/bunker-key-<agentID>), so a previous spawn of the SAME id that was
// killed before its rollback could run leaves the pair behind and the next
// spawn's ssh-keygen dies on its interactive overwrite prompt on the
// daemon's closed stdin:
//
//	run 37666760535 (bunker-mvp): "ssh-keygen failed: exit status 1
//	(output: ... /tmp/bunker-key-e2e-imgspec already exists.
//	Overwrite (y/n)?)" at stage keygen, 0.4s after the allowed-spec cell
//	started;
//	run 37654405376 (same runner, 30 min earlier): true deadline_exceeded
//	after the full 300s window, "NOT found on re-list after 5 attempts" —
//	the killed mid-spawn daemon that seeded run 1's stale file.
//
// The pre-clear must remove the stale pair (red arm), must NOT stand in the
// way of a clean spawn (green arm), and must surface a real removal failure
// instead of silently proceeding (loud arm).
func TestINTCI042_PreClearStaleKeyFiles(t *testing.T) {
	tests := []struct {
		name    string
		stale   bool                           // seed the deterministic path before the call
		blocker bool                           // make the removal fail via a non-empty directory
		wantErr bool                           // expect the helper to surface the failure
		inspect func(t *testing.T, dir string) // post-conditions when wantErr=false
	}{
		{
			name:  "stale pair is removed",
			stale: true,
			inspect: func(t *testing.T, dir string) {
				for _, p := range []string{
					filepath.Join(dir, "bunker-key-t042a"),
					filepath.Join(dir, "bunker-key-t042a.pub"),
				} {
					if _, serr := os.Stat(p); !os.IsNotExist(serr) {
						t.Fatalf("%s still present: %v", p, serr)
					}
				}
			},
		},
		{
			name: "clean path is a no-op",
		},
		{
			name:    "removal failure surfaces loudly",
			blocker: true,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)

			keyPath := filepath.Join(dir, "bunker-key-t042a")
			if tt.stale || tt.blocker {
				if err := os.WriteFile(keyPath, []byte("stale"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(keyPath+".pub", []byte("stale"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.blocker {
				// Swap the .pub FILE for a non-empty DIRECTORY: os.Remove
				// of a directory that still contains an entry fails
				// (ENOTEMPTY), which is the stand-in for a real removal
				// failure (permissions, busy mount). The helper must
				// surface it instead of silently "succeeding".
				if err := os.Remove(keyPath + ".pub"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(keyPath+".pub", 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(keyPath+".pub/inner", nil, 0644); err != nil {
					t.Fatal(err)
				}
			}

			err := preClearSpawnKeyFiles("t042a")
			if tt.wantErr != (err != nil) {
				t.Fatalf("preClearSpawnKeyFiles error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.inspect != nil {
				tt.inspect(t, dir)
			}
		})
	}
}

// The isolation arm: another agent's keyfile at ITS deterministic path is
// never touched by this agent's pre-clear — nor is a directory that merely
// shares the prefix. Run 1's kill seeded only e2e-imgspec's pair; a fix
// that swept the whole /tmp prefix would cross into files the spawn does
// not own.
func TestINTCI042_PreClearDoesNotTouchOtherAgents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	other := filepath.Join(dir, "bunker-key-other-agent")
	if err := os.WriteFile(other, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other+".pub", []byte("other"), 0644); err != nil {
		t.Fatal(err)
	}
	prefixDir := filepath.Join(dir, "bunker-key-other-agent.dir")
	if err := os.MkdirAll(prefixDir, 0700); err != nil {
		t.Fatal(err)
	}

	if err := preClearSpawnKeyFiles("t042a"); err != nil {
		t.Fatalf("preClearSpawnKeyFiles: %v", err)
	}
	for _, survivor := range []string{other, other + ".pub", prefixDir} {
		if _, serr := os.Stat(survivor); serr != nil {
			t.Fatalf("unrelated path %s was touched: %v", survivor, serr)
		}
	}
}

// The run-37666760535 fingerprint: with a stale pair seeded at the
// deterministic path, the REAL spawn-stage keygen argv no longer dies on
// the interactive overwrite prompt. The test drives the exact command
// constructor the spawn path uses (newKeygenCmd); against pre-fix code
// (no pre-clear before keygen) this fails with "already exists".
func TestINTCI042_KeygenSucceedsOverStaleKeyFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	agentID := "t042c"
	keyPath := filepath.Join(dir, "bunker-key-t042c")
	if err := os.WriteFile(keyPath, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".pub", []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := preClearSpawnKeyFiles(agentID); err != nil {
		t.Fatalf("pre-clear: %v", err)
	}

	cmd := newKeygenCmd(context.Background(), agentID)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen over stale path still fails: %v (output: %s)", err, out)
	}
	for _, p := range []string{keyPath, keyPath + ".pub"} {
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("expected fresh %s: %v", p, serr)
		}
	}
}
