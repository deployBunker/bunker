package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForDockerd_SocketAppears(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Override the process checker so we don't need a real dockerd process.
	originalChecker := dockerdProcessChecker
	dockerdProcessChecker = func(ctx context.Context, username string) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { dockerdProcessChecker = originalChecker })

	root := t.TempDir()
	sockPath := filepath.Join(root, "docker.sock")

	// Create the socket file after a short delay so the polling path is exercised.
	go func() {
		time.Sleep(250 * time.Millisecond)
		if err := os.WriteFile(sockPath, []byte(""), 0644); err != nil {
			t.Logf("background write failed: %v", err)
		}
	}()

	ctx := context.Background()
	if err := waitForDockerd(ctx, "testuser", sockPath, sockPath+".actual", "bunker-docker-test", logger); err != nil {
		t.Fatalf("waitForDockerd failed: %v", err)
	}
}

func TestWaitForDockerd_SocketUnderRunUser(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	originalChecker := dockerdProcessChecker
	dockerdProcessChecker = func(ctx context.Context, username string) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { dockerdProcessChecker = originalChecker })

	root := t.TempDir()
	expectedSock := filepath.Join(root, "docker.sock")
	actualSock := filepath.Join(root, "actual.sock")

	go func() {
		time.Sleep(250 * time.Millisecond)
		if err := os.WriteFile(actualSock, []byte(""), 0644); err != nil {
			t.Logf("background write failed: %v", err)
		}
	}()

	ctx := context.Background()
	if err := waitForDockerd(ctx, "testuser", expectedSock, actualSock, "bunker-docker-test", logger); err != nil {
		t.Fatalf("waitForDockerd failed: %v", err)
	}

	// Verify the symlink was created and points to the actual socket.
	info, err := os.Lstat(expectedSock)
	if err != nil {
		t.Fatalf("expected symlink missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to be a symlink, got mode %v", expectedSock, info.Mode())
	}
	target, err := os.Readlink(expectedSock)
	if err != nil {
		t.Fatalf("readlink failed: %v", err)
	}
	if target != actualSock {
		t.Fatalf("expected symlink target %q, got %q", actualSock, target)
	}
}

func TestWaitForDockerd_Timeout(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	originalChecker := dockerdProcessChecker
	dockerdProcessChecker = func(ctx context.Context, username string) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { dockerdProcessChecker = originalChecker })

	root := t.TempDir()
	sockPath := filepath.Join(root, "docker.sock")

	ctx := context.Background()
	err := waitForDockerd(ctx, "testuser", sockPath, sockPath+".actual", "bunker-docker-test", logger)
	if err == nil {
		t.Fatal("expected timeout error when dockerd process and socket are missing")
	}
	if !strings.Contains(err.Error(), "dockerd did not start") {
		t.Errorf("expected 'dockerd did not start' in error, got: %v", err)
	}
}
