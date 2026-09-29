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
	// Version names the running build (a commit), reported by /healthz
	// and /version.
	Version string
}

// Server is the HTTP entry point.
type Server struct {
	cfg  Config
	log  *slog.Logger
	http *http.Server
}

// Routes is anything that adds its own handlers, such as the auth service.
type Routes interface {
	Register(mux *http.ServeMux)
}

// New builds a Server with its routes registered. ws serves the WebSocket
// endpoint; it may be nil in tests that only need the HTTP routes.
func New(cfg Config, log *slog.Logger, ws http.Handler, extra ...Routes) *Server {
	mux := http.NewServeMux()
	health := map[string]string{"status": "ok"}
	if cfg.Version != "" {
		health["version"] = cfg.Version
	}
	mux.HandleFunc("GET /healthz", jsonHandler(health))
	mux.HandleFunc("GET /version", jsonHandler(map[string]string{"version": cfg.Version}))
	if ws != nil {
		mux.Handle("GET /ws", ws)
	}
	for _, r := range extra {
		r.Register(mux)
	}

	return &Server{
		cfg: cfg,
		log: log,
		http: &http.Server{
			Addr:              cfg.Addr,
			Handler:           logUpgrades(log, mux),
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

func jsonHandler(body map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

// logUpgrades logs every request for /ws or asking for a protocol
// upgrade as it arrives, before any handler runs. Behind a hosting proxy
// it tells apart "the socket never reached us" from "we rejected it".
func logUpgrades(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" || r.Header.Get("Upgrade") != "" {
			log.Info("upgrade request", "path", r.URL.Path, "proto", r.Proto,
				"upgrade", r.Header.Get("Upgrade"), "connection", r.Header.Get("Connection"),
				"origin", r.Header.Get("Origin"), "host", r.Host)
		}
		next.ServeHTTP(w, r)
	})
}
