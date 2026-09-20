package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadStdinPayload covers the CLI-side --stdin loader: empty -> nil, a real
// file -> its bytes, "-" -> bunker's own stdin (verified by swapping os.Stdin
// for a pipe that carries the payload).
func TestReadStdinPayload_Empty(t *testing.T) {
	got, err := readStdinPayload("")
	if err != nil || got != nil {
		t.Fatalf("empty --stdin = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestReadStdinPayload_File(t *testing.T) {
	f := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(f, []byte{0x00, 0x01, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readStdinPayload(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string([]byte{0x00, 0x01, 0xff}) {
		t.Errorf("payload = %x, want bytes intact", got)
	}
}

func TestReadStdinPayload_DashPassthrough(t *testing.T) {
	r, w, _ := os.Pipe()
	if _, err := w.Write([]byte("through the pipe\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })

	got, err := readStdinPayload("-")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "through the pipe\n" {
		t.Errorf("dash passthrough = %q", got)
	}
}

func TestReadStdinPayload_MissingFile(t *testing.T) {
	if _, err := readStdinPayload("/nonexistent/payload"); err == nil {
		t.Fatal("expected an error for a missing --stdin file")
	}
}
