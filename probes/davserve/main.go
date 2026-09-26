// Command davserve serves the landed bunkerd WebDAV surface on a loopback
// listener, for the bunker-fs client's measurements and manual checks.
//
// It is deliberately NOT bunkerd: a full daemon also reconciles the agent
// registry, which on a host with an empty registry destroys live agents. This
// binary serves the same handler with no agent lifecycle at all.
//
// Usage:
//
//	go run ./probes/davserve --root /path/to/tree [--addr 127.0.0.1:0] [--user u --pass p]
//
// It prints "URL=<surface root>" on stdout once it is listening, then serves
// until SIGINT/SIGTERM.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/deployBunker/bunker/internal/davserve"
)

func main() {
	root := flag.String("root", "", "directory to serve (required)")
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	user := flag.String("user", "", "HTTP Basic username (optional)")
	pass := flag.String("pass", "", "HTTP Basic password (optional)")
	flag.Parse()

	if *root == "" {
		fmt.Fprintln(os.Stderr, "davserve: --root is required")
		os.Exit(2)
	}
	var opts []davserve.Option
	if *user != "" || *pass != "" {
		opts = append(opts, davserve.WithBasicAuth(*user, *pass))
	}
	s, err := davserve.Serve(*root, *addr, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "davserve: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("URL=%s\n", s.URL)
	fmt.Printf("ROOT=%s\n", s.Root)
	os.Stdout.Sync()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = s.Close()
}
