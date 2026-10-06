// Package server provides the bunkerd HTTP/gRPC server.
// Uses chi router with connect-go handlers for gRPC + REST on a single port.
package server

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"connectrpc.com/connect"
	"github.com/caddyserver/certmagic"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/audit"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/hilo"
	"github.com/deployBunker/bunker/internal/programalias"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/deployBunker/bunker/internal/server/webdav"
	"github.com/deployBunker/bunker/internal/tailscale"
	"github.com/deployBunker/bunker/internal/tlsutil"
	"github.com/deployBunker/bunker/internal/tunnel"
	"github.com/deployBunker/bunker/internal/version"
)

// BunkerdServer is the main bunkerd daemon server.
type BunkerdServer struct {
	cfg      *config.Config
	logger   *slog.Logger
	keyMgr   *apikey.Manager
	jwtAuth  *auth.JWTAuth
	auditLog *audit.AuditLog
}

// New creates a new BunkerdServer with the given configuration.
func New(cfg *config.Config) *BunkerdServer {
	s := &BunkerdServer{
		cfg:    cfg,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	// Open the daemon-side audit trail when enabled. Failure is non-fatal
	// (warn + run without audit) so a missing/unwritable /var/log/bunkerd
	// never blocks daemon startup on existing deployments.
	//
	// GAP-073: when audit.ship_to is configured, remote shipping is attached
	// here. An INVALID ship_to is non-fatal by design — warn and keep the
	// local log without shipping; the same policy applies if the log itself
	// fails to open. Sealing (audit.seal_key) rides the same options struct.
	if cfg.Audit.Enabled {
		auditOpts := audit.Options{
			ShipTo:  cfg.Audit.ShipTo,
			SealKey: cfg.Audit.SealKey,
			Logger:  s.logger,
			// GAP-126: a daemon serving plaintext on a non-loopback
			// listener under the explicit tls.insecure_dev opt-in stamps
			// every record so the trail states its own transport. The
			// predicate is decided in config so startup and the log agree
			// by construction.
			InsecurePlaintext: cfg.InsecurePlaintextActive(),
		}
		l, err := audit.NewWithOptions(cfg.Audit.Path, auditOpts)
		if err != nil {
			if shipErr := (*audit.InvalidShipToError)(nil); errors.As(err, &shipErr) {
				// Only the ship endpoint was bad: fall back to a plain
				// log (no shipping) instead of losing the audit trail.
				// Fall back through NewWithOptions with the same
				// insecure-transport option — a bad ship_to must not
				// silently UNMARK records.
				s.logger.Warn("audit shipping disabled",
					"ship_to", audit.RedactShipTo(cfg.Audit.ShipTo), "error", err)
				l, err = audit.NewWithOptions(cfg.Audit.Path, audit.Options{
					InsecurePlaintext: auditOpts.InsecurePlaintext,
					Logger:            s.logger,
				})
			}
		}
		if err != nil {
			s.logger.Warn("audit logging disabled", "path", cfg.Audit.Path, "error", err)
		} else {
			s.auditLog = l
		}
	}
	return s
}

// Run starts the bunkerd server and blocks until shutdown.
func (s *BunkerdServer) Run(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	// GAP-126 / REQ-T1 transport gate: refuse to serve a NON-loopback
	// PLAINTEXT listener unless the operator made the explicit
	// tls.insecure_dev opt-in. Mirrors the CheckAuth pattern — it runs here,
	// before any listener opens (the same fail-before-listen point as the
	// durable-registry gate below), so the daemon never puts an
	// admin-capable RPC plane in cleartext on a reachable address by
	// accident. A warning (the opt-in path) is written to the structured
	// logger AND to stderr, so whichever stream a supervisor captures
	// records it.
	if warn, err := s.cfg.CheckTLS(s.cfg.Server.GRPCAddr, s.cfg.Server.RESTAddr); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	} else if warn != "" {
		s.logger.Warn(warn)
		// Belt and braces: the same line on stderr, like the auth gate, so a
		// supervisor that captures only stderr still records it.
		fmt.Fprintln(os.Stderr, warn)
	}
	s.logger.Info("bunkerd config loaded", "max_agents", s.cfg.Agent.MaxAgents)
	// GAP-067: safe startup note about containment disclosure state. Only
	// the boolean is logged — never secrets, tokens, or host paths.
	if s.cfg.Containment.Disclosure {
		s.logger.Info(logDisclosureStartup(true))
	} else {
		s.logger.Info(logDisclosureStartup(false))
	}
	// GAP-116: same safe-startup shape for the effective safety preset. The
	// preset is validated by cfg.Validate() above, so this can only fail on a
	// hand-built config that skipped Validate — and even then the name (never
	// a secret) is the only content in the message.
	if effectivePreset, perr := s.cfg.ResolveSafetyPreset(""); perr != nil {
		s.logger.Warn("bunkerd safety.preset is invalid; spawns will be rejected until the config is fixed", "error", perr)
	} else {
		s.logger.Info("bunkerd safety preset", "preset", effectivePreset)
	}

	// Close the audit trail when the daemon exits.
	if s.auditLog != nil {
		defer s.auditLog.Close()
	}

	// Build the chi router with middleware
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	//nolint:staticcheck // RealIP is deprecated but acceptable for our use case
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(s.cfg.Server.RequestTimeout))

	// Health check
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Hilo graph endpoint — dependency analysis for the codebase.
	//
	// REV-BUNKER-005: the graph is LOADED here, but its routes are registered
	// below, once the credential validator exists: the /graph surface rides
	// the daemon's own credential model, so registration needs the same
	// instance the connect interceptors validate against.
	var hiloGraph *hilo.Graph
	if g, gerr := hilo.NewGraph(".", s.logger); gerr != nil {
		s.logger.Warn("hilo graph init failed", "error", gerr)
	} else {
		hiloGraph = g
	}

	// Build the Bunkerd service handler with master-only auth interceptor
	// The Bunkerd service handles server-level RPCs (spawn, destroy, etc.) and
	// must NOT accept agent-scoped sub-keys — only master tokens are allowed.
	// Agent-scoped keys are handled by the separate Agent service below.
	// GAP-132: the API-key manager must be backed by the durable JSONL key
	// store under the daemon data directory, so issued sub-keys and
	// revocations survive a restart. A daemon that cannot OPEN the store
	// must not serve (the GAP-070 fail-before-listen posture): silently
	// falling back to the in-memory manager would re-create the exact bug
	// GAP-132 fixes (spawn responses handing out tokens the next restart
	// forgets).
	keyMgr, err := apikey.NewManagerAt(s.cfg.Auth.JWTSecret, filepath.Join(s.cfg.Agent.BaseDataDir, "keys"))
	if err != nil {
		return fmt.Errorf("api key store unavailable: %w", err)
	}
	s.keyMgr = keyMgr
	// DF-BUNKER-45: ONE JWTAuth instance is shared between the key-lifecycle
	// RPCs (RotateJWTSecret mutates s.jwtAuth via RotateSecret) and BOTH
	// request-validation interceptors. The previous wiring passed the raw
	// secret string to the string-based factories, which each built a PRIVATE
	// JWTAuth holding the boot secret forever: rotations never took effect on
	// the validating path (live-proven — a JWT minted with the secret the
	// rotate RPC returned was rejected 401 immediately after every rotation).
	// The FromAuth factories wrap/derive from THIS instance, whose rotating
	// secret is shared, so new-secret tokens validate immediately and the
	// retired one dual-accepts through the overlap window (GAP-132) exactly
	// as internal/auth implements it. The static token rides the instance so
	// the fallback behavior matches the old string-based construction.
	s.jwtAuth = auth.NewJWTAuthWithStaticFallback(s.cfg.Auth.JWTSecret, s.cfg.Auth.Token, s.keyMgr)
	// REV-BUNKER-006: the interceptor composition (GAP-133 deny sink + the
	// SEC-15 per-source throttle) has ONE definition, extracted from Run so a
	// test can drive the wiring the daemon actually uses — including the
	// audit-DISABLED configuration, where this block is the whole story.
	bunkerdAuthInterceptor, agentAuthInterceptor := s.buildAuthInterceptors(s.jwtAuth)
	// REV-BUNKER-005: the hilo graph routes expose the daemon host's codebase
	// dependency graph, so they are gated with the SAME credential model as the
	// daemon-level RPC plane — the master-only derivation of the shared JWTAuth
	// instance, so a master token that authenticates an RPC authenticates these
	// routes, a rotation takes effect on both without a restart, and every
	// denial is attributed to the same source in the audit trail and the
	// per-source throttle. A stock authenticated client is unaffected: it
	// already sends `Authorization: Bearer <master token>`.
	//
	// Gating them was preferred over a default-off config flag on three
	// grounds: the endpoint reflects host-CWD structure (anything that can
	// reach the port can map the host), every shipped config runs the RPC plane
	// next to it credentialed, and NOTHING reads these routes — the CLI, the
	// in-process code and the scripts in this repo all stop at /healthz — so a
	// flag would leave a dead surface while gating removes no capability.
	if hiloGraph != nil {
		registerGraphRoutes(r, hiloGraph, graphAuthMiddleware(auth.NewMasterOnlyJWTAuthFromAuth(s.jwtAuth), s.cfg.Auth.Enabled))
		if !s.cfg.Auth.Enabled {
			// Same shape as the WebDAV warn below: with auth off there is no
			// credential check anywhere on this daemon, so these routes are no
			// more open than the RPC plane — but the posture is stated on both
			// streams rather than left silent.
			warn := "bunkerd: auth.enabled is false — the hilo graph routes " +
				"(/graph/stats, /graph/related, /graph/impact) are reachable WITHOUT credentials " +
				"by anyone who can reach the listener"
			s.logger.Warn(warn)
			fmt.Fprintln(os.Stderr, warn)
		}
	}

	tracker := resource.NewTracker(s.cfg.Agent.MaxAgents, s.logger)
	tunnelMgr := tunnel.NewTunnelManager(&s.cfg.Tunnel, s.logger)
	tailscaleMgr := tailscale.NewTailscaleManager(&s.cfg.Tailscale, s.logger)
	agentMgr := agent.NewAgentManager(s.cfg, s.logger, tracker, tunnelMgr, tailscaleMgr)
	// GAP-070: a daemon that cannot persist agent lifecycle state must not
	// serve — every spawn it accepted would be forgotten by the next
	// restart. Fail before opening any listener.
	if err := agentMgr.RegistryError(); err != nil {
		return fmt.Errorf("agent registry unavailable: %w", err)
	}
	// Replay + reconcile BEFORE serving and before the TTL reaper ticks:
	// replayed live agents are restored (with their exact port
	// reservations), stale records are purged, and orphans are destroyed or
	// adopted per agent.reconciliation.mode.
	//
	// INT-CI-043: restore/purge run synchronously (they gate what may be
	// served), but the orphan walk is dispatched to a background goroutine —
	// a daemon starting against a stale registry (live=0, known=709) must
	// not spend its whole readiness window archiving one orphan's home
	// before its first listener exists. The TTL reaper still waits for the
	// full reconciliation (reconcileDone closes only after the walk).
	rep, reconcileFinal := agentMgr.ReconcileStartup(ctx)
	s.logger.Info("agent registry reconciliation dispatched",
		"mode", rep.Mode,
		"replayed_live", rep.ReplayedLive,
		"replayed_known", rep.ReplayedKnown,
		"restored", rep.Restored,
		"restored_foreign_pool", rep.RestoredForeignPool,
		"purged", rep.Purged,
		"orphans_async", true,
	)
	go func() {
		// The final report (orphan counters included) is logged by the
		// reconciliation goroutine itself when the walk completes; drain the
		// final-report channel so the buffered send never leaks.
		<-reconcileFinal
	}()
	bunkerdSvc := &bunkerdService{cfg: s.cfg, logger: s.logger, agentMgr: agentMgr, heartbeats: agentMgr, tracker: tracker, tunnelMgr: tunnelMgr, tailscaleMgr: tailscaleMgr, keyMgr: s.keyMgr, jwtAuth: s.jwtAuth, cpuSampler: resource.NewCPUSampler(), auditLog: s.auditLog}
	// GAP-066: the docker-as-installer program-alias store. It lives beside
	// the agent registry (so a daemon's durable state stays in one directory),
	// and an operator can relocate it with BUNKERD_PROGRAM_ALIASES_PATH. The
	// CLI reaches it through the Bunkerd alias RPCs.
	{
		agentsPath := config.DefaultRegistryPath
		if s.cfg != nil && s.cfg.Agent.Registry.Path != "" {
			agentsPath = s.cfg.Agent.Registry.Path
		}
		bunkerdSvc.programAliases = programalias.NewRegistry(programalias.DefaultPath(agentsPath))
	}
	// DF-BUNKER-34: the orphan-uid probe rides the info/list surfaces. The
	// manager carries the /proc probe; the nil check inside the service keeps
	// tests and unwired services probe-free. PERF-008: the list surface goes
	// through the batch summarizer so one /proc sweep answers the whole
	// fleet; GetAgent keeps the single-id method.
	bunkerdSvc.orphanUIDSummarizer = agentMgr.OrphanUIDSummary
	bunkerdSvc.orphanUIDBatchSummarizer = agentMgr.OrphanUIDSummaries

	// Audit interceptor: composed INSIDE the auth interceptor (auth listed
	// first, so it runs outermost) so only authenticated requests reach it —
	// failed-auth attempts never produce audit records. Caller identity comes
	// from the Claims the auth interceptor placed in the context; the raw
	// token is never written to the log.
	bunkerdInterceptors := []connect.Interceptor{bunkerdAuthInterceptor}
	agentInterceptors := []connect.Interceptor{agentAuthInterceptor}
	if s.auditLog != nil {
		auditInterceptor := audit.NewInterceptor(s.auditLog, s.logger)
		bunkerdInterceptors = append(bunkerdInterceptors, auditInterceptor)
		agentInterceptors = append(agentInterceptors, auditInterceptor)
	}
	bunkerdPath, bunkerdHandler := bunkerv1connect.NewBunkerdHandler(
		bunkerdSvc,
		connect.WithInterceptors(bunkerdInterceptors...),
	)
	// DF-BUNKER-22: wrap both mounts so a streaming RPC reached with the unary
	// REST media type answers 415 with a {"code","message"} envelope instead of
	// connect's body-less 415. Non-streaming paths pass through untouched.
	r.Mount(bunkerdPath, connectStreamingEnvelope(bunkerdHandler))

	// Also mount the Agent service with a permissive auth interceptor
	// that accepts both master tokens and agent-scoped sub-keys.
	agentSvc := &agentService{logger: s.logger, tracker: tracker, heartbeats: agentMgr}
	agentPath, agentHandler := bunkerv1connect.NewAgentHandler(
		agentSvc,
		connect.WithInterceptors(agentInterceptors...),
	)
	r.Mount(agentPath, connectStreamingEnvelope(agentHandler))

	// BFS-006: the WebDAV surface, when the operator opted in. It rides the
	// SAME listeners — and therefore the same HTTP versions — as everything
	// else, so every version a listener negotiates serves the identical
	// surface: HTTP/1.1 always, HTTP/2 via TLS ALPN for free (stdlib), and
	// h2c where server.h2c_enabled opted in (BFS-002 §6/§4). There is no
	// second server, and HTTP/1.1 keeps working unchanged: no method,
	// property or extension of this surface is gated on the negotiated
	// version (§4.3 C-1).
	//
	// The handler is NOT mounted on the chi router. chi's method-agnostic
	// routes cover only the methods in its fixed bitmask (mALL), which has no
	// PROPFIND/MKCOL/COPY/MOVE/PROPPATCH — and no room for an unknown token
	// either. Mounted on the router, a PROPFIND would be answered by chi's
	// own 405 before this surface ever saw it (measured on the live probe
	// before this comment existed), and a FROBNICATE could never reach the
	// 501 method_unknown answer §2.1 requires. Serving the /dav subtree ahead
	// of the router keeps every verb in one handler.
	//
	// BFS-007: the HTTP/3 (QUIC) endpoint record. It is created here — before
	// the surface and before either listener — because two surfaces read it:
	// the capability document's `transports.h3` block and the Alt-Svc wrapper
	// on the TCP listeners. One record, so they cannot disagree; the QUIC
	// socket bound below is what makes it live.
	var h3ep *webdav.H3Endpoint
	if s.cfg.Server.H3Enabled {
		h3ep = webdav.NewH3Endpoint()
	}

	var davHandler http.Handler
	if s.cfg.Server.WebDAVEnabled {
		// BFS-043: the invalidation surface is resolved from the config (the
		// declared defaults for every absent key) and validated before the surface
		// is built, so a knob the daemon cannot obey fails the boot rather than
		// being dropped. webdav.New validates it again — two gates, one table.
		invValues, err := s.cfg.InvalidationValues()
		if err != nil {
			return fmt.Errorf("server.invalidation: %w", err)
		}
		h, err := webdav.New(webdav.Config{
			Root:         s.cfg.Server.WebDAVRoot,
			Build:        version.Version,
			TLS:          s.cfg.TLS.Enabled,
			H2C:          s.cfg.Server.H2CEnabled,
			H3:           h3ep,
			Authenticate: webdavAuthenticator(s.jwtAuth, s.cfg.Auth.Enabled),
			Invalidation: &invValues,
		})
		if err != nil {
			return fmt.Errorf("webdav surface: %w", err)
		}
		davHandler = h
		s.logger.Info("bunkerd WebDAV surface mounted",
			"prefix", webdav.Prefix,
			"root", h.Root(),
			"auth_required", s.cfg.Auth.Enabled,
			"h2c", s.cfg.Server.H2CEnabled,
			"tls", s.cfg.TLS.Enabled,
		)
		if !s.cfg.Auth.Enabled {
			// A served tree with the daemon's auth disabled is reachable by
			// anyone who can reach the listener. Say so on both streams, the
			// same way the plaintext gate does.
			warn := "bunkerd: server.webdav_enabled is true and auth.enabled is false — " +
				"the served tree is reachable WITHOUT credentials by anyone who can reach the listener"
			s.logger.Warn(warn)
			fmt.Fprintln(os.Stderr, warn)
		}
	}

	// BFS-006: the WebDAV subtree and the asterisk-form OPTIONS are both
	// answered ahead of the router (see the note where davHandler is built).
	// The asterisk wrapper goes outermost so `OPTIONS *` — which has no path
	// for any router to match and is not under /dav — is answered too.
	var rootHandler http.Handler = r
	if davHandler != nil {
		rootHandler = mountWebDAV(rootHandler, webdav.Prefix, davHandler)
		rootHandler = asteriskOptions(rootHandler)
	}

	// Determine TLS config
	var tlsConfig *tls.Config
	if s.cfg.TLS.Enabled {
		var err error
		tlsConfig, err = s.buildTLSConfig()
		if err != nil {
			return fmt.Errorf("tls config: %w", err)
		}
	}

	// Start servers
	// Capacity 3: the gRPC listener, the REST listener, and (BFS-007) the
	// HTTP/3 server.
	errCh := make(chan error, 3)

	// BFS-006/BFS-002: the protocol set for both listeners. nil (the default)
	// keeps the runtime behaviour: HTTP/1.1 always, plus HTTP/2 whenever TLS
	// is on. The h2c opt-in adds cleartext prior-knowledge h2 — and states
	// SetHTTP2(true) explicitly, because a non-nil *http.Protocols REPLACES
	// the default set and would otherwise switch h2-over-TLS off.
	protocols := webdav.ServeProtocols(s.cfg.Server.H2CEnabled)
	if warn := s.cfg.CheckH2C(); warn != "" {
		s.logger.Warn(warn)
		fmt.Fprintln(os.Stderr, warn)
	}

	// net/http answers `OPTIONS *` itself (200 + Content-Length: 0) before any
	// handler runs, unless this is disabled. With the WebDAV surface mounted
	// the asterisk-form request is answered by the surface instead, so RFC
	// 4918 §10.1's "200 without DAV" is joined by the Allow list a WebDAV
	// client expects; without the surface, the runtime's default handling is
	// left exactly as it was.
	disableGeneralOptions := davHandler != nil

	// BFS-007: bind and start the HTTP/3 (QUIC) listener BEFORE the TCP
	// listeners open. The order is the point: a taken UDP port is a startup
	// failure, not a daemon that comes up announcing a transport it does not
	// serve. `tlsConfig` is non-nil whenever h3 can be enabled — Validate()
	// refuses the h3-without-TLS pair outright — and the nil guard below keeps
	// a hand-built config from reaching a QUIC server with no certificate.
	//
	// The QUIC listener serves rootHandler itself: the SAME handler as the TCP
	// listeners, no forked surface, no per-version code path.
	var h3 *h3Listener
	if h3ep != nil {
		if tlsConfig == nil {
			return fmt.Errorf("server.h3_enabled requires tls.enabled: QUIC always encrypts")
		}
		h3Addr := s.cfg.H3ListenAddr()
		h3, err = startH3(h3ep, h3Addr, tlsConfig, rootHandler, s.logger, errCh)
		if err != nil {
			return err
		}
		h3Port, _ := h3.ep.Port()
		s.logger.Info("bunkerd HTTP/3 listening",
			"addr", h3Addr, "udp_port", h3Port, "alt_svc", h3.ep.AltSvc(), "tls", true)
	}

	// The TCP listeners answer with the surface plus the Alt-Svc advertisement
	// for the live QUIC endpoint; the QUIC listener answers with the surface
	// alone — a client already on h3 needs no advertisement. Nothing else about
	// the TCP path changes: it is the same rootHandler, wrapped.
	tcpHandler := rootHandler
	if h3 != nil {
		tcpHandler = h3.withAltSvc(rootHandler)
	}

	// gRPC listener on Server.GRPCAddr
	go func() {
		srv := &http.Server{
			Addr:                         s.cfg.Server.GRPCAddr,
			Handler:                      tcpHandler,
			TLSConfig:                    tlsConfig,
			Protocols:                    protocols,
			DisableGeneralOptionsHandler: disableGeneralOptions,
		}
		s.logger.Info("bunkerd gRPC listening", "addr", s.cfg.Server.GRPCAddr, "tls", s.cfg.TLS.Enabled)
		if tlsConfig != nil {
			errCh <- srv.ListenAndServeTLS("", "")
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()

	// REST listener on Server.RESTAddr (optional, may be empty)
	if s.cfg.Server.RESTAddr != "" && s.cfg.Server.RESTAddr != s.cfg.Server.GRPCAddr {
		go func() {
			srv := &http.Server{
				Addr:                         s.cfg.Server.RESTAddr,
				Handler:                      tcpHandler,
				TLSConfig:                    tlsConfig,
				Protocols:                    protocols,
				DisableGeneralOptionsHandler: disableGeneralOptions,
			}
			s.logger.Info("bunkerd REST listening", "addr", s.cfg.Server.RESTAddr, "tls", s.cfg.TLS.Enabled)
			if tlsConfig != nil {
				errCh <- srv.ListenAndServeTLS("", "")
			} else {
				errCh <- srv.ListenAndServe()
			}
		}()
	}

	// Wait for shutdown signal or error
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		s.logger.Info("shutting down", "signal", sig.String())
		return nil
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("server error: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acmeProxyHook is a TEST SEAM for the certmagic auto-TLS path. When non-nil,
// buildTLSConfig installs it as the ACME issuer's HTTP proxy selector, which
// certmagic uses as the `Proxy` hook of the transport behind its ACME client.
//
// Why a proxy selector and not an *http.Client: certmagic v0.25.4 builds the
// ACME client's transport internally (ACMEIssuer.httpClient is unexported and
// is derived from the issuer template at acmeissuer.go NewACMEIssuer), so the
// only exported knob that governs EVERY outbound ACME request — directory
// fetch, nonce, account, order — is ACMEIssuer.HTTPProxy, which
// github.com/mholt/acmez receives as its `HTTPClient`'s transport proxy.
//
// Production leaves this nil, in which case buildTLSConfig touches nothing and
// certmagic keeps its own default (http.ProxyFromEnvironment). Tests set it to
// a recorder that counts requests by host and refuses anything that is not
// loopback, so no unit test in this package can reach a public CA (INT-CI-016).
var acmeProxyHook func(*http.Request) (*url.URL, error)

func (s *BunkerdServer) buildTLSConfig() (*tls.Config, error) {
	if !s.cfg.TLS.Enabled {
		return nil, nil
	}

	if s.cfg.TLS.AutoTLS {
		// Test seam (INT-CI-016): certmagic constructs a fresh ACMEIssuer from
		// the current DefaultACME value on every NewDefault() call, so the hook
		// must be installed before certmagic.TLS() below.
		if acmeProxyHook != nil {
			certmagic.DefaultACME.HTTPProxy = acmeProxyHook
		}
		// Use certmagic for automatic Let's Encrypt certificates
		certmagic.DefaultACME.Agreed = true
		email := s.cfg.TLS.Domain
		if email == "" {
			return nil, fmt.Errorf("tls.domain is required when auto_tls is enabled")
		}
		// Ensure the contact email looks like a valid email address. certmagic
		// uses this as the ACME account contact; a bare domain is rejected by
		// Let's Encrypt with invalidContact.
		if !strings.Contains(email, "@") {
			certmagic.DefaultACME.Email = "admin@" + email
		} else {
			certmagic.DefaultACME.Email = email
		}

		tlsCfg, err := certmagic.TLS([]string{s.cfg.TLS.Domain})
		if err != nil {
			return nil, fmt.Errorf("certmagic: %w", err)
		}
		if s.cfg.TLS.MTLS {
			return nil, fmt.Errorf("mtls is not supported with auto_tls")
		}
		return tlsCfg, nil
	}

	if s.cfg.TLS.SelfSigned {
		hosts := s.cfg.TLS.Hosts
		if len(hosts) == 0 {
			hosts = []string{"localhost"}
		}
		tlsCfg, err := tlsutil.LoadOrGenerateSelfSigned(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile, hosts)
		if err != nil {
			return nil, fmt.Errorf("self-signed cert: %w", err)
		}
		if s.cfg.TLS.MTLS {
			mtlsCfg, err := auth.BuildMTLSConfig(s.cfg.TLS.CAFile)
			if err != nil {
				return nil, fmt.Errorf("mtls: %w", err)
			}
			tlsCfg.ClientCAs = mtlsCfg.ClientCAs
			tlsCfg.ClientAuth = mtlsCfg.ClientAuth
		}
		return tlsCfg, nil
	}

	if s.cfg.TLS.MTLS {
		if s.cfg.TLS.CAFile == "" {
			return nil, fmt.Errorf("tls.ca_file is required for mtls")
		}
		return auth.BuildMTLSConfigWithCert(s.cfg.TLS.CAFile, s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	}

	// Use file-based certificates (standard TLS)
	if s.cfg.TLS.CertFile == "" {
		return nil, fmt.Errorf("tls.cert_file is required when TLS is enabled without auto_tls")
	}
	if s.cfg.TLS.KeyFile == "" {
		return nil, fmt.Errorf("tls.key_file is required when TLS is enabled without auto_tls")
	}
	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// mountWebDAV serves the WebDAV surface for prefix and everything under it,
// ahead of the router.
//
// It is deliberately not a chi mount: chi matches method-agnostic routes only
// for the methods in its fixed bitmask (mALL — CONNECT/DELETE/GET/HEAD/OPTIONS/
// PATCH/POST/PUT/QUERY/TRACE), so PROPFIND, MKCOL, COPY, MOVE, PROPPATCH and
// every unknown method token would be answered by chi's own 405 handler before
// the surface saw them. The prefix test is path-component exact, so /davfoo is
// not captured.
func mountWebDAV(next http.Handler, prefix string, dav http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p == prefix || strings.HasPrefix(p, prefix+"/") {
			dav.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// asteriskOptions answers the asterisk-form OPTIONS (RFC 4918 §10.1) before
// the router sees it: there is no path to match, and the answer deliberately
// carries no DAV header, so a client cannot read whole-server support out of
// an unper-URI request. Only installed while the WebDAV surface is mounted.
func asteriskOptions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions && (r.RequestURI == "*" || r.URL.Path == "*") {
			webdav.OptionsAsterisk(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// registerGraphRoutes wires the hilo dependency-graph HTTP endpoints onto r,
// behind gate.
//
// It is a function (not inline in Run) so a test can build the SAME router the
// daemon serves and observe the status a real client gets — the credential
// gate on these routes is then pinned by the daemon's own registration path
// rather than by a hand-built lookalike.
//
// REV-BUNKER-005: these routes are on the raw router, which serves every HTTP
// version on every listener, so the gate must be here — at registration,
// where the daemon's router is composed. The gate is a REQUIRED parameter:
// there is no path through this function that leaves the surface open.
func registerGraphRoutes(r chi.Router, graph *hilo.Graph, gate func(http.Handler) http.Handler) {
	r.Group(func(gr chi.Router) {
		gr.Use(gate)
		gr.Get("/graph/stats", func(w http.ResponseWriter, r *http.Request) {
			stats := graph.Stats()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"total_edges":%d,"unique_files":%d,"unique_deps":%d,"files_with_edges":%d}`,
				stats.TotalEdges, stats.UniqueFiles, stats.UniqueDeps, stats.FilesWithEdges)
		})
		gr.Get("/graph/related", func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Query().Get("path")
			if path == "" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"path query param required"}`))
				return
			}
			edges := graph.Related(path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"edges":[`))
			for i, e := range edges {
				if i > 0 {
					w.Write([]byte(","))
				}
				fmt.Fprintf(w, `{"from":"%s","to":"%s","rel":"%s"}`, e.From, e.To, e.Rel)
			}
			w.Write([]byte(`]}`))
		})
		gr.Get("/graph/impact", func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Query().Get("path")
			if path == "" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"path query param required"}`))
				return
			}
			edges := graph.Impact(path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"edges":[`))
			for i, e := range edges {
				if i > 0 {
					w.Write([]byte(","))
				}
				fmt.Fprintf(w, `{"from":"%s","to":"%s","rel":"%s"}`, e.From, e.To, e.Rel)
			}
			w.Write([]byte(`]}`))
		})
	})
}

// graphAuthMiddleware is the credential gate for the daemon's plain-HTTP
// /graph surface (REV-BUNKER-005).
//
// It is the auth package's bearer gate over the SAME validator instance the
// connect interceptors use — here the master-only derivation, because the
// graph describes the HOST's codebase and therefore belongs to the
// daemon-level plane (the Bunkerd service), not to an agent-scoped one. A
// master token that authenticates an RPC authenticates these routes; an
// agent-scoped sub-key does not. Secret rotation and the SEC-15 per-source
// throttle apply here exactly as they do to the RPCs, because the state is
// shared rather than re-implemented.
func graphAuthMiddleware(jwa *auth.JWTAuth, authEnabled bool) func(http.Handler) http.Handler {
	return auth.RequireBearerHTTP(jwa, authEnabled)
}

// buildAuthInterceptors composes the two request-validation interceptors the
// daemon mounts (master-only for the Bunkerd service, permissive for the Agent
// service) and, when the daemon has an audit trail, attaches the GAP-133 deny
// sink to each of them.
//
// GAP-133: authentication denials are composed BEFORE the audit interceptor in
// the chain (auth runs outermost), so they would otherwise never reach it —
// exactly why denials were invisible. Attaching the sink directly to the auth
// interceptors appends each denial through the SAME AuditLog the audit
// interceptor uses, so the hash chain stays intact. The sink maps the denial
// to the same Record shape, with the presented token reduced to a SHA-256
// fingerprint (never the secret).
//
// REV-BUNKER-006: the deny sink is OPTIONAL and the SEC-15 throttle is NOT.
// The sink is what makes denials *visible*; the throttle is what makes them
// *cost* something. Gating the throttle on the sink meant an operator running
// with audit.enabled:false — or whose audit path had become unwritable — lost
// the brute-force backoff at the exact moment they were least able to see
// denials. The throttle is therefore armed by the auth constructors whenever
// auth is enabled, and this function only decides whether denial RECORDS ride
// the sink. It must never re-introduce a sink→throttle dependency.
func (s *BunkerdServer) buildAuthInterceptors(jwtAuth *auth.JWTAuth) (bunkerd, agent connect.Interceptor) {
	bunkerdAuthInterceptor := auth.NewMasterOnlyAuthInterceptorFromAuth(jwtAuth, s.cfg.Auth.Enabled)
	agentAuthInterceptor := auth.NewJWTAuthInterceptorFromAuth(jwtAuth, s.cfg.Auth.Enabled)
	if s.auditLog != nil {
		sink := &authDenySink{log: s.auditLog}
		bunkerdAuthInterceptor = auth.AttachDenySink(bunkerdAuthInterceptor, sink.record)
		agentAuthInterceptor = auth.AttachDenySink(agentAuthInterceptor, sink.record)
	}
	return bunkerdAuthInterceptor, agentAuthInterceptor
}

// webdavAuthenticator builds the WebDAV mount's credential check out of the
// daemon's OWN credential model: the same JWTAuth instance the connect
// interceptors validate against, so a token that authenticates an RPC
// authenticates the mount and a rotation takes effect on both. Returning nil
// means "the daemon has auth disabled" — the caller warns loudly about that
// rather than pretending the mount is protected.
//
// Both credential forms a WebDAV client can send are accepted:
//
//   - `Authorization: Bearer <token>` — our own client;
//   - `Authorization: Basic base64(<user>:<token>)` — every stock WebDAV
//     client (davfs2, rclone, Finder), which is the form the spec's O-6
//     adopts for v1. The token may travel in either the password or the
//     username slot, because clients disagree about which one is "secret".
func webdavAuthenticator(jwtAuth *auth.JWTAuth, enabled bool) func(*http.Request) bool {
	if !enabled || jwtAuth == nil {
		return nil
	}
	return func(r *http.Request) bool {
		for _, token := range presentedCredentials(r) {
			if _, err := jwtAuth.AuthenticateRawToken(token, "webdav", r.Method+" "+r.URL.Path); err == nil {
				return true
			}
		}
		return false
	}
}

// presentedCredentials extracts the credential(s) a WebDAV request carries.
// The raw token is never logged or returned to the caller — only validated.
func presentedCredentials(r *http.Request) []string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return nil
	}
	scheme, value, found := strings.Cut(header, " ")
	if !found {
		return nil
	}
	value = strings.TrimSpace(value)
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		if value == "" {
			return nil
		}
		return []string{value}
	case strings.EqualFold(scheme, "Basic"):
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return nil
		}
		out := make([]string, 0, 2)
		for _, candidate := range []string{pass, user} {
			if candidate != "" {
				out = append(out, candidate)
			}
		}
		return out
	}
	return nil
}
