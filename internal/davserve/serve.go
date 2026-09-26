// Package davserve serves the landed WebDAV surface (internal/server/webdav) on
// a loopback listener.
//
// WHY THIS EXISTS RATHER THAN JUST RUNNING bunkerd. A bunkerd started for a
// client test also runs the daemon's agent-registry reconciliation, and with an
// empty registry that reconciliation treats LIVE agents on the host as orphans
// and destroys them (measured while building this row: a local bunkerd with an
// empty registry recorded a destroy for two pre-existing `bunker-probeok-*`
// agents and tried to terminate their user sessions). A client's test endpoint
// must not be able to do that, so the endpoint here is the SAME handler the
// daemon mounts — internal/server/webdav.New — served over the same mounting
// shape as server.go's mountWebDAV, with no registry, no agent lifecycle and no
// root.
//
// It is therefore a faithful endpoint for the client and the battery: the same
// RFC 4918 surface, the same urn:bunker:fs:1 extension layer, HTTP/1.1 by
// default (and optionally h2c, which is a server-side opt-in the client must not
// require).
package davserve

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/server/webdav"
)

// Server is one running surface endpoint.
type Server struct {
	// URL is the surface root, e.g. http://127.0.0.1:34567/dav.
	URL string
	// Root is the served directory.
	Root string

	ln   net.Listener
	srv  *http.Server
	done chan struct{}
}

// Option configures the endpoint.
type Option func(*config)

type config struct {
	authenticate func(*http.Request) bool
	h2c          bool
	build        string
	maxResult    int64
}

// WithBasicAuth requires the given credentials (the daemon's own credential
// shape: HTTP Basic at the WebDAV layer).
func WithBasicAuth(user, pass string) Option {
	return func(c *config) {
		c.authenticate = func(r *http.Request) bool {
			u, p, ok := r.BasicAuth()
			return ok && u == user && p == pass
		}
	}
}

// WithResultCap sets the envelope result cap (X-Bunker-Max-Bytes ceiling).
func WithResultCap(n int64) Option { return func(c *config) { c.maxResult = n } }

// Serve starts the surface on addr (use "127.0.0.1:0" for an ephemeral port) and
// returns the running endpoint. The caller closes it.
func Serve(root, addr string, opts ...Option) (*Server, error) {
	cfg := config{build: "bunker-fs-probe"}
	for _, o := range opts {
		o(&cfg)
	}
	h, err := webdav.New(webdav.Config{
		Root:            root,
		Build:           cfg.build,
		Authenticate:    cfg.authenticate,
		DefaultMaxBytes: cfg.maxResult,
		AbsMaxBytes:     cfg.maxResult,
	})
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := &http.ServeMux{}
	prefix := webdav.Prefix
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions && (r.RequestURI == "*" || r.URL.Path == "*") {
			webdav.OptionsAsterisk(w, r)
			return
		}
		if p := r.URL.Path; p == prefix || strings.HasPrefix(p, prefix+"/") {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	base := "http://" + ln.Addr().String() + prefix
	s := &Server{URL: base, Root: h.Root(), ln: ln, srv: srv, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		_ = srv.Serve(ln)
	}()
	return s, nil
}

// Close stops the endpoint.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	<-s.done
	return err
}

// Wait blocks until the server stops (used by the probe binary).
func (s *Server) Wait() { <-s.done }

// String names the endpoint for logs.
func (s *Server) String() string { return fmt.Sprintf("davserve(%s root=%s)", s.URL, s.Root) }
