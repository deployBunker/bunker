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
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/deployBunker/bunker/internal/agent"
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

// printVersion writes the version block to w. The --version flag and the
// `version` positional verb both call it, so the two forms stay byte-identical
// and use one implementation. internal/hostsetup.ParseDaemonVersionOutput
// parses exactly this shape (a "bunkerd <version>" line, then lines prefixed
// commit: / built: / caps:), so field order and indentation are load-bearing.
//
// caps: carries the spawn-side capability tokens this build reports
// (internal/agent.SpawnCapabilities). The installer's daemon-skew probe
// REQUIRES the isolation-grant token, because a version number alone cannot
// prove a capability — a bare `go build` reports the package default version.
func printVersion(w io.Writer) {
	fmt.Fprintf(w, "bunkerd %s\n", version.Version)
	fmt.Fprintf(w, "  commit:     %s\n", version.Commit)
	fmt.Fprintf(w, "  built:      %s\n", version.BuildDate)
	fmt.Fprintf(w, "  caps:       %s\n", strings.Join(agent.SpawnCapabilities(), ","))
	fmt.Fprintf(w, "  go version: %s\n", runtime.Version())
	fmt.Fprintf(w, "  platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
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
  bunkerd version      Print version (same as --version)
  bunkerd help         Show this help (same as --help)

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

	// Positional arguments (GAP-078). Go's flag package stops parsing at the
	// first non-flag token, so without this block `bunkerd version` leaves
	// cfgPath at the default and falls straight through to config.Load and the
	// serve path — loading the real config and racing the running daemon for
	// ports and agent reconciliation. That is the entry path behind the
	// DF-BUNKER-13 second-bunkerd incident. Resolve positionals here, before
	// anything reads config, binds a port or installs a signal handler.
	if args := fs.Args(); len(args) > 0 {
		if len(args) > 1 {
			return fmt.Errorf("unexpected argument %q", args[1])
		}
		switch args[0] {
		case "version":
			printVersion(os.Stdout)
			return nil
		case "help":
			fs.Usage()
			return nil
		default:
			return fmt.Errorf("unknown argument %q", args[0])
		}
	}

	if showHelp {
		fs.Usage()
		return nil
	}

	if showVersion {
		printVersion(os.Stdout)
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

	// Arm the host-level rootless installer cache (GAP-091): env wins over
	// the config file (BUNKERD_* convention), the config file wins over the
	// DefaultConfig default. An empty env var is treated as unset so it
	// never clobbers an explicit config value; an empty config value keeps
	// the legacy uncached download path.
	cacheDir := cfg.Agent.RootlessInstallerCacheDir
	if envCacheDir := os.Getenv(agent.RootlessInstallerCacheDirEnv); envCacheDir != "" {
		cacheDir = envCacheDir
	}
	agent.SetRootlessInstallerCacheDir(cacheDir)

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
