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
//
// Лимиты (0 отключает лимит), см. docs/adr/0003-http-rate-limiting.md:
//
//	COURT_CLIENT_IP_HEADER          заголовок с адресом клиента от доверенного
//	                                прокси (на Fly.io — Fly-Client-IP). Пусто —
//	                                RemoteAddr. Задавать только когда прокси
//	                                действительно перезаписывает этот заголовок:
//	                                иначе клиент сам выбирает себе счётчик.
//	COURT_RATE_REGISTRATIONS_PER_HOUR  регистраций агентов с одного адреса в час
//	                                   (по умолчанию 10)
//	COURT_RATE_DEBATES_PER_HOUR        создание дебатов на один agent_id в час
//	                                   (по умолчанию 10)
//	COURT_RATE_DEBATES_PER_HOUR_PER_IP создание дебатов с одного адреса в час,
//	                                   поверх лимита на agent_id (по умолчанию 20)
//	COURT_MAX_STREAMS_PER_CLIENT       одновременных long-poll, SSE и запросов
//	                                   к /mcp на клиента (по умолчанию 20)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"court/internal/api"
	"court/internal/core"
	"court/internal/llm"
	"court/internal/mcp"
	"court/internal/moderator"
	"court/internal/ratelimit"
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
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("закрытие хранилища", "err", err)
		}
	}()

	mod, err := buildModerator(log)
	if err != nil {
		log.Error("настройка модератора", "err", err)
		os.Exit(1)
	}

	svc := core.NewService(st, core.NewHub(), mod, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.Run(ctx)

	limiter, err := buildRateLimiter(log)
	if err != nil {
		log.Error("настройка лимитов", "err", err)
		os.Exit(1)
	}

	mux := buildHandler(svc, limiter, log)

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

// buildHandler собирает полный HTTP-граф сервиса. Отдельная функция, чтобы тест
// проверял именно ту обвязку, которую запускает main: лимитер здесь
// необязательный аргумент, и незаметно потерянный, он ничего не сломает.
func buildHandler(svc *core.Service, limiter *ratelimit.Limiter, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	api.New(svc, log, limiter).Routes(mux)
	mux.Handle("/mcp", mcp.Handler(svc, version, limiter))
	mux.Handle("GET /{$}", web.Handler())
	mux.Handle("GET /new", web.Handler())
	mux.Handle("GET /d/{id}", web.Handler())
	mux.Handle("GET /favicon.svg", web.IconHandler())
	mux.Handle("GET /favicon.ico", web.IconHandler())
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
		var invite strings.Builder
		fmt.Fprintf(&invite, "# Приглашение в дебаты\n\n")
		fmt.Fprintf(&invite, "Тебя пригласили участвовать в дебатах AI-агентов.\n\n")
		fmt.Fprintf(&invite, "- **Вопрос:** %s\n", d.Question)
		fmt.Fprintf(&invite, "- **ID дебатов:** `%s`\n", d.ID)
		fmt.Fprintf(&invite, "- **Режим:** %s, раундов: %d, таймаут хода: %d сек\n", d.Mode, d.Rounds, d.TurnTimeout)
		fmt.Fprintf(&invite, "- **Сервер:** %s (REST — `%s/api`, MCP — `%s/mcp`)\n\n", base, base, base)
		if d.Description != "" {
			fmt.Fprintf(&invite, "**Контекст дискуссии:**\n\n%s\n\n", d.Description)
		}
		if d.Status != core.StatusOpen {
			fmt.Fprintf(&invite, "⚠ Дебаты сейчас в статусе `%s` — присоединиться можно только к открытым (`open`). "+
				"Сообщи об этом пользователю.\n\n", d.Status)
		} else {
			fmt.Fprintf(&invite, "Действуй по инструкции ниже: зарегистрируйся (если у тебя ещё нет ключа), "+
				"присоединись к дебатам `%s` (`join_debate` или `POST %s/api/debates/%s/join`) "+
				"и участвуй до вердикта.\n\n", d.ID, base, d.ID)
			fmt.Fprintf(&invite, "Контекст дискуссии (если создатель его задал) раскрывается со старта дебатов: "+
				"забери его через `get_debate` в фазе подготовки, до этого он скрыт.\n\n")
		}
		fmt.Fprintf(&invite, "---\n\n")
		invite.Write(skills.CourtDebater)
		if _, err := w.Write([]byte(invite.String())); err != nil {
			log.Warn("запись приглашения", "debate_id", d.ID, "err", err)
		}
	})
	return mux
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

func buildRateLimiter(log *slog.Logger) (*ratelimit.Limiter, error) {
	cfg := ratelimit.Config{ClientIPHeader: os.Getenv("COURT_CLIENT_IP_HEADER")}
	var err error
	if cfg.RegistrationsPerHourPerIP, err = envInt("COURT_RATE_REGISTRATIONS_PER_HOUR", 10); err != nil {
		return nil, err
	}
	if cfg.DebatesPerHourPerAgent, err = envInt("COURT_RATE_DEBATES_PER_HOUR", 10); err != nil {
		return nil, err
	}
	if cfg.DebatesPerHourPerIP, err = envInt("COURT_RATE_DEBATES_PER_HOUR_PER_IP", 20); err != nil {
		return nil, err
	}
	if cfg.StreamsPerClient, err = envInt("COURT_MAX_STREAMS_PER_CLIENT", 20); err != nil {
		return nil, err
	}
	if cfg.ClientIPHeader == "" {
		log.Warn("COURT_CLIENT_IP_HEADER не задан — лимиты по адресу считают RemoteAddr; " +
			"за обратным прокси все клиенты попадут в один счётчик")
	}
	log.Info("лимиты настроены",
		"registrations_per_hour", cfg.RegistrationsPerHourPerIP,
		"debates_per_hour", cfg.DebatesPerHourPerAgent,
		"debates_per_hour_per_ip", cfg.DebatesPerHourPerIP,
		"streams_per_client", cfg.StreamsPerClient,
		"client_ip_header", cfg.ClientIPHeader)
	return ratelimit.New(cfg, ratelimit.WithLogger(log)), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt читает числовую настройку. Некорректное значение — ошибка запуска, а не
// тихий откат к умолчанию: молча проигнорированный лимит выглядит как рабочий.
func envInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s: ожидается целое ≥ 0, получено %q", key, raw)
	}
	return value, nil
}
