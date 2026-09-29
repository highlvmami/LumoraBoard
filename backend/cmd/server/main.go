// Command server runs the LumoraBoard backend.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/highlvmami/lumoraboard/backend/internal/auth"
	"github.com/highlvmami/lumoraboard/backend/internal/cluster"
	"github.com/highlvmami/lumoraboard/backend/internal/export"
	"github.com/highlvmami/lumoraboard/backend/internal/room"
	"github.com/highlvmami/lumoraboard/backend/internal/server"
	"github.com/highlvmami/lumoraboard/backend/internal/store"
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

	addr := os.Getenv("LUMORA_ADDR")
	if addr == "" {
		// Hosts such as Render say which port to listen on in PORT.
		addr = ":" + envOr("PORT", "8080")
	}
	// Render sets RENDER_EXTERNAL_URL to the service's public address.
	publicURL := envOr("LUMORA_PUBLIC_URL", envOr("RENDER_EXTERNAL_URL", "http://localhost:5173"))
	wsCfg := ws.DefaultConfig()
	// The Vite dev server proxies /ws from :5173, so localhost is allowed
	// by default, as is the public URL's host.
	origins := "localhost:*,127.0.0.1:*"
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" {
		origins += "," + u.Host
	}
	wsCfg.OriginPatterns = strings.Split(envOr("LUMORA_ALLOWED_ORIGINS", origins), ",")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	roomCfg := room.DefaultConfig()
	var authStore auth.Store = auth.NewMemory()
	var leases store.Leases // set with a database; clustering needs one
	if dsn := os.Getenv("LUMORA_DATABASE_URL"); dsn != "" {
		connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		pool, err := newPool(connectCtx, dsn)
		if err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		// Closed after g.Wait below: the hub has flushed every room by then.
		defer pool.Close()
		boards, err := store.NewPostgres(connectCtx, pool)
		if err != nil {
			return err
		}
		accounts, err := auth.NewPostgres(connectCtx, pool)
		if err != nil {
			return err
		}
		roomCfg.Store, authStore, leases = boards, accounts, boards
		log.Info("persistence enabled", "store", "postgres")
	} else {
		// Kept in process memory so idle boards survive until restart.
		roomCfg.Store = store.NewMemory()
		log.Warn("LUMORA_DATABASE_URL not set; boards and accounts live in memory only")
	}

	authCfg := authConfig(publicURL)
	authSvc := auth.New(authCfg, authStore, log)
	if authSvc.Enabled() {
		log.Info("sign-in enabled")
	} else {
		log.Warn("no sign-in provider configured; everyone can edit every board")
	}

	node, err := clusterNode(leases, addr, log)
	if err != nil {
		return err
	}
	if node != nil {
		roomCfg.Owner = node
	}
	hub := room.NewHub(roomCfg, log)
	wsHandler := ws.NewHandler(hub, wsCfg, log)
	if node != nil {
		node.Attach(hub)
		wsHandler.WithCluster(node, []byte(os.Getenv("LUMORA_CLUSTER_SECRET")))
	}
	var boardAuth ws.Authorizer // nil in open mode
	if authSvc.Enabled() {
		boardAuth = authSvc
		wsHandler.WithAuth(boardAuth)
	}
	chat := ws.NewChatHistory(roomCfg.Store, boardAuth, log)
	exports := export.New(export.Config{}, hub, hub, log)
	exportHandler := export.NewHandler(exports, roomCfg.Store, boardAuth, authCfg.PublicURL, log)
	routes := []server.Routes{authSvc, chat, exportHandler}
	if dir := os.Getenv("LUMORA_STATIC_DIR"); dir != "" {
		// The built frontend, served from the same origin as the API.
		routes = append(routes, server.NewStatic(dir))
	}
	srv := server.New(server.Config{Addr: addr, ShutdownTimeout: 10 * time.Second}, log, wsHandler, routes...)

	// The hub and the listener stop together: the first error, or the
	// signal, cancels the group context and the other winds down cleanly.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return hub.Run(gctx) })
	g.Go(func() error { return exports.Run(gctx) })
	g.Go(func() error { return srv.Run(gctx) })
	if node != nil {
		g.Go(func() error { return node.Run(gctx) })
	}
	err = g.Wait()
	if node != nil {
		// Every room has flushed; hand the boards over now rather than
		// making the other instances wait for the leases to expire.
		node.Release()
	}
	return err
}

// newPool opens the database pool. Hosted Postgres such as Neon suspends
// an idle database and drops its connections, sometimes without a reset
// reaching us; a query on such a connection would hang until TCP gives up.
// So idle connections are retired well before a typical five-minute
// suspend, health checks run often, and keepalives find dead peers.
func newPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConnIdleTime = time.Minute
	cfg.HealthCheckPeriod = 15 * time.Second
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	cfg.ConnConfig.DialFunc = dialer.DialContext
	return pgxpool.NewWithConfig(ctx, cfg)
}

// clusterNode joins a cluster when LUMORA_CLUSTER_SECRET is set. Every
// instance needs the same secret and database, and an address the others
// can reach it on (LUMORA_ADVERTISE_URL, default http://<listen addr>).
func clusterNode(leases store.Leases, addr string, log *slog.Logger) (*cluster.Node, error) {
	secret := os.Getenv("LUMORA_CLUSTER_SECRET")
	if secret == "" {
		return nil, nil
	}
	if leases == nil {
		return nil, errors.New("LUMORA_CLUSTER_SECRET needs LUMORA_DATABASE_URL: instances share boards through the database")
	}
	if len(secret) < 16 {
		return nil, errors.New("LUMORA_CLUSTER_SECRET must be at least 16 characters")
	}
	// Both may name platform variables, e.g. http://[${FLY_PRIVATE_IP}]:8080.
	advertise := os.ExpandEnv(os.Getenv("LUMORA_ADVERTISE_URL"))
	if advertise == "" {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("LUMORA_ADDR %q: %w", addr, err)
		}
		if host == "" {
			host = "127.0.0.1"
		}
		advertise = "http://" + net.JoinHostPort(host, port)
	}
	node := cluster.New(cluster.Config{Instance: os.ExpandEnv(os.Getenv("LUMORA_INSTANCE")), Addr: advertise}, leases, log)
	log.Info("cluster mode", "instance", node.Instance(), "advertise", advertise)
	return node, nil
}

// authConfig reads sign-in settings. A provider is enabled when both its
// client id and secret are set.
func authConfig(publicURL string) auth.Config {
	cfg := auth.Config{
		PublicURL: publicURL,
		DevLogin:  os.Getenv("LUMORA_DEV_LOGIN") == "1",
		Guests:    os.Getenv("LUMORA_GUESTS") == "view",
	}
	if id, secret := os.Getenv("LUMORA_GITHUB_CLIENT_ID"), os.Getenv("LUMORA_GITHUB_CLIENT_SECRET"); id != "" && secret != "" {
		cfg.Providers = append(cfg.Providers, auth.GitHub(id, secret))
	}
	if id, secret := os.Getenv("LUMORA_GOOGLE_CLIENT_ID"), os.Getenv("LUMORA_GOOGLE_CLIENT_SECRET"); id != "" && secret != "" {
		cfg.Providers = append(cfg.Providers, auth.Google(id, secret))
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
