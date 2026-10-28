// Command llmgate starts the LLM gateway HTTP server.
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

	"github.com/harshakumar25/llmgate/internal/cache"
	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/guardrails"
	"github.com/harshakumar25/llmgate/internal/metering"
	"github.com/harshakumar25/llmgate/internal/providers"
	"github.com/harshakumar25/llmgate/internal/router"
	"github.com/harshakumar25/llmgate/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(os.Getenv("LLMGATE_CONFIG"))
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	// Build the provider chain in the configured order. Providers are tried
	// in order by the router until one succeeds.
	chain, err := providers.Build(cfg.Providers, cfg.Timeout)
	if err != nil {
		logger.Error("build providers", "err", err)
		os.Exit(1)
	}

	meter, err := metering.NewLedger(cfg.UsageLog)
	if err != nil {
		logger.Error("open usage log", "err", err)
		os.Exit(1)
	}

	served := make([][]string, len(cfg.Providers))
	for i, p := range cfg.Providers {
		served[i] = p.Serves()
	}

	srv := server.New(server.Deps{
		Config:     cfg,
		Router:     router.NewWithModels(chain, served, cfg.Retry, cfg.ModelFallback),
		Guardrails: guardrails.New(cfg.Guardrails),
		Meter:      meter,
		Prices:     metering.NewPrices(cfg.Providers),
		Cache:      cache.NewRedis(cfg.Redis, cfg.Cache.MaxEntries), // in-memory when redis is unset
		Logger:     logger,
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("llmgate listening", "addr", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "err", err)
	}
}
