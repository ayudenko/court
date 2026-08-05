// courtd — сервер дебатов AI-агентов: REST API, SSE и MCP.
//
// Конфигурация через переменные окружения:
//
//	COURT_ADDR                адрес прослушивания (по умолчанию :8080)
//	COURT_DB                  путь к файлу SQLite (по умолчанию court.db)
//	COURT_MODERATOR_PROVIDER  anthropic | openai (по умолчанию anthropic)
//	COURT_MODERATOR_MODEL     модель модератора (по умолчанию claude-opus-5)
//	COURT_MODERATOR_BASE_URL  base URL для openai-совместимых API (OpenRouter и т.п.)
//	COURT_MODERATOR_API_KEY   ключ провайдера модератора; если пуст —
//	                          ANTHROPIC_API_KEY / OPENAI_API_KEY
//	COURT_MODERATOR_NAME      отображаемое имя (по умолчанию «Модератор»)
package main

import (
	"context"
	"errors"
	"fmt"
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
	"court/internal/web"
	"court/skills"
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
	mux.Handle("GET /{$}", web.Handler())
	mux.Handle("GET /new", web.Handler())
	mux.Handle("GET /d/{id}", web.Handler())
	mux.HandleFunc("GET /skill.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write(skills.CourtDebater)
	})
	// Приглашение для агента: инструкция участника + данные конкретных дебатов.
	// Человеку достаточно дать агенту эту ссылку — дальше тот справится сам.
	mux.HandleFunc("GET /d/{id}/invite.md", func(w http.ResponseWriter, r *http.Request) {
		d, err := svc.GetDebate(r.PathValue("id"))
		if err != nil {
			http.Error(w, "дебаты не найдены", http.StatusNotFound)
			return
		}
		scheme := r.Header.Get("X-Forwarded-Proto")
		if scheme == "" {
			scheme = "http"
		}
		base := scheme + "://" + r.Host
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		fmt.Fprintf(w, "# Приглашение в дебаты\n\n")
		fmt.Fprintf(w, "Тебя пригласили участвовать в дебатах AI-агентов.\n\n")
		fmt.Fprintf(w, "- **Вопрос:** %s\n", d.Question)
		fmt.Fprintf(w, "- **ID дебатов:** `%s`\n", d.ID)
		fmt.Fprintf(w, "- **Режим:** %s, раундов: %d, таймаут хода: %d сек\n", d.Mode, d.Rounds, d.TurnTimeout)
		fmt.Fprintf(w, "- **Сервер:** %s (REST — `%s/api`, MCP — `%s/mcp`)\n\n", base, base, base)
		if d.Description != "" {
			fmt.Fprintf(w, "**Контекст дискуссии:**\n\n%s\n\n", d.Description)
		}
		if d.Status != core.StatusOpen {
			fmt.Fprintf(w, "⚠ Дебаты сейчас в статусе `%s` — присоединиться можно только к открытым (`open`). "+
				"Сообщи об этом пользователю.\n\n", d.Status)
		} else {
			fmt.Fprintf(w, "Действуй по инструкции ниже: зарегистрируйся (если у тебя ещё нет ключа), "+
				"присоединись к дебатам `%s` (`join_debate` или `POST %s/api/debates/%s/join`) "+
				"и участвуй до вердикта.\n\n", d.ID, base, d.ID)
		}
		fmt.Fprintf(w, "---\n\n")
		_, _ = w.Write(skills.CourtDebater)
	})

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
	// Явный ключ модератора; если пуст, SDK возьмёт стандартную переменную
	// провайдера (ANTHROPIC_API_KEY / OPENAI_API_KEY).
	apiKey := os.Getenv("COURT_MODERATOR_API_KEY")

	var provider llm.Provider
	switch providerName {
	case "anthropic":
		if apiKey == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
			log.Warn("ключ модератора не задан (COURT_MODERATOR_API_KEY / ANTHROPIC_API_KEY) — модерация будет недоступна")
		}
		provider = llm.NewAnthropicProvider(apiKey, model, 4096)
	case "openai":
		if apiKey == "" && os.Getenv("OPENAI_API_KEY") == "" {
			log.Warn("ключ модератора не задан (COURT_MODERATOR_API_KEY / OPENAI_API_KEY) — модерация будет недоступна")
		}
		provider = llm.NewOpenAICompatProvider(apiKey, os.Getenv("COURT_MODERATOR_BASE_URL"), model, 4096)
	default:
		return nil, errors.New("COURT_MODERATOR_PROVIDER: ожидается anthropic или openai")
	}
	log.Info("модератор настроен", "provider", providerName, "model", model,
		"base_url", os.Getenv("COURT_MODERATOR_BASE_URL"))
	return moderator.New(name, provider), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
