package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	cmd := NewVersionCommand()

	// Capture stdout since the command uses fmt.Printf
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := cmd.Execute()

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	io.Copy(&buf, r)
	out := buf.String()

	if err != nil {
		t.Fatalf("version command returned error: %v", err)
	}
	if !strings.Contains(out, "bunker") {
		t.Errorf("expected output to contain 'bunker', got: %s", out)
	}
	if !strings.Contains(out, "go version:") {
		t.Errorf("expected output to contain 'go version:', got: %s", out)
	}
	if !strings.Contains(out, "platform:") {
		t.Errorf("expected output to contain 'platform:', got: %s", out)
	}
	if !strings.Contains(out, "commit:") {
		t.Errorf("expected output to contain 'commit:', got: %s", out)
	}
	if !strings.Contains(out, "built:") {
		t.Errorf("expected output to contain 'built:', got: %s", out)
	}
}
