package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── DF-BUNKER-83: `bunker pull` local destination guard ─────────────────────
//
// scp's destination semantics are the defect: an existing FILE at the local
// destination is written ONTO (and the transfer still exits 0, so the CLI's
// "Copied ..." line masks the loss), while a missing path is created as a
// file. The guard must refuse the file case and create the missing directory
// so the pulled file always lands at <local-dir>/<basename>.

// scpEmulatingStub writes an `scp` stub that (a) records its argv one argument
// per line — the same contract setupPullScpStub's stub honors — and (b)
// EMULATES scp's file placement so a test can observe where the pulled bytes
// land: into <dest>/<basename> when dest is a directory, otherwise onto dest
// itself (the clobbering behavior). It deliberately does NOT create the
// destination directory: if the CLI fails to create it, the stub writes a file
// onto that path and the test's "is a directory" assertion fails.
func scpEmulatingStub(t *testing.T) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "scp-argv.txt")

	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
: > %[1]q
src=""
dest=""
for a in "$@"; do
  printf '%%s\n' "$a" >> %[1]q
  src="$dest"
  dest="$a"
done
base="${src##*:}"
base="${base##*/}"
if [ -d "$dest" ]; then
  printf 'PULLED:%%s\n' "$base" > "$dest/$base"
  exit 0
fi
# scp would create/clobber a FILE at dest — the DF-BUNKER-83 data loss.
printf 'PULLED:%%s\n' "$base" > "$dest"
exit 0
`, record)
	if err := os.WriteFile(filepath.Join(binDir, "scp"), []byte(script), 0755); err != nil {
		t.Fatalf("write scp stub: %v", err)
	}
	t.Setenv("PATH", binDir)
	return record
}

// TestEnsurePullDestination pins the guard's behavior for every destination
// shape: existing directory (untouched), existing non-directory (refused, bytes
// preserved), and missing path (created as a directory).
func TestEnsurePullDestination(t *testing.T) {
	const precious = "PRECIOUS - MUST SURVIVE"

	tests := []struct {
		name     string
		setup    func(t *testing.T, dir string) (target string, preserved string)
		wantErr  bool
		errParts []string
	}{
		{
			name: "existing directory is left alone",
			setup: func(t *testing.T, dir string) (string, string) {
				target := filepath.Join(dir, "out")
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return target, ""
			},
		},
		{
			name: "existing file is refused",
			setup: func(t *testing.T, dir string) (string, string) {
				target := filepath.Join(dir, "out")
				if err := os.WriteFile(target, []byte(precious), 0o644); err != nil {
					t.Fatalf("write file: %v", err)
				}
				return target, precious
			},
			wantErr:  true,
			errParts: []string{"exists and is not a directory", "pass a directory or remove the file"},
		},
		{
			name: "existing symlink to a file is refused",
			setup: func(t *testing.T, dir string) (string, string) {
				real := filepath.Join(dir, "real.txt")
				if err := os.WriteFile(real, []byte(precious), 0o644); err != nil {
					t.Fatalf("write file: %v", err)
				}
				target := filepath.Join(dir, "link")
				if err := os.Symlink(real, target); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return target, precious
			},
			wantErr:  true,
			errParts: []string{"exists and is not a directory"},
		},
		{
			name: "missing path is created as a directory",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "fresh"), ""
			},
		},
		{
			name: "missing nested path is created",
			setup: func(t *testing.T, dir string) (string, string) {
				return filepath.Join(dir, "a", "b", "c"), ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, preserved := tt.setup(t, t.TempDir())

			err := ensurePullDestination(target)

			if tt.wantErr && err == nil {
				t.Fatalf("ensurePullDestination(%q): expected error, got nil", target)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ensurePullDestination(%q): unexpected error: %v", target, err)
			}
			if tt.wantErr {
				for _, part := range tt.errParts {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("error %q does not mention %q", err.Error(), part)
					}
				}
				if !strings.Contains(err.Error(), target) {
					t.Errorf("error %q does not name the refused path %q", err.Error(), target)
				}
			}

			info, statErr := os.Stat(target)
			if !tt.wantErr {
				if statErr != nil {
					t.Fatalf("stat %q after guard: %v", target, statErr)
				}
				if !info.IsDir() {
					t.Fatalf("%q exists but is not a directory (mode %v)", target, info.Mode())
				}
				if info.Mode().Perm()&0o700 != 0o700 {
					t.Errorf("%q mode %v is not owner-traversable", target, info.Mode().Perm())
				}
			} else if preserved != "" {
				// The refused path must still be the same FILE with the same
				// bytes — the whole point of the guard.
				if statErr != nil {
					t.Fatalf("refused path %q vanished: %v", target, statErr)
				}
				if info.IsDir() {
					t.Fatalf("refused path %q was replaced by a directory", target)
				}
				// Resolve through any symlink so the preserved bytes are read
				// from the real file the guard refused to clobber.
				got, readErr := os.ReadFile(target)
				if readErr != nil {
					t.Fatalf("read refused path %q: %v", target, readErr)
				}
				if string(got) != preserved {
					t.Errorf("refused path %q content = %q, want %q", target, got, preserved)
				}
			}
		})
	}
}

// TestPullCommand_DestinationGuard drives `bunker pull` end-to-end with an
// scp stub that emulates where the bytes land, covering the three acceptance
// outcomes: existing directory (unchanged), missing directory (created), and
// existing file (refused, never overwritten, scp never invoked).
func TestPullCommand_DestinationGuard(t *testing.T) {
	const remotePath = "/tmp/remote.txt"
	const base = "remote.txt"
	const precious = "PRECIOUS - MUST SURVIVE"

	tests := []struct {
		name        string
		setupDest   func(t *testing.T) string
		wantErr     string // "" = no error
		wantScpRuns bool
		wantDestDir bool
		wantLanded  bool
	}{
		{
			name:        "existing directory: file lands inside (unchanged behavior)",
			setupDest:   func(t *testing.T) string { return t.TempDir() },
			wantScpRuns: true,
			wantDestDir: true,
			wantLanded:  true,
		},
		{
			name: "missing directory: created, file lands inside",
			setupDest: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "pulled-here")
			},
			wantScpRuns: true,
			wantDestDir: true,
			wantLanded:  true,
		},
		{
			name: "existing file: refused, content preserved, scp never invoked",
			setupDest: func(t *testing.T) string {
				target := filepath.Join(t.TempDir(), "dogfood-bunker")
				if err := os.WriteFile(target, []byte(precious), 0o644); err != nil {
					t.Fatalf("seed destination file: %v", err)
				}
				return target
			},
			wantErr:     "exists and is not a directory",
			wantScpRuns: false,
			wantDestDir: false,
			wantLanded:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupPullConfig(t)
			record := scpEmulatingStub(t)
			dest := tt.setupDest(t)

			out, _, err := runPullCommand(t, nil, "test-agent", remotePath, dest)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("pull failed: %v", err)
				}
				if !strings.Contains(out, "Copied ") {
					t.Errorf("success output %q missing the Copied line", out)
				}
			} else {
				if err == nil {
					t.Fatalf("pull to a non-directory %q: expected refusal, got nil error", dest)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				if !strings.Contains(err.Error(), dest) {
					t.Errorf("error %q does not name the refused destination %q", err.Error(), dest)
				}
				if strings.Contains(out, "Copied ") {
					t.Errorf("refusal still printed a success line: %q", out)
				}
			}

			// scp must run exactly when the destination is usable.
			argv, argvErr := os.ReadFile(record)
			scpRan := argvErr == nil
			if scpRan != tt.wantScpRuns {
				t.Fatalf("scp invoked = %v, want %v (record err: %v)", scpRan, tt.wantScpRuns, argvErr)
			}
			if scpRan {
				args := strings.Split(strings.TrimRight(string(argv), "\n"), "\n")
				if last := args[len(args)-1]; last != dest {
					t.Errorf("local destination must be scp's last arg: got %q, want %q", last, dest)
				}
			}

			info, statErr := os.Stat(dest)
			if statErr != nil {
				t.Fatalf("stat destination %q: %v", dest, statErr)
			}
			if info.IsDir() != tt.wantDestDir {
				t.Errorf("destination %q IsDir = %v, want %v", dest, info.IsDir(), tt.wantDestDir)
			}

			if tt.wantLanded {
				// B/C: the pulled file landed INSIDE the directory, named
				// after the remote path's basename.
				got, readErr := os.ReadFile(filepath.Join(dest, base))
				if readErr != nil {
					t.Fatalf("expected %q inside %q: %v", base, dest, readErr)
				}
				if want := "PULLED:" + base + "\n"; string(got) != want {
					t.Errorf("%s content = %q, want %q", base, got, want)
				}
				return
			}

			// A: the destination is still the same file, byte-identical.
			if info.IsDir() {
				t.Fatalf("destination %q was replaced by a directory", dest)
			}
			got, readErr := os.ReadFile(dest)
			if readErr != nil {
				t.Fatalf("read destination %q: %v", dest, readErr)
			}
			if string(got) != precious {
				t.Errorf("clobbered! destination content = %q, want %q", got, precious)
			}
		})
	}
}

// TestPullCommand_GuardPrecedesNetwork proves the destination check runs before
// the CLI ever resolves a server: with no config and a non-directory target the
// failure is the destination refusal, not a config/binding/connection error.
func TestPullCommand_GuardPrecedesNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(SessionTargetEnvVar, "")

	dest := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(dest, []byte("KEEP"), 0o644); err != nil {
		t.Fatalf("seed destination file: %v", err)
	}

	_, _, err := runPullCommand(t, nil, "test-agent", "/tmp/remote.txt", dest)
	if err == nil {
		t.Fatal("expected refusal for a non-directory destination")
	}
	if !strings.Contains(err.Error(), "exists and is not a directory") {
		t.Fatalf("expected the destination refusal before any server work, got: %v", err)
	}
	if got, readErr := os.ReadFile(dest); readErr != nil || string(got) != "KEEP" {
		t.Fatalf("destination file changed: content=%q err=%v", got, readErr)
	}
}

// TestPullCommand_DocsDestinationGuard keeps the new refusal discoverable in
// --help (the behavior is user-visible; docs parity for the flag list lives in
// internal/docscheck).
func TestPullCommand_DocsDestinationGuard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := NewPullCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		_ = cmd.Execute()
	})

	for _, want := range []string{"created as a directory", "exists as a FILE is refused", "remove the file"} {
		if !strings.Contains(output, want) {
			t.Errorf("help output missing %q, got:\n%s", want, output)
		}
	}
}
