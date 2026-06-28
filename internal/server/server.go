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
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/caddyserver/certmagic"
	"github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/deployBunker/bunker/internal/tunnel"
)

// BunkerdServer is the main bunkerd daemon server.
type BunkerdServer struct {
	cfg    *config.Config
	logger *slog.Logger
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

	// Build the Bunkerd service handler with auth interceptor
	authInterceptor := auth.NewAuthInterceptor(s.cfg.Auth.Token, s.cfg.Auth.Enabled)
	tracker := resource.NewTracker(s.cfg.Agent.MaxAgents, s.logger)
	tunnelMgr := tunnel.NewTunnelManager(&s.cfg.Tunnel, s.logger)
	agentMgr := agent.NewAgentManager(s.cfg, s.logger, tracker, tunnelMgr)
	bunkerdSvc := &bunkerdService{cfg: s.cfg, logger: s.logger, agentMgr: agentMgr, tracker: tracker, tunnelMgr: tunnelMgr}
	bunkerdPath, bunkerdHandler := bunkerv1connect.NewBunkerdHandler(
		bunkerdSvc,
		connect.WithInterceptors(authInterceptor),
	)
	r.Mount(bunkerdPath, bunkerdHandler)

	// Also mount the Agent service
	agentSvc := &agentService{logger: s.logger, tracker: tracker}
	agentPath, agentHandler := bunkerv1connect.NewAgentHandler(
		agentSvc,
		connect.WithInterceptors(authInterceptor),
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
	if s.cfg.TLS.AutoTLS {
		// Use certmagic for automatic Let's Encrypt certificates
		certmagic.DefaultACME.Agreed = true
		certmagic.DefaultACME.Email = s.cfg.TLS.Domain // using domain as email for now

		tlsCfg, err := certmagic.TLS([]string{s.cfg.TLS.Domain})
		if err != nil {
			return nil, fmt.Errorf("certmagic: %w", err)
		}
		return tlsCfg, nil
	}

	// Use file-based certificates
	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
