// Copyright 2026 Andy Lo-A-Foe
// SPDX-License-Identifier: Apache-2.0

// Command mt-mcp-proxy is a generic multi-tenant auth proxy in front of any
// unmodified MCP server speaking the streamable-HTTP transport (e.g. mcp-grafana
// or `github-mcp-server http`). It is itself an MCP server: it verifies an
// incoming JWT, maps the caller's groups to authorized tenants, and routes each
// tool call to the backend that serves the selected tenant, injecting that
// tenant's downstream credential.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loafoe/mt-mcp-proxy/internal/auth"
	"github.com/loafoe/mt-mcp-proxy/internal/config"
	"github.com/loafoe/mt-mcp-proxy/internal/observability"
	"github.com/loafoe/mt-mcp-proxy/internal/proxy"
	"github.com/loafoe/mt-mcp-proxy/internal/registry"
	"github.com/loafoe/mt-mcp-proxy/internal/session"
)

// version is the reported service version; overridable at build time via
// -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*configPath, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	verifier, err := auth.New(ctx, cfg.Auth)
	if err != nil {
		return err
	}

	reg, err := registry.New(cfg.Backends)
	if err != nil {
		return err
	}

	obs, err := observability.Setup(ctx, observability.Config{
		MetricsListen:  cfg.Observability.MetricsListen,
		MetricsPath:    cfg.Observability.MetricsPath,
		ServiceName:    "mt-mcp-proxy",
		ServiceVersion: version,
	})
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Shutdown(shutdownCtx); err != nil {
			logger.Warn("observability shutdown", "err", err)
		}
	}()

	store := session.NewStore(30 * time.Minute)
	cat := proxy.NewCatalog(reg.ReferenceBackend(), reg.ReferenceCredential(), 5*time.Minute)
	h := proxy.NewHandler(verifier, reg, store, cat, logger, obs, cfg.Server, cfg.Auth)

	healthz := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}

	mux := http.NewServeMux()
	mux.Handle(cfg.Server.Path, h)
	mux.HandleFunc("/.well-known/oauth-protected-resource", h.ServeProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource"+cfg.Server.Path, h.ServeProtectedResourceMetadata)
	mux.HandleFunc("/healthz", healthz)

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Separate observability listener for Prometheus scrapes, isolated from the
	// data plane. Disabled when metrics_listen is empty.
	var obsSrv *http.Server
	if addr := obs.MetricsListen(); addr != "" {
		obsMux := http.NewServeMux()
		obsMux.Handle(obs.MetricsPath(), obs.MetricsHandler())
		obsMux.HandleFunc("/healthz", healthz)
		obsSrv = &http.Server{
			Addr:              addr,
			Handler:           obsMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			logger.Info("observability listening", "addr", addr, "metrics_path", obs.MetricsPath())
			if err := obsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("observability listener failed", "err", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if obsSrv != nil {
			_ = obsSrv.Shutdown(shutdownCtx)
		}
	}()

	logger.Info("listening", "addr", cfg.Server.Listen, "path", cfg.Server.Path, "auth_mode", cfg.Auth.Mode, "backends", len(cfg.Backends))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
