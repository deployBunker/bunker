// Package server provides the bunkerd HTTP/gRPC server.
// Uses chi router with connect-go handlers for gRPC + REST on a single port.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/caddyserver/certmagic"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/hilo"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/deployBunker/bunker/internal/tailscale"
	"github.com/deployBunker/bunker/internal/tlsutil"
	"github.com/deployBunker/bunker/internal/tunnel"
)

// BunkerdServer is the main bunkerd daemon server.
type BunkerdServer struct {
	cfg     *config.Config
	logger  *slog.Logger
	keyMgr  *apikey.Manager
	jwtAuth *auth.JWTAuth
}

// New creates a new BunkerdServer with the given configuration.
func New(cfg *config.Config) *BunkerdServer {
	return &BunkerdServer{
		cfg:    cfg,
		logger: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// Run starts the bunkerd server and blocks until shutdown.
func (s *BunkerdServer) Run(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// Build the chi router with middleware
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	//nolint:staticcheck // RealIP is deprecated but acceptable for our use case
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	// Health check
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Hilo graph endpoint — dependency analysis for the codebase
	hiloGraph, err := hilo.NewGraph(".", s.logger)
	if err != nil {
		s.logger.Warn("hilo graph init failed", "error", err)
	} else {
		r.Get("/graph/stats", func(w http.ResponseWriter, r *http.Request) {
			stats := hiloGraph.Stats()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"total_edges":%d,"unique_files":%d,"unique_deps":%d,"files_with_edges":%d}`,
				stats.TotalEdges, stats.UniqueFiles, stats.UniqueDeps, stats.FilesWithEdges)
		})
		r.Get("/graph/related", func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Query().Get("path")
			if path == "" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"path query param required"}`))
				return
			}
			edges := hiloGraph.Related(path)
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
		r.Get("/graph/impact", func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Query().Get("path")
			if path == "" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"path query param required"}`))
				return
			}
			edges := hiloGraph.Impact(path)
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
	}

	// Build the Bunkerd service handler with master-only auth interceptor
	// The Bunkerd service handles server-level RPCs (spawn, destroy, etc.) and
	// must NOT accept agent-scoped sub-keys — only master tokens are allowed.
	// Agent-scoped keys are handled by the separate Agent service below.
	s.keyMgr = apikey.NewManager(s.cfg.Auth.JWTSecret)
	s.jwtAuth = auth.NewJWTAuth(s.cfg.Auth.JWTSecret, s.keyMgr)
	bunkerdAuthInterceptor := auth.NewMasterOnlyAuthInterceptor(s.cfg.Auth.JWTSecret, s.keyMgr, s.cfg.Auth.Token, s.cfg.Auth.Enabled)
	agentAuthInterceptor := auth.NewJWTAuthInterceptor(s.cfg.Auth.JWTSecret, s.keyMgr, s.cfg.Auth.Token, s.cfg.Auth.Enabled)
	tracker := resource.NewTracker(s.cfg.Agent.MaxAgents, s.logger)
	tunnelMgr := tunnel.NewTunnelManager(&s.cfg.Tunnel, s.logger)
	tailscaleMgr := tailscale.NewTailscaleManager(&s.cfg.Tailscale, s.logger)
	agentMgr := agent.NewAgentManager(s.cfg, s.logger, tracker, tunnelMgr, tailscaleMgr)
	bunkerdSvc := &bunkerdService{cfg: s.cfg, logger: s.logger, agentMgr: agentMgr, tracker: tracker, tunnelMgr: tunnelMgr, tailscaleMgr: tailscaleMgr, keyMgr: s.keyMgr, jwtAuth: s.jwtAuth}
	bunkerdPath, bunkerdHandler := bunkerv1connect.NewBunkerdHandler(
		bunkerdSvc,
		connect.WithInterceptors(bunkerdAuthInterceptor),
	)
	r.Mount(bunkerdPath, bunkerdHandler)

	// Also mount the Agent service with a permissive auth interceptor
	// that accepts both master tokens and agent-scoped sub-keys.
	agentSvc := &agentService{logger: s.logger, tracker: tracker}
	agentPath, agentHandler := bunkerv1connect.NewAgentHandler(
		agentSvc,
		connect.WithInterceptors(agentAuthInterceptor),
	)
	r.Mount(agentPath, agentHandler)

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
	errCh := make(chan error, 2)

	// gRPC listener on Server.GRPCAddr
	go func() {
		srv := &http.Server{
			Addr:      s.cfg.Server.GRPCAddr,
			Handler:   r,
			TLSConfig: tlsConfig,
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
				Addr:      s.cfg.Server.RESTAddr,
				Handler:   r,
				TLSConfig: tlsConfig,
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

func (s *BunkerdServer) buildTLSConfig() (*tls.Config, error) {
	if !s.cfg.TLS.Enabled {
		return nil, nil
	}

	if s.cfg.TLS.AutoTLS {
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
