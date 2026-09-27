// Command davserve serves the landed bunkerd WebDAV surface on a listener, for
// the bunker-fs client's measurements and manual checks.
//
// It is deliberately NOT bunkerd: a full daemon also reconciles the agent
// registry, which on a host with an empty registry destroys live agents. This
// binary serves the same handler with no agent lifecycle at all.
//
// Usage:
//
//	go run ./probes/davserve --root /path/to/tree [--addr 127.0.0.1:0] [--user u --pass p]
//	go run ./probes/davserve --root /path/to/tree --addr 0.0.0.0:18471 --watch
//
// It prints "URL=<surface root>", "ROOT=<tree>" and — when the watcher is
// configured — one "WATCH=on ..." line carrying the invalidation values the
// endpoint will obey, then serves until SIGINT/SIGTERM.
//
// --watch is what makes this an endpoint that really OFFERS the pushed form
// (BFS-036): without it the handler serves the pre-row surface (no inotify
// watcher on the target, the push form refused, the declared poll form served),
// which is the honest shape for a host that has no watcher — and the reason a
// measurement aimed at the push channel must say which of the two it was aimed
// at. The overrides exist so a measurement can run the channel at the values a
// deployment resolves, rather than at whatever the compiled-in defaults happen
// to be; every value is validated by the SAME validator the surface uses
// (invalidation.Values.Validate), so an endpoint can never be started with a
// pair the table refuses (push_write_deadline < heartbeat_ms).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/deployBunker/bunker/internal/davserve"
	"github.com/deployBunker/bunker/internal/invalidation"
)

func main() {
	root := flag.String("root", "", "directory to serve (required)")
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	user := flag.String("user", "", "HTTP Basic username (optional)")
	pass := flag.String("pass", "", "HTTP Basic password (optional)")
	watch := flag.Bool("watch", false, "establish the inotify watcher on the served tree, so this target really OFFERS the pushed watch form (default: no watcher — the push form is refused and the declared poll form is served, exactly as a host without a watcher behaves)")
	heartbeatMS := flag.Int("heartbeat-ms", 0, "override the declared watch heartbeat period in ms (0 = the declared default)")
	writeDeadlineMS := flag.Int("write-deadline-ms", 0, "override the push write deadline in ms (0 = the declared default; must be strictly below --heartbeat-ms)")
	maxSubscribers := flag.Int("max-subscribers", 0, "override the per-target subscriber cap (0 = the declared default)")
	bufferEvents := flag.Int("subscriber-buffer-events", 0, "override the per-subscriber event-buffer bound (0 = the declared default)")
	flag.Parse()

	if *root == "" {
		fmt.Fprintln(os.Stderr, "davserve: --root is required")
		os.Exit(2)
	}
	var opts []davserve.Option
	if *user != "" || *pass != "" {
		opts = append(opts, davserve.WithBasicAuth(*user, *pass))
	}

	// Any invalidation flag (including --watch) resolves the surface EXPLICITLY
	// and hands it to the endpoint, so the values this endpoint obeys are the
	// values it prints. A flag that cannot be honoured stops the endpoint rather
	// than being silently replaced by a default.
	var (
		resolved  *invalidation.Values
		overrides = *watch || *heartbeatMS != 0 || *writeDeadlineMS != 0 ||
			*maxSubscribers != 0 || *bufferEvents != 0
	)
	if overrides {
		v := invalidation.DefaultValues()
		v.Watch.Enabled = *watch
		if *heartbeatMS != 0 {
			v.Watch.HeartbeatMS = *heartbeatMS
		}
		if *writeDeadlineMS != 0 {
			v.Push.WriteDeadlineMS = *writeDeadlineMS
		}
		if *maxSubscribers != 0 {
			v.Push.MaxSubscribers = *maxSubscribers
		}
		if *bufferEvents != 0 {
			v.Push.SubscriberBufferEvents = *bufferEvents
		}
		if err := v.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "davserve: refusing to start with an invalidation surface the table does not allow: %v\n", err)
			os.Exit(2)
		}
		resolved = &v
		opts = append(opts, davserve.WithInvalidation(v))
	}

	s, err := davserve.Serve(*root, *addr, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "davserve: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("URL=%s\n", s.URL)
	fmt.Printf("ROOT=%s\n", s.Root)
	if resolved != nil {
		fmt.Printf("WATCH=%v heartbeat_ms=%d write_deadline_ms=%d max_subscribers=%d subscriber_buffer_events=%d\n",
			resolved.Watch.Enabled, resolved.Watch.HeartbeatMS, resolved.Push.WriteDeadlineMS,
			resolved.Push.MaxSubscribers, resolved.Push.SubscriberBufferEvents)
	} else {
		fmt.Printf("WATCH=false (no watcher configured: the push form will be refused on this endpoint)\n")
	}
	os.Stdout.Sync()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = s.Close()
}
