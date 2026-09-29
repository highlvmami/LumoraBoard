// Package server wires the HTTP routes and owns the listener lifecycle.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Config holds the settings the server needs at startup.
type Config struct {
	Addr            string
	ShutdownTimeout time.Duration
}

// Server is the HTTP entry point.
type Server struct {
	cfg  Config
	log  *slog.Logger
	http *http.Server
}

// New builds a Server with its routes registered. ws serves the WebSocket
// endpoint; it may be nil in tests that only need the HTTP routes.
func New(cfg Config, log *slog.Logger, ws http.Handler) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	if ws != nil {
		mux.Handle("GET /ws", ws)
	}

	return &Server{
		cfg: cfg,
		log: log,
		http: &http.Server{
			Addr:              cfg.Addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

// Handler exposes the router, mainly for tests.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve is Run with a caller-supplied listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", ln.Addr().String())
		errCh <- s.http.Serve(ln)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
