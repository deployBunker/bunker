// BFS-007: the HTTP/3 (QUIC) listener — the second transport on the SAME port
// number — and the Alt-Svc advertisement that lets a client find it without
// being configured.
//
// BFS-002 measured this design end to end inside this daemon (one port number,
// two transports, one process): a TCP socket whose TLS ALPN selects HTTP/1.1
// or HTTP/2, plus a UDP socket on the SAME port number served by quic-go's
// HTTP/3 server, with the TCP responses carrying `Alt-Svc: h3=":<port>";
// ma=2592000`. The TCP path is untouched by any of it — QUIC is a different
// transport, so a UDP listener cannot change what the TCP socket does, and an
// old client that has never heard of Alt-Svc ignores the header entirely.

package server

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/quic-go/quic-go/http3"

	"github.com/deployBunker/bunker/internal/server/webdav"
)

// h3Listener is a running HTTP/3 server plus the liveness record that both
// advertisements read — the Alt-Svc wrapper below and the surface's capability
// document (`transports.h3`). One record, so they cannot disagree.
type h3Listener struct {
	srv *http3.Server
	ep  *webdav.H3Endpoint
}

// startH3 binds the UDP socket for HTTP/3 on addr and serves handler over QUIC.
//
// handler is the SAME handler the TCP listeners serve — the surface is not
// forked for h3 (no second WebDAV implementation exists or is wanted: a
// duplicate would be a defect in the design, not a detail).
//
// ep is the liveness record the surface's capability document already holds;
// this function owns marking it, because only the code that binds the socket
// can honestly say whether it is up. It must not be nil: an h3 listener with no
// record to mark is a listener nobody can report on.
//
// The socket is bound SYNCHRONOUSLY, before the caller opens any TCP listener,
// and a bind failure is returned as an error: the daemon must refuse to start
// rather than serve a transport it does not have. That is also what makes
// liveness mean something — the endpoint is marked live only after the socket
// exists, so no advertisement can precede the listener.
//
// Serving then runs in a goroutine. If it ever returns, the endpoint is marked
// down BEFORE the error reaches errCh: an Alt-Svc header, or a capability
// document claiming h3, that outlives the socket it names is exactly the
// silent lie C-4 forbids.
func startH3(ep *webdav.H3Endpoint, addr string, tlsCfg *tls.Config, handler http.Handler, logger *slog.Logger, errCh chan<- error) (*h3Listener, error) {
	if ep == nil {
		return nil, fmt.Errorf("http/3 listener requires the shared endpoint record")
	}
	if tlsCfg == nil {
		return nil, fmt.Errorf("http/3 requires tls.enabled: QUIC always encrypts")
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("http/3 address %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("http/3 listen %s/udp: %w", addr, err)
	}
	boundPort := conn.LocalAddr().(*net.UDPAddr).Port

	srv := &http3.Server{
		Addr: addr,
		// Pin the announced port to the socket we actually bound, so
		// quic-go's own generator (`generateAltSvcHeader`) can only ever name
		// the authority this process is listening on.
		Port: boundPort,
		// ConfigureTLSConfig CLONES cfg and sets NextProtos ["h3"] on the copy.
		// The shared *tls.Config must NEVER be handed to a QUIC server with
		// NextProtos assigned: the TCP listener uses the same config, and an
		// `h3` entry there would be offered on the TCP handshake — the one way
		// this wiring can lose `h2`/`http/1.1` for every client (BFS-002 §8 F3).
		TLSConfig: http3.ConfigureTLSConfig(tlsCfg),
		Handler:   handler,
		Logger:    logger,
	}

	ep.SetLive(boundPort)
	go func() {
		err := srv.Serve(conn)
		ep.SetDown()
		errCh <- err
	}()
	return &h3Listener{srv: srv, ep: ep}, nil
}

// withAltSvc wraps a TCP handler so every response it produces advertises the
// live HTTP/3 endpoint, which is the only way a client that knows nothing
// about this deployment discovers it without configuration.
//
// Two rules, both gated on the endpoint record rather than on configuration:
//
//   - No live QUIC socket, no header. AltSvc() returns "" until the socket is
//     bound and "" again the moment the QUIC server stops, so the header is
//     emitted only while h3 is really listening (C-4: advertising an endpoint
//     that is not listening is worse than not advertising at all).
//   - A response that is already HTTP/3 carries no Alt-Svc: the client is on
//     the advertised transport already, so the header would name the transport
//     the request arrived on. It is for h1/h2 responses.
//
// HTTP/1.1 and HTTP/2 are otherwise untouched here — the header is appended to
// whatever the surface produced, exactly as an old client would ignore it.
func (h *h3Listener) withAltSvc(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor < 3 {
			if value := h.ep.AltSvc(); value != "" {
				w.Header().Add("Alt-Svc", value)
			}
		}
		next.ServeHTTP(w, r)
	})
}
