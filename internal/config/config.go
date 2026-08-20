// Package config загружает конфигурацию шлюза из переменных окружения.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config содержит все настройки opencode-backend.
type Config struct {
	Port             string
	OpenCodeBaseURL  string
	OpenCodeUsername string
	OpenCodePassword string
	AdminToken       string // если пусто — генерируется и логируется
	DBPath           string // пусто = in-memory хранилище
	WorkspaceDir     string // каталог, доступный opencode-серверу для чтения файлов
	RequestTimeout   time.Duration
	PermissionMode   string // ask / allow / deny (проксирование в opencode-сервер)
	DefaultAgent     string
	DefaultModel     string
	LogLevel         string
}

// Load читает конфигурацию из окружения.
func Load() (*Config, error) {
	timeout, err := time.ParseDuration(getEnv("OPENCODE_REQUEST_TIMEOUT", "30m"))
	if err != nil {
		return nil, fmt.Errorf("OPENCODE_REQUEST_TIMEOUT: %w", err)
	}

	return &Config{
		Port:             getEnv("PORT", "8080"),
		OpenCodeBaseURL:  getEnv("OPENCODE_BASE_URL", "http://localhost:4096"),
		OpenCodeUsername: getEnv("OPENCODE_USERNAME", "opencode"),
		OpenCodePassword: os.Getenv("OPENCODE_PASSWORD"),
		AdminToken:       os.Getenv("ADMIN_TOKEN"),
		DBPath:           os.Getenv("DB_PATH"),
		WorkspaceDir:     getEnv("WORKSPACE_DIR", "/workspace"),
		RequestTimeout:   timeout,
		PermissionMode:   getEnv("PERMISSION_MODE", "ask"),
		DefaultAgent:     getEnv("OPENCODE_AGENT", "build"),
		DefaultModel:     os.Getenv("OPENCODE_MODEL"),
		LogLevel:         getEnv("LOG_LEVEL", "info"),
	}, nil
}

func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
