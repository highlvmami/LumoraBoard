// Command server runs the LumoraBoard backend.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/highlvmami/lumoraboard/backend/internal/room"
	"github.com/highlvmami/lumoraboard/backend/internal/server"
	"github.com/highlvmami/lumoraboard/backend/internal/ws"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := envOr("LUMORA_ADDR", ":8080")
	wsCfg := ws.DefaultConfig()
	// The Vite dev server proxies /ws from :5173, so allow localhost by
	// default; production sets LUMORA_ALLOWED_ORIGINS explicitly.
	wsCfg.OriginPatterns = strings.Split(envOr("LUMORA_ALLOWED_ORIGINS", "localhost:*,127.0.0.1:*"), ",")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := room.NewHub(room.DefaultConfig(), log)
	srv := server.New(server.Config{Addr: addr, ShutdownTimeout: 10 * time.Second}, log, ws.NewHandler(hub, wsCfg, log))

	// The hub and the listener stop together: the first error, or the
	// signal, cancels the group context and the other winds down cleanly.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return hub.Run(gctx) })
	g.Go(func() error { return srv.Run(gctx) })
	return g.Wait()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
