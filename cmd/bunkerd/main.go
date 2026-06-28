// bunkerd — Bunker agent host daemon
// Single binary gRPC+REST server managing per-user Docker hosts.
//
// Architecture:
//   - gRPC on :9090 (TLS + token auth)
//   - REST gateway on :8080 (same handlers, same auth)
//   - systemd user units for per-agent dockerd lifecycle
//   - Cloudflare Tunnel / Tailscale for public networking
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bunkerd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Load configuration
	cfgPath := "/etc/bunkerd/config.yaml"
	if envPath := os.Getenv("BUNKERD_CONFIG"); envPath != "" {
		cfgPath = envPath
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Create and run server
	srv := server.New(cfg)

	// Context that cancels on SIGINT/SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	return srv.Run(ctx)
}
