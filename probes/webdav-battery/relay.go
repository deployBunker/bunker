// The delay relay: the instrument that puts a round trip on the wire without
// touching the host.
//
// WHY IT EXISTS. The lever this battery measures — concurrent independent
// requests — is an RTT-borne effect: the study's A/B got 38.47 s for 100
// concurrent HTTP/1.1 requests against 0.79 s over HTTP/2 on a 185.24 ms link
// (docs/prd/PRD-bunker-fs.md:60-61), and 100 sequential requests on the SAME
// server over loopback would take a fraction of a second. A battery run only on
// loopback therefore cannot distinguish "the protocol multiplexes" from "the
// request was cheap", which is the distinction the row exists to make.
//
// WHY NOT `tc netem`: that would need CAP_NET_ADMIN on a host shared with other
// agents (the fleet runs sibling ticks on this box), and changing the loopback
// interface's latency changes every other session's measurements. This relay is
// userspace, binds its own port, and disappears when the battery ends.
//
// WHAT IT IS, PRECISELY. Every forwarded chunk is written after sleeping
// `delay`, in each direction, so a round trip costs about 2x delay more than it
// would directly. It is therefore a LATENCY adder, not a link emulator: it does
// not model bandwidth, and a multi-megabyte body pays one delay per ≤1 MiB chunk
// (so the large read/write cells are reported on the delayed link but are NOT
// used for the concurrency claim — the claim rests on the small-message cells
// and on the whole-tree walk).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// relayChunk is the read size the TCP direction uses. 1 MiB keeps the chunk
// count (and therefore the accidental rate-limiting) small for big bodies while
// still delaying every request/response exchange.
const relayChunk = 1 << 20

// runRelay forwards TCP and UDP from listen to target, delaying every forwarded
// chunk. Both transports are served from ONE port number, mirroring the shape of
// the daemon under test (one port, two transports). It blocks until ctx is done.
func runRelay(ctx context.Context, listen, target string, delay time.Duration, ready func(tcp, udp string)) error {
	tcpLn, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("relay tcp listen %s: %w", listen, err)
	}
	defer func() { _ = tcpLn.Close() }()

	udpLn, err := net.ListenPacket("udp", listen)
	if err != nil {
		return fmt.Errorf("relay udp listen %s: %w", listen, err)
	}
	defer func() { _ = udpLn.Close() }()

	if ready != nil {
		ready(tcpLn.Addr().String(), udpLn.LocalAddr().String())
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		relayTCP(ctx, tcpLn, target, delay)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		relayUDP(ctx, udpLn, target, delay)
	}()

	<-ctx.Done()
	_ = tcpLn.Close()
	_ = udpLn.Close()
	wg.Wait()
	return nil
}

// relayTCP accepts connections and relays them with a per-chunk delay.
func relayTCP(ctx context.Context, ln net.Listener, target string, delay time.Duration) {
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				break
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			up, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
			if err != nil {
				return
			}
			defer func() { _ = up.Close() }()
			var inner sync.WaitGroup
			inner.Add(2)
			go func() { defer inner.Done(); delayedCopy(up, conn, delay) }()
			go func() { defer inner.Done(); delayedCopy(conn, up, delay) }()
			inner.Wait()
		}()
	}
	wg.Wait()
}

// relayUDP relays datagrams in both directions on one socket, remembering the
// client's address the way a NAT does.
func relayUDP(ctx context.Context, pc net.PacketConn, target string, delay time.Duration) {
	up, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return
	}
	var mu sync.Mutex
	var clientAddr net.Addr

	buf := make([]byte, 1<<16)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			continue
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])

		if addr.String() == up.String() {
			mu.Lock()
			dst := clientAddr
			mu.Unlock()
			if dst == nil {
				continue
			}
			go delayedSend(pc, dst, payload, delay)
			continue
		}
		mu.Lock()
		clientAddr = addr
		mu.Unlock()
		go delayedSend(pc, up, payload, delay)
	}
}

func delayedSend(pc net.PacketConn, dst net.Addr, payload []byte, delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay)
	}
	_, _ = pc.WriteTo(payload, dst)
}

// delayedCopy relays src to dst, adding `delay` of latency to every chunk it
// forwards.
//
// THE CRITICAL PROPERTY, and the one the first two versions of this relay got
// wrong: the delay must be measured from when a chunk ARRIVED, not from when the
// single writer goroutine got around to it. Sleeping `delay` inside the write
// loop serializes a burst — N requests that arrive together leave N x delay
// apart — which caps concurrency at 1 no matter what the client does. Measured
// cost of that bug on this battery: HTTP/2 on one connection looked like it
// gained 1.4x from concurrency=8 while HTTP/3 gained 20x, i.e. the instrument
// would have reported the release's central premise as false. A link delays
// packets independently and they travel at the same time, so the reader
// timestamps every chunk and the writer keeps only an ORDERING guarantee.
func delayedCopy(dst io.Writer, src io.Reader, delay time.Duration) {
	type chunk struct {
		data []byte
		at   time.Time
	}
	chunks := make(chan chunk, 256)
	done := make(chan struct{})

	go func() {
		defer close(chunks)
		buf := make([]byte, relayChunk)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				c := chunk{data: make([]byte, n), at: time.Now()}
				copy(c.data, buf[:n])
				select {
				case chunks <- c:
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	defer close(done)

	next := time.Time{}
	for c := range chunks {
		ready := c.at.Add(delay)
		if ready.Before(next) {
			ready = next
		}
		if wait := time.Until(ready); wait > 0 {
			time.Sleep(wait)
		}
		if _, err := dst.Write(c.data); err != nil {
			return
		}
		next = ready
	}
}
