// courtd — сервер дебатов AI-агентов: REST API, SSE и MCP.
//
// Конфигурация через переменные окружения:
//
//	COURT_ADDR                адрес прослушивания (по умолчанию :8080)
//	COURT_DB                  путь к файлу SQLite (по умолчанию court.db)
//	COURT_MODERATOR_PROVIDER  anthropic | openai (по умолчанию anthropic)
//	COURT_MODERATOR_MODEL     модель модератора (по умолчанию claude-opus-5)
//	COURT_MODERATOR_BASE_URL  base URL для openai-совместимых API
//	COURT_MODERATOR_NAME      отображаемое имя (по умолчанию «Модератор»)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY — ключ провайдера модератора
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

	"court/internal/api"
	"court/internal/core"
	"court/internal/llm"
	"court/internal/mcp"
	"court/internal/moderator"
	"court/internal/store"
)

const version = "0.2.0"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	addr := envOr("COURT_ADDR", ":8080")
	dbPath := envOr("COURT_DB", "court.db")

	st, err := store.Open(dbPath)
	if err != nil {
		log.Error("открытие хранилища", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	mod, err := buildModerator(log)
	if err != nil {
		log.Error("настройка модератора", "err", err)
		os.Exit(1)
	}

	svc := core.NewService(st, core.NewHub(), mod, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.Run(ctx)

	mux := http.NewServeMux()
	apiServer := api.New(svc, log)
	apiServer.Routes(mux)
	mux.Handle("/mcp", mcp.Handler(svc, version))

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Info("courtd запущен", "addr", addr, "db", dbPath, "version", version)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("HTTP-сервер", "err", err)
		os.Exit(1)
	}
	log.Info("courtd остановлен")
}

func buildModerator(log *slog.Logger) (core.Moderator, error) {
	providerName := envOr("COURT_MODERATOR_PROVIDER", "anthropic")
	model := envOr("COURT_MODERATOR_MODEL", "claude-opus-5")
	name := envOr("COURT_MODERATOR_NAME", "Модератор")

	var provider llm.Provider
	switch providerName {
	case "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			log.Warn("ANTHROPIC_API_KEY не задан — модерация будет недоступна, дебаты завершатся без итогов")
		}
		provider = llm.NewAnthropicProvider("", model, 4096)
	case "openai":
		if os.Getenv("OPENAI_API_KEY") == "" {
			log.Warn("OPENAI_API_KEY не задан — модерация будет недоступна, дебаты завершатся без итогов")
		}
		provider = llm.NewOpenAICompatProvider("", os.Getenv("COURT_MODERATOR_BASE_URL"), model, 4096)
	default:
		return nil, errors.New("COURT_MODERATOR_PROVIDER: ожидается anthropic или openai")
	}
	return moderator.New(name, provider), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
