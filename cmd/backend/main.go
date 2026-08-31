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

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"opencode-backend/internal/api"
	"opencode-backend/internal/config"
	"opencode-backend/internal/engine"
	"opencode-backend/internal/logx"
	"opencode-backend/internal/opencode"
	"opencode-backend/internal/store"
	"opencode-backend/internal/telemetry"
	"opencode-backend/internal/token"
	"opencode-backend/internal/ws"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "error", err)
		os.Exit(1)
	}

	adminToken := cfg.AdminToken
	if adminToken == "" {
		adminToken = token.New()
		// Решение по bootstrap-токену: в лог печатаем только факт генерации,
		// сам токен записываем в файл .admin_token (права 0600) рядом с БД,
		// либо в текущей директории, если БД не используется.
		if err := saveAdminToken(adminToken, cfg.DBPath); err != nil {
			slog.Error("save generated ADMIN_TOKEN", "error", err)
			os.Exit(1)
		}
	}

	logger := logx.Setup("opencode-backend", adminToken, cfg.OpenCodePassword)
	shutdownTelemetry, err := telemetry.Init(context.Background())
	if err != nil {
		logger.Error("telemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			logger.Error("telemetry shutdown", "error", err)
		}
	}()

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

	// Сид администратора: токен из env или сгенерированный (см. выше).
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
		Handler:           otelhttp.NewHandler(srv.Handler(), "opencode-backend"),
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

// saveAdminToken записывает сгенерированный токен в файл .admin_token
// (0600) в каталоге БД или в текущей директории. Токен не логируется,
// чтобы не утекать в централизованные логи.
func saveAdminToken(tok, dbPath string) error {
	dir := "."
	if dbPath != "" {
		dir = filepath.Dir(dbPath)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, ".admin_token")
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	slog.Warn("generated new ADMIN_TOKEN", "path", path)
	return nil
}
