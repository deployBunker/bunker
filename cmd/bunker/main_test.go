package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/cli"
)

func TestExitCodeFor_ExitError(t *testing.T) {
	code, ok := exitCodeFor(&cli.ExitError{Code: 7})
	if !ok || code != 7 {
		t.Errorf("exitCodeFor(ExitError{7}) = (%d, %v), want (7, true)", code, ok)
	}
}

func TestExitCodeFor_WrappedExitError(t *testing.T) {
	err := fmt.Errorf("exec agent: %w", &cli.ExitError{Code: 127})
	code, ok := exitCodeFor(err)
	if !ok || code != 127 {
		t.Errorf("exitCodeFor(wrapped ExitError) = (%d, %v), want (127, true)", code, ok)
	}
}

func TestExitCodeFor_PlainError(t *testing.T) {
	if code, ok := exitCodeFor(errors.New("boom")); ok || code != 0 {
		t.Errorf("exitCodeFor(plain error) = (%d, %v), want (0, false)", code, ok)
	}
}

func TestExitCodeFor_Nil(t *testing.T) {
	if code, ok := exitCodeFor(nil); ok || code != 0 {
		t.Errorf("exitCodeFor(nil) = (%d, %v), want (0, false)", code, ok)
	}
}

// TestVersionFlagMatchesVersionSubcommand asserts the UX-005 contract: the
// --version flag must print the exact same 5-field block as the `version`
// subcommand (GAP-045).
func TestVersionFlagMatchesVersionSubcommand(t *testing.T) {
	flagCmd := newRootCommand()
	var flagOut bytes.Buffer
	flagCmd.SetOut(&flagOut)
	flagCmd.SetErr(io.Discard)
	flagCmd.SetArgs([]string{"--version"})
	if err := flagCmd.Execute(); err != nil {
		t.Fatalf("--version returned error: %v", err)
	}

	subCmd := newRootCommand()
	var subOut bytes.Buffer
	subCmd.SetOut(&subOut)
	subCmd.SetErr(io.Discard)
	subCmd.SetArgs([]string{"version"})
	if err := subCmd.Execute(); err != nil {
		t.Fatalf("version subcommand returned error: %v", err)
	}

	if flagOut.String() != subOut.String() {
		t.Errorf("--version output differs from version subcommand\n--version:\n%s\nversion subcommand:\n%s", flagOut.String(), subOut.String())
	}

	lines := strings.Split(strings.TrimRight(flagOut.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("--version printed %d lines, want 5 (bunker <ver>, commit, built, go version, platform): %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "bunker ") {
		t.Errorf("first line = %q, want prefix \"bunker \"", lines[0])
	}
}
