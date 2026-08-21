// opencode-backend — единая точка взаимодействия с opencode-сервером.
// См. планирование в opencode-bot/agents (backend-gateway).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"opencode-backend/internal/api"
	"opencode-backend/internal/config"
	"opencode-backend/internal/engine"
	"opencode-backend/internal/opencode"
	"opencode-backend/internal/store"
	"opencode-backend/internal/token"
	"opencode-backend/internal/ws"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	oc := opencode.New(cfg.OpenCodeBaseURL, cfg.OpenCodeUsername, cfg.OpenCodePassword)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := oc.Health(ctx); err != nil {
			logger.Warn("opencode server unreachable", "url", cfg.OpenCodeBaseURL, "error", err)
		}
		cancel()
	}

	var st store.Store
	var sqliteDB *store.SQLite
	if cfg.DBPath != "" {
		if dir := filepath.Dir(cfg.DBPath); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				logger.Error("mkdir db dir", "dir", dir, "error", err)
				os.Exit(1)
			}
		}
		sqliteDB, err = store.NewSQLite(cfg.DBPath)
		if err != nil {
			logger.Error("sqlite", "path", cfg.DBPath, "error", err)
			os.Exit(1)
		}
		st = sqliteDB
	} else {
		st = store.NewMemory()
	}
	hub := ws.NewHub()
	eng := engine.New(oc, st, hub, cfg, logger)

	// Сид администратора: токен из env (или сгенерированный, логируется один раз).
	adminToken := cfg.AdminToken
	if adminToken == "" {
		adminToken = token.New()
		logger.Warn("ADMIN_TOKEN не задан — сгенерирован. Сохраните его!",
			"token", adminToken)
	}
	eng.EnsureUser("admin", "admin")
	_ = st.EnsureAPIKey(&store.APIKey{
		TokenHash: token.Hash(adminToken),
		UserID:    "admin",
		Label:     "admin",
		CreatedAt: time.Now(),
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	eng.Start(ctx)

	srv := api.New(eng, hub, st, cfg, logger)
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("opencode-backend listening", "port", cfg.Port, "workspace", cfg.WorkspaceDir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if sqliteDB != nil {
		_ = sqliteDB.Close()
	}
	logger.Info("stopped")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
