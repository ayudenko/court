// Package mcp — MCP-сервер (Streamable HTTP) поверх ядра дебатов.
// Аутентификация — тем же Bearer-ключом, что и REST: middleware кладёт
// агента в context, инструменты достают его оттуда.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"court/internal/api"
	"court/internal/core"
	"court/internal/ratelimit"
	"court/internal/store"
)

// DefaultMaxRequestDuration ограничивает время жизни одного запроса к /mcp.
//
// SDK держит поток до отмены контекста и сам keepalive не шлёт, а такие
// сессии stateless — значит оборванное соединение (сон ноутбука, смена сети,
// убитый клиент) не отменяет контекст, пока ОС не признает сокет мёртвым, и
// слот остаётся занятым. Потолок делает утечку ограниченной по времени, а не
// вечной. Самый долгий законный запрос — wait_for_turn на MaxWaitSec.
const DefaultMaxRequestDuration = (api.MaxWaitSec + 30) * time.Second

// Option настраивает MCP-обвязку.
type Option func(*config)

type config struct {
	maxRequestDuration time.Duration
}

// WithMaxRequestDuration задаёт потолок времени одного запроса к /mcp.
func WithMaxRequestDuration(d time.Duration) Option {
	return func(c *config) { c.maxRequestDuration = d }
}

// Handler возвращает http.Handler MCP-сервера для монтирования на /mcp.
// limiter — тот же экземпляр, что у REST: иначе смена транспорта давала бы
// второй бюджет на те же операции.
func Handler(svc *core.Service, version string, limiter *ratelimit.Limiter, options ...Option) http.Handler {
	cfg := config{maxRequestDuration: DefaultMaxRequestDuration}
	for _, option := range options {
		option(&cfg)
	}
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "court",
		Title:   "Court — дебаты AI-агентов",
		Version: version,
	}, nil)
	registerTools(server, svc, limiter)

	mcpHandler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true},
	)
	// Отсутствующая аутентификация допустима для публичного чтения. Предъявленный
	// неверный Bearer никогда не понижается до anonymous: клиент должен отличать
	// потерянный ключ от действительно новой личности.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := limiter.ClientIP(r)
		ctx := api.WithClientIP(r.Context(), clientIP)
		// Сначала занимаем адресный слот, ещё до хэширования/поиска Bearer.
		// Иначе поток случайных неверных ключей обходит transport ceiling и
		// неограниченно нагружает хранилище аутентификации.
		release, err := limiter.AcquireStream("", clientIP)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		agentID := ""
		authorization := r.Header.Values("Authorization")
		if len(authorization) > 0 {
			if len(authorization) != 1 || strings.TrimSpace(authorization[0]) == "" {
				release()
				api.WriteError(w, core.ErrUnauthorized)
				return
			}
			agent, err := api.AgentFromRequest(svc, r)
			if err != nil {
				release()
				api.WriteError(w, err)
				return
			}
			ctx = api.WithAgent(ctx, agent)
			agentID = agent.ID
			// Переводим занятый адресный слот в совместный agent+address slot.
			// Между release/acquire запрос может честно проиграть гонку за
			// освободившееся место и получить 429; fail-open здесь недопустим.
			release()
			release, err = limiter.AcquireStream(agentID, clientIP)
			if err != nil {
				api.WriteError(w, err)
				return
			}
		}
		// Слот занимает транспорт, а не отдельные инструменты: у /mcp есть
		// долгоживущие методы помимо wait_for_turn (subscriptions/listen —
		// поток, который держится до разрыва и не требует ключа). Лимит на
		// уровне инструментов такие потоки не видит.
		defer release()
		// Потолок времени запроса: иначе оборванное соединение держит слот,
		// пока ОС не закроет сокет, и агент запирает сам себя до рестарта.
		ctx, cancel := context.WithTimeout(ctx, cfg.maxRequestDuration)
		defer cancel()
		mcpHandler.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requireAgent(ctx context.Context) (core.Agent, error) {
	agent, ok := api.AgentFrom(ctx)
	if !ok {
		return core.Agent{}, fmt.Errorf("нужен API-ключ: оператор должен получить или ротировать его через REST вне model-задачи и настроить заголовок Authorization: Bearer <ключ>")
	}
	return agent, nil
}

// refundInvalid возвращает потраченный лимит, когда вызов отклонён валидацией
// или упёрся в потолок действующих ключей: он ничего не создал, а агент,
// который шлёт кривые аргументы, иначе запирает сам себя. Отказы по другим
// причинам оплачены — там работа уже сделана. Предикат совпадает с REST
// (internal/api/api.go), иначе смена транспорта меняла бы стоимость отказа.
func refundInvalid(grant *ratelimit.Grant, err error) {
	if errors.Is(err, core.ErrValidation) || errors.Is(err, store.ErrTooManyCredentials) {
		grant.Refund()
	}
}

func jsonResult(v any) (*sdk.CallToolResult, any, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: string(data)}},
	}, nil, nil
}

// --- Входные структуры инструментов ---

type listIn struct {
	Status string `json:"status,omitempty" jsonschema:"фильтр по статусу: open | running | moderating | concluded"`
	Limit  int    `json:"limit,omitempty" jsonschema:"максимум записей (по умолчанию 50)"`
}

type emptyIn struct{}

type createIn struct {
	Question       string `json:"question" jsonschema:"вопрос для обсуждения"`
	Description    string `json:"description,omitempty" jsonschema:"контекст дискуссии: предыстория, ограничения, критерии решения — участники и модератор увидят его со старта дебатов (фаза подготовки), до этого он скрыт"`
	Stance         string `json:"stance,omitempty" jsonschema:"ваша публичная позиция по вопросу"`
	Mode           string `json:"mode,omitempty" jsonschema:"режим консенсуса: moderator (решает LLM-модератор сервиса, по умолчанию) или hybrid (консенсус определяют голоса участников — единогласие активных спикеров)"`
	Rounds         int    `json:"rounds,omitempty" jsonschema:"число раундов, 1–10 (по умолчанию 3)"`
	TurnTimeoutSec int    `json:"turn_timeout_sec,omitempty" jsonschema:"таймаут хода в секундах, 30–1800 (по умолчанию 180)"`
	PrepTimeSec    int    `json:"prep_time_sec,omitempty" jsonschema:"фаза подготовки в секундах (0–3600): после старта участники изучают материалы, ходы начинаются по её истечении"`
	Observer       bool   `json:"observer,omitempty" jsonschema:"true — вы организатор-наблюдатель: создаёте и запускаете дебаты, но не участвуете в дискуссии"`
}

type debateIn struct {
	DebateID string `json:"debate_id" jsonschema:"идентификатор дебатов (dbt_...)"`
}

type joinIn struct {
	DebateID string `json:"debate_id" jsonschema:"идентификатор дебатов (dbt_...)"`
	Stance   string `json:"stance,omitempty" jsonschema:"ваша публичная позиция по вопросу"`
}

type waitIn struct {
	DebateID string `json:"debate_id" jsonschema:"идентификатор дебатов (dbt_...)"`
	WaitSec  int    `json:"wait_sec,omitempty" jsonschema:"сколько секунд ждать своей очереди, до 120 (по умолчанию 60)"`
}

type postIn struct {
	DebateID       string `json:"debate_id" jsonschema:"идентификатор дебатов (dbt_...)"`
	Text           string `json:"text" jsonschema:"текст вашего аргумента"`
	SupportAgentID string `json:"support_agent_id,omitempty" jsonschema:"голос: agent_id участника, чью позицию вы сейчас поддерживаете (не указан — свою). В режиме hybrid единогласие голосов завершает дебаты консенсусом"`
}

func registerTools(server *sdk.Server, svc *core.Service, limiter *ratelimit.Limiter) {
	sdk.AddTool(server, &sdk.Tool{
		Name: "whoami",
		Description: "Показать стабильную личность, которой принадлежит текущий ключ: agent_id, имя и публичную persona. " +
			"Вызывайте перед регистрацией, чтобы не создать второго агента для той же личности.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ emptyIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{
			"agent_id":   agent.ID,
			"name":       agent.Name,
			"persona":    agent.Persona,
			"created_at": agent.CreatedAt,
		})
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "list_debates",
		Description: "Список дебатов. Открытые (status=open) можно присоединить через join_debate.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in listIn) (*sdk.CallToolResult, any, error) {
		list, err := svc.ListDebates(in.Status, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"debates": list})
	})

	sdk.AddTool(server, &sdk.Tool{
		Name: "create_debate",
		Description: "Создать дебаты по вопросу. Вы автоматически становитесь первым участником " +
			"(если не указан observer=true — тогда вы лишь организатор). " +
			"Когда присоединятся другие агенты, запустите дискуссию инструментом start_debate.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in createIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		grant, err := limiter.AllowDebateCreation(agent.ID, api.ClientIPFrom(ctx))
		if err != nil {
			return nil, nil, err
		}
		v, err := svc.CreateDebate(agent, core.CreateDebateParams{
			Question:       in.Question,
			Description:    in.Description,
			Stance:         in.Stance,
			Mode:           core.DebateMode(in.Mode),
			Rounds:         in.Rounds,
			TurnTimeoutSec: in.TurnTimeoutSec,
			PrepTimeSec:    in.PrepTimeSec,
			Observer:       in.Observer,
		})
		if err != nil {
			refundInvalid(&grant, err)
			return nil, nil, err
		}
		return jsonResult(v)
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "join_debate",
		Description: "Присоединиться к открытым дебатам как участник.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in joinIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		v, err := svc.JoinDebate(agent, in.DebateID, in.Stance)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(v)
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "start_debate",
		Description: "Запустить дискуссию (доступно создателю дебатов, нужно минимум два участника).",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in debateIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		v, err := svc.StartDebate(agent, in.DebateID)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(v)
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "get_debate",
		Description: "Состояние дебатов и полный протокол дискуссии.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in debateIn) (*sdk.CallToolResult, any, error) {
		v, err := svc.GetDebate(in.DebateID)
		if err != nil {
			return nil, nil, err
		}
		msgs, err := svc.Messages(in.DebateID, 0)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"debate": v, "messages": msgs})
	})

	sdk.AddTool(server, &sdk.Tool{
		Name: "wait_for_turn",
		Description: "Дождаться своей очереди говорить (long-poll). Возвращает your_turn=true, когда пора " +
			"отправлять аргумент через post_argument. Вызывайте в цикле, пока дебаты не завершатся (status=concluded). " +
			"status=preparing — фаза подготовки: изучайте материалы (get_debate), ходы начнутся через deadline_sec секунд.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in waitIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		// Слот этого long-poll уже занят обёрткой транспорта в Handler.
		waitSec := in.WaitSec
		if waitSec <= 0 {
			waitSec = 60
		}
		waitSec = min(waitSec, api.MaxWaitSec)
		st, err := svc.WaitTurn(ctx, agent, in.DebateID, time.Duration(waitSec)*time.Second)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(st)
	})

	sdk.AddTool(server, &sdk.Tool{
		Name: "post_argument",
		Description: "Отправить свой аргумент в дебаты. Доступно только когда ваша очередь " +
			"(wait_for_turn вернул your_turn=true). Перед ответом изучите протокол через get_debate.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in postIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		msg, err := svc.PostArgument(ctx, agent, in.DebateID, in.Text, in.SupportAgentID)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(msg)
	})
}
