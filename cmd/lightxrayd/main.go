// lightxrayd: drop-in Hiddify Panel replacement for xray-core nodes.
//
// Entry point. Wires config → db → xray gRPC client → reconciler → HTTP
// server, then waits for SIGINT/SIGTERM and shuts down cleanly.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/macrodigital283/lightxray/internal/config"
	"github.com/macrodigital283/lightxray/internal/db"
	"github.com/macrodigital283/lightxray/internal/reconciler"
	"github.com/macrodigital283/lightxray/internal/server"
	"github.com/macrodigital283/lightxray/internal/xray"
)

// version is stamped at link time via -ldflags="-X main.version=…".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	slog.Info("starting", "version", version, "addr", cfg.HTTPAddr, "host", cfg.PublicHost)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Postgres ---------------------------------------------------------
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("db open", "err", err)
		os.Exit(1)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		slog.Error("db migrate", "err", err)
		os.Exit(1)
	}

	// --- xray gRPC --------------------------------------------------------
	// Keep retrying — xray may still be coming up when systemd starts us.
	xc, err := xray.DialWithRetry(ctx, cfg.XrayGRPCAddr, cfg.XrayInboundTag, 30*time.Second)
	if err != nil {
		slog.Error("xray dial", "err", err)
		os.Exit(1)
	}
	defer xc.Close()

	// xray's user list lives in-process; if xray restarted, our DB is the
	// source of truth and must be replayed back in.
	if n, err := xc.HydrateFromDB(ctx, store); err != nil {
		slog.Warn("xray hydrate (continuing)", "err", err, "rehydrated", n)
	} else {
		slog.Info("xray hydrated", "users", n)
	}

	// --- background reconciler -------------------------------------------
	rec := reconciler.New(store, xc, cfg)
	go rec.Run(ctx)

	// --- HTTP server ------------------------------------------------------
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.New(cfg, store, xc, version),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		slog.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown")
	sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sd)
}
