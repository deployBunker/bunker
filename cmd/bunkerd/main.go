// bunkerd — Bunker agent host daemon
// Single binary gRPC+REST server managing per-user Docker hosts.
//
// Architecture:
//   - gRPC on :9090 (TLS + token auth)
//   - REST gateway on :8080 (same auth)
//   - systemd user units for per-agent dockerd lifecycle
//   - Cloudflare Tunnel / Tailscale for public networking
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bunkerd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("bunkerd v0.1.0 — starting")
	// TODO: WI-002 — gRPC server with TLS + token auth
	// TODO: WI-003 — Agent spawn lifecycle
	return nil
}
