// BFS-007: the HTTP/3 (QUIC) liveness record and the Alt-Svc payload.

package webdav

import (
	"fmt"
	"sync"
)

// AltSvcValue is the Alt-Svc payload that announces the HTTP/3 endpoint of a
// listener bound on port, in the exact shape quic-go itself generates
// (`http3.Server.generateAltSvcHeader`: `h3=":<port>"; ma=2592000`, 30 days).
//
// It lives here, and in ONE place, because two surfaces advertise the same
// endpoint — the `Alt-Svc` header on h1/h2 responses and the capability
// document's `transports.h3.alt_svc` field — and a process that names two
// different authorities for one socket is lying in at least one of them. The
// literal is pinned to quic-go's own generator by
// TestAltSvcValueMatchesQUICGoGeneratedHeader in package server, so a quic-go
// bump that changes the format fails a test instead of drifting silently.
func AltSvcValue(port int) string {
	return fmt.Sprintf("h3=\":%d\"; ma=2592000", port)
}

// H3Endpoint is the ONE record of "is this process serving HTTP/3 right now,
// and on which UDP port?".
//
// The daemon creates one, hands it to the surface (whose capability document
// reports `transports.h3`) and to the Alt-Svc wrapper on the TCP listeners,
// and marks it live only AFTER the QUIC socket is bound. Both advertisements
// therefore read the same fact, and neither can claim h3 while the socket is
// absent — the silent-lie class C-4 names ("advertising an endpoint that is
// not listening is worse than not advertising at all").
//
// A nil *H3Endpoint is valid and means "no QUIC listener": every method is
// nil-receiver safe, so a cleartext daemon, an h3-disabled daemon and a unit
// test all report h3 unavailable without a sentinel value.
type H3Endpoint struct {
	mu   sync.RWMutex
	port int
}

// NewH3Endpoint returns an endpoint that is NOT live. The listener marks it
// live only once its UDP socket is bound (SetLive).
func NewH3Endpoint() *H3Endpoint { return &H3Endpoint{} }

// SetLive records that a QUIC socket is bound on port and this process serves
// HTTP/3 there. The caller must hold the socket: liveness is a property of the
// bound socket, never of the operator's intent.
func (e *H3Endpoint) SetLive(port int) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.port = port
}

// SetDown records that the QUIC listener has stopped serving. Every caller
// that returns an error out of the QUIC serve loop MUST call this before
// reporting it, so an advertisement cannot outlive the socket.
func (e *H3Endpoint) SetDown() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.port = 0
}

// Port reports the UDP port HTTP/3 is served on and whether it is live. A nil
// endpoint, and an endpoint that was never marked live, report (0, false).
func (e *H3Endpoint) Port() (int, bool) {
	if e == nil {
		return 0, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.port <= 0 {
		return 0, false
	}
	return e.port, true
}

// AltSvc returns the Alt-Svc payload to advertise, or "" while h3 is not live.
// An empty string is the whole gate: a caller that writes the returned value
// only when it is non-empty cannot advertise a listener that is not there.
func (e *H3Endpoint) AltSvc() string {
	port, live := e.Port()
	if !live {
		return ""
	}
	return AltSvcValue(port)
}
