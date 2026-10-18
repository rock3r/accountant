package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"accountant/internal/config"
	"accountant/internal/provider"
	"accountant/internal/provider/anthropic"
	"accountant/internal/provider/codex"
	"accountant/internal/proxy"
	"accountant/internal/router"
)

func cmdServe(_ []string, paths config.Paths, stderr io.Writer) int {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	store := storeFor(paths)
	rt := router.New(paths.ConfigFile, store, router.RealClock(), log)
	providers := map[string]provider.Provider{
		"anthropic": anthropic.New(""),
		"codex":     codex.New(""),
	}
	v, err := openVault(paths)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	px := proxy.New(paths.ConfigFile, paths.StateFile, rt, v, providers, log)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: px.Handler(),
		// ReadTimeout and WriteTimeout stay disabled: a write timeout would
		// kill long SSE streams mid-generation.
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("accountant serving", "listen", cfg.Listen, "config", paths.ConfigFile, "state", paths.StateFile)
	fmt.Fprintf(stderr, "note: %s", disclaimer)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
