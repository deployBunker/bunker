package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindHostPublicKey_None(t *testing.T) {
	// Save and restore the global hostKeyPaths.
	orig := hostKeyPaths
	defer func() { hostKeyPaths = orig }()

	hostKeyPaths = []string{"/tmp/bunker-test-nonexistent-key.pub"}

	key, path := findHostPublicKey()
	if key != "" {
		t.Errorf("expected empty key, got %q", key)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}

func TestFindHostPublicKey_Found(t *testing.T) {
	orig := hostKeyPaths
	defer func() { hostKeyPaths = orig }()

	// Write a fake host key.
	dir := t.TempDir()
	fakeKeyPath := filepath.Join(dir, "id_ed25519.pub")
	const fakeKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI fake-host-key bunker-host"
	if err := os.WriteFile(fakeKeyPath, []byte(fakeKey+"\n"), 0644); err != nil {
		t.Fatalf("write fake host key: %v", err)
	}

	hostKeyPaths = []string{fakeKeyPath}

	key, path := findHostPublicKey()
	if key != fakeKey+"\n" {
		t.Errorf("expected key %q, got %q", fakeKey+"\n", key)
	}
	if path != fakeKeyPath {
		t.Errorf("expected path %q, got %q", fakeKeyPath, path)
	}
}

func TestFindHostPublicKey_EmptyFile(t *testing.T) {
	orig := hostKeyPaths
	defer func() { hostKeyPaths = orig }()

	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "id_ed25519.pub")
	if err := os.WriteFile(emptyPath, []byte("   \n  "), 0644); err != nil {
		t.Fatalf("write empty host key: %v", err)
	}

	hostKeyPaths = []string{emptyPath}

	key, path := findHostPublicKey()
	if key != "" {
		t.Errorf("expected empty key for whitespace-only file, got %q", key)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}

func TestFindHostPublicKey_SecondPath(t *testing.T) {
	orig := hostKeyPaths
	defer func() { hostKeyPaths = orig }()

	dir := t.TempDir()
	// First path doesn't exist.
	missingPath := filepath.Join(dir, "nonexistent.pub")
	// Second path exists.
	keyPath := filepath.Join(dir, "id_rsa.pub")
	const fakeKey = "ssh-rsa AAAAB3NzaC1yc2E... bunker-host"
	if err := os.WriteFile(keyPath, []byte(fakeKey+"\n"), 0644); err != nil {
		t.Fatalf("write fake host key: %v", err)
	}

	hostKeyPaths = []string{missingPath, keyPath}

	key, path := findHostPublicKey()
	if key != fakeKey+"\n" {
		t.Errorf("expected key %q, got %q", fakeKey+"\n", key)
	}
	if path != keyPath {
		t.Errorf("expected path %q, got %q", keyPath, path)
	}
}

func TestProvisionHostSSHKey_NoKey(t *testing.T) {
	orig := hostKeyPaths
	defer func() { hostKeyPaths = orig }()

	hostKeyPaths = []string{"/tmp/bunker-test-nonexistent-key.pub"}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dir := t.TempDir()
	authKeysFile := filepath.Join(dir, "authorized_keys")
	// Pre-populate with an existing key (as spawn would).
	const existingContent = `environment="DOCKER_HOST=unix:///run/bunker/test/docker.sock" ssh-ed25519 AAA... bunker-test
`
	if err := os.WriteFile(authKeysFile, []byte(existingContent), 0600); err != nil {
		t.Fatalf("write initial authorized_keys: %v", err)
	}

	// Should succeed (warn only) when no host key exists.
	err := provisionHostSSHKey(context.Background(), "bunker-test", authKeysFile, logger)
	if err != nil {
		t.Fatalf("provisionHostSSHKey should not error when no host key: %v", err)
	}

	// authorized_keys content should be unchanged.
	content, err := os.ReadFile(authKeysFile)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if string(content) != existingContent {
		t.Errorf("authorized_keys was modified when no host key exists\ngot: %q\nwant: %q", string(content), existingContent)
	}
}

func TestProvisionHostSSHKey_Found(t *testing.T) {
	orig := hostKeyPaths
	defer func() { hostKeyPaths = orig }()

	dir := t.TempDir()

	// Write a fake host key.
	hostKeyPath := filepath.Join(dir, "host-id_ed25519.pub")
	const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI host-key bunker-host"
	if err := os.WriteFile(hostKeyPath, []byte(hostKey+"\n"), 0644); err != nil {
		t.Fatalf("write fake host key: %v", err)
	}
	hostKeyPaths = []string{hostKeyPath}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	authKeysFile := filepath.Join(dir, "authorized_keys")
	// Pre-populate with an existing key (as spawn would).
	const existingContent = `environment="DOCKER_HOST=unix:///run/bunker/test/docker.sock" ssh-ed25519 AAA... bunker-test
`
	if err := os.WriteFile(authKeysFile, []byte(existingContent), 0600); err != nil {
		t.Fatalf("write initial authorized_keys: %v", err)
	}

	err := provisionHostSSHKey(context.Background(), "bunker-test", authKeysFile, logger)
	if err != nil {
		t.Fatalf("provisionHostSSHKey: %v", err)
	}

	// authorized_keys should contain both the original key and the host key.
	content, err := os.ReadFile(authKeysFile)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	got := string(content)
	if !strings.Contains(got, existingContent) {
		t.Errorf("original authorized_keys content missing\ngot: %q", got)
	}
	wantHostKey := hostKey + "\n"
	if !strings.Contains(got, wantHostKey) {
		t.Errorf("host key not found in authorized_keys\ngot: %q\nwant substring: %q", got, wantHostKey)
	}
	// The host key should be on its own line at the end.
	if !strings.HasSuffix(got, wantHostKey) {
		t.Errorf("host key should be the last line in authorized_keys\ngot: %q", got)
	}
}

func TestProvisionHostSSHKey_MissingFile(t *testing.T) {
	orig := hostKeyPaths
	defer func() { hostKeyPaths = orig }()

	dir := t.TempDir()
	hostKeyPath := filepath.Join(dir, "host-id_ed25519.pub")
	const hostKey = "ssh-ed25519 AAA... host-key"
	if err := os.WriteFile(hostKeyPath, []byte(hostKey+"\n"), 0644); err != nil {
		t.Fatalf("write fake host key: %v", err)
	}
	hostKeyPaths = []string{hostKeyPath}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Point to a nonexistent authorized_keys file.
	authKeysFile := filepath.Join(dir, "nonexistent", "authorized_keys")

	err := provisionHostSSHKey(context.Background(), "bunker-test", authKeysFile, logger)
	if err == nil {
		t.Fatal("expected error when authorized_keys doesn't exist")
	}
}
