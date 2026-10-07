package cli

import (
	"fmt"
	"strings"
	"testing"
)

// TestPullCommand_Help checks the help text documents the optional
// [local-dir] default and the flags mirrored from cp.
func TestPullCommand_Help(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := NewPullCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		_ = cmd.Execute()
	})

	for _, want := range []string{"[local-dir]", "--ssh-port", "--ssh-key", "--ssh-host", "--recursive", "current working"} {
		if !strings.Contains(output, want) {
			t.Errorf("help output missing %q, got:\n%s", want, output)
		}
	}
}

// TestPullCommand_MissingArgs covers 0/1/2/3 args: fewer than 2 must be a
// usage error, exactly 2 or 3 must pass arg validation.
func TestPullCommand_MissingArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"no args", nil, true},
		{"agent only", []string{"abc123"}, true},
		{"agent + remote path", []string{"abc123", "/tmp/f.txt"}, false},
		{"agent + remote path + local dir", []string{"abc123", "/tmp/f.txt", "."}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			cmd := NewPullCommand()
			// Silence the usage/help dump so failures are readable.
			cmd.SilenceUsage = true
			cmd.SetOut(&discardedBuffer{})
			cmd.SetErr(&discardedBuffer{})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()

			if tt.wantErr && err == nil {
				t.Errorf("args %v: expected usage error, got nil", tt.args)
			}
			if !tt.wantErr && err != nil {
				// Reaching past arg validation is enough: it must NOT be
				// cobra's arg-count error.
				if strings.Contains(err.Error(), "accepts") && strings.Contains(err.Error(), "arg(s)") {
					t.Errorf("args %v: unexpected arg-count error: %v", tt.args, err)
				}
			}
		})
	}
}

// TestBuildPullSCPArgs pins the scp argv: remote source FIRST, local
// destination LAST, and the recursive flag passthrough.
func TestBuildPullSCPArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // mux options create ~/.bunker/mux

	const key = "/home/kara/.bunker/keys/test-agent"

	tests := []struct {
		name      string
		recursive bool
		localDir  string
	}{
		{"plain file", false, "."},
		{"explicit local dir", false, "/tmp/out"},
		{"recursive directory", true, "/tmp/out"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildPullSCPArgs(key, 2222, "bunker-test-agent@bunker-host", "/tmp/test.txt", tt.localDir, tt.recursive)
			joined := strings.Join(args, " ")

			// Connection options mirrored from cp.
			for _, want := range []string{
				"-i " + key,
				"-P 2222",
				"-o IdentitiesOnly=yes",
				"-o ConnectTimeout=10",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("argv %q missing %q", joined, want)
				}
			}
			if tt.recursive && !strings.Contains(joined, " -r ") {
				t.Errorf("recursive argv %q missing -r", joined)
			}
			if !tt.recursive && strings.Contains(joined, " -r ") {
				t.Errorf("non-recursive argv %q contains -r", joined)
			}

			// scp order: remote source first, local destination last.
			wantSource := fmt.Sprintf("%d:%s", 2222, "/tmp/test.txt") // placeholder, replaced below
			_ = wantSource
			srcIdx := -1
			for i, a := range args {
				if a == "bunker-test-agent@bunker-host:/tmp/test.txt" {
					srcIdx = i
				}
			}
			if srcIdx == -1 {
				t.Fatalf("argv %v missing remote source host:path", args)
			}
			if args[srcIdx+1] != tt.localDir {
				t.Errorf("remote source must be immediately followed by local destination: got %q after %q, want %q", args[srcIdx], args[srcIdx+1], tt.localDir)
			}
			if args[len(args)-1] != tt.localDir {
				t.Errorf("local destination must be LAST: got %q", args[len(args)-1])
			}
		})
	}
}

// TestPullCommand_RecursiveFlagPassthrough drives the command with --recursive
// and captures the scp stub's argv, proving the flag reaches scp.
func TestPullCommand_RecursiveFlagPassthrough(t *testing.T) {
	for _, tt := range []struct {
		name      string
		flag      []string
		recursive bool
	}{
		{name: "no flag", flag: nil, recursive: false},
		{name: "--recursive", flag: []string{"--recursive"}, recursive: true},
		{name: "-r shorthand", flag: []string{"-r"}, recursive: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupPullConfig(t)
			record := setupPullScpStub(t)
			_, _, err := runPullCommand(t, tt.flag, "test-agent", "/tmp/remote.txt", ".")
			if err != nil {
				t.Fatalf("pull failed: %v", err)
			}
			argv := readRecordedArgv(t, record)
			gotRecursive := containsArg(argv, "-r")
			if gotRecursive != tt.recursive {
				t.Errorf("scp argv -r = %v, want %v (argv: %v)", gotRecursive, tt.recursive, argv)
			}
		})
	}
}

// TestPullCommand_ScpCommandLineOrder drives the command end-to-end with the
// stubbed scp and asserts the composed command line has the remote source
// before the local destination.
func TestPullCommand_ScpCommandLineOrder(t *testing.T) {
	setupPullConfig(t)
	record := setupPullScpStub(t)

	localDir := t.TempDir()
	_, _, err := runPullCommand(t, nil, "test-agent", "/tmp/remote.txt", localDir)
	if err != nil {
		t.Fatalf("pull failed: %v", err)
	}

	argv := readRecordedArgv(t, record)
	srcIdx := indexOfArg(argv, "bunker-test-agent@127.0.0.1:/tmp/remote.txt")
	if srcIdx == -1 {
		t.Fatalf("scp argv missing remote source; got %v", argv)
	}
	if argv[len(argv)-1] != localDir {
		t.Errorf("local destination must be last: got %q, want %q", argv[len(argv)-1], localDir)
	}
}

// discardedBuffer is a sink for cobra's help/usage output in tests.
type discardedBuffer struct{ data []byte }

func (b *discardedBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}
func (b *discardedBuffer) String() string { return string(b.data) }

func containsArg(argv []string, want string) bool {
	return indexOfArg(argv, want) != -1
}

func indexOfArg(argv []string, want string) int {
	for i, a := range argv {
		if a == want {
			return i
		}
	}
	return -1
}
