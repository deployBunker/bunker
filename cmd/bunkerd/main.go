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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/server"
	"github.com/deployBunker/bunker/internal/version"
)

const defaultConfigPath = "/etc/bunkerd/config.yaml"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bunkerd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Flags
	var (
		showHelp    bool
		showVersion bool
		cfgPath     string
	)

	fs := flag.NewFlagSet("bunkerd", flag.ContinueOnError)
	fs.BoolVar(&showHelp, "help", false, "Show help")
	fs.BoolVar(&showHelp, "h", false, "Show help (shorthand)")
	fs.BoolVar(&showVersion, "version", false, "Print version")
	fs.BoolVar(&showVersion, "v", false, "Print version (shorthand)")
	fs.StringVar(&cfgPath, "config", defaultConfigPath, "Config file path")
	fs.StringVar(&cfgPath, "c", defaultConfigPath, "Config file path (shorthand)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `bunkerd — Bunker agent host daemon

Usage:
  bunkerd [flags]

Flags:
  -h, --help       Show help
  -v, --version    Print version
  -c, --config     Config file path (default: %s)
                   Also settable via BUNKERD_CONFIG env var
                   Example: cp config.example.yaml /etc/bunkerd/config.yaml

bunkerd starts the Bunker gRPC+REST server that manages per-user
Docker agent hosts. Send SIGINT/SIGTERM for graceful shutdown.
`, defaultConfigPath)
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	if showHelp {
		fs.Usage()
		return nil
	}

	if showVersion {
		fmt.Printf("bunkerd %s\n", version.Version)
		fmt.Printf("  commit:     %s\n", version.Commit)
		fmt.Printf("  built:      %s\n", version.BuildDate)
		fmt.Printf("  go version: %s\n", runtime.Version())
		fmt.Printf("  platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
		return nil
	}

	// Config: flag takes priority, then env var
	if cfgPath == defaultConfigPath {
		if envPath := os.Getenv("BUNKERD_CONFIG"); envPath != "" {
			cfgPath = envPath
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Authentication gate: refuse to start when auth is enabled but no
	// credential is configured; warn loudly when auth is explicitly disabled.
	if warn, err := cfg.CheckAuth(); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	} else if warn != "" {
		fmt.Fprintln(os.Stderr, warn)
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
