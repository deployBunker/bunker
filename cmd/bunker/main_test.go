package main

import (
	"errors"
	"fmt"
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
