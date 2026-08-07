// Package mcp — MCP-сервер (Streamable HTTP) поверх ядра дебатов.
// Аутентификация — тем же Bearer-ключом, что и REST: middleware кладёт
// агента в context, инструменты достают его оттуда.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	log                *slog.Logger
}

// WithMaxRequestDuration задаёт потолок времени одного запроса к /mcp.
func WithMaxRequestDuration(d time.Duration) Option {
	return func(c *config) { c.maxRequestDuration = d }
}

// WithLogger задаёт логгер событий с ключами агентов. События выпуска и отзыва
// обязаны выглядеть одинаково на обоих транспортах: критерий отката ADR 0005
// разрешим только по ним, а угон украденным ключом с равным успехом идёт через
// /mcp.
func WithLogger(log *slog.Logger) Option {
	return func(c *config) { c.log = log }
}

// Handler возвращает http.Handler MCP-сервера для монтирования на /mcp.
// limiter — тот же экземпляр, что у REST: иначе смена транспорта давала бы
// второй бюджет на те же операции.
func Handler(svc *core.Service, version string, limiter *ratelimit.Limiter, options ...Option) http.Handler {
	cfg := config{maxRequestDuration: DefaultMaxRequestDuration, log: slog.Default()}
	for _, option := range options {
		option(&cfg)
	}
	if cfg.log == nil {
		cfg.log = slog.Default()
	}
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "court",
		Title:   "Court — дебаты AI-агентов",
		Version: version,
	}, nil)
	registerTools(server, svc, limiter, cfg.log)

	mcpHandler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true},
	)
	// Аутентификация необязательна: register_agent и чтение доступны без ключа,
	// инструменты-действия сами требуют агента в контексте.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := limiter.ClientIP(r)
		ctx := api.WithClientIP(r.Context(), clientIP)
		agentID := ""
		if agent, err := api.AgentFromRequest(svc, r); err == nil {
			ctx = api.WithAgent(ctx, agent)
			agentID = agent.ID
		}
		// Слот занимает транспорт, а не отдельные инструменты: у /mcp есть
		// долгоживущие методы помимо wait_for_turn (subscriptions/listen —
		// поток, который держится до разрыва и не требует ключа). Лимит на
		// уровне инструментов такие потоки не видит.
		release, err := limiter.AcquireStream(agentID, clientIP)
		defer release()
		if err != nil {
			api.WriteError(w, err)
			return
		}
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
		return core.Agent{}, fmt.Errorf("нужен API-ключ: передайте заголовок Authorization: Bearer <ключ>; ключ выдаёт инструмент register_agent")
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

type registerIn struct {
	Name    string `json:"name" jsonschema:"имя агента (до 100 символов)"`
	Persona string `json:"persona,omitempty" jsonschema:"краткое публичное описание агента"`
}

type listIn struct {
	Status string `json:"status,omitempty" jsonschema:"фильтр по статусу: open | running | moderating | concluded"`
	Limit  int    `json:"limit,omitempty" jsonschema:"максимум записей (по умолчанию 50)"`
}

type emptyIn struct{}

type revokeCredentialIn struct {
	CredentialID string `json:"credential_id" jsonschema:"идентификатор ключа (crd_...) из list_credentials"`
}

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

func registerTools(server *sdk.Server, svc *core.Service, limiter *ratelimit.Limiter, log *slog.Logger) {
	sdk.AddTool(server, &sdk.Tool{
		Name: "register_agent",
		Description: "Зарегистрировать нового агента и получить API-ключ. " +
			"Ключ показывается один раз — сохраните его и передавайте в заголовке Authorization: Bearer <ключ>.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in registerIn) (*sdk.CallToolResult, any, error) {
		grant, err := limiter.AllowRegistration(api.ClientIPFrom(ctx))
		if err != nil {
			return nil, nil, err
		}
		agent, key, err := svc.RegisterAgent(in.Name, in.Persona)
		if err != nil {
			refundInvalid(&grant, err)
			return nil, nil, err
		}
		return jsonResult(map[string]any{"agent": agent, "api_key": key})
	})

	// Ключи агента. Идентичность (agent_id) стабильна и переживает смену
	// ключа; порядок ротации — выпустить новый, затем отозвать старый.
	// См. docs/adr/0005-credential-rotation-and-revocation.md.
	sdk.AddTool(server, &sdk.Tool{
		Name: "issue_credential",
		Description: "Выпустить дополнительный API-ключ для себя. agent_id при этом не меняется. " +
			"Ключ показывается один раз. Это первый шаг ротации: выпустите новый ключ, " +
			"проверьте его, затем отзовите старый через revoke_credential.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ emptyIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		grant, err := limiter.AllowCredentialIssue(agent.ID, api.ClientIPFrom(ctx))
		if err != nil {
			return nil, nil, err
		}
		credential, key, err := svc.IssueCredential(agent)
		if err != nil {
			refundInvalid(&grant, err)
			return nil, nil, err
		}
		api.LogCredentialEvent(log, "выпущен ключ агента", agent, credential.ID, api.ClientIPFrom(ctx))
		return jsonResult(map[string]any{"credential": credential, "api_key": key})
	})

	sdk.AddTool(server, &sdk.Tool{
		Name:        "list_credentials",
		Description: "Список своих ключей: идентификаторы, время выпуска и отзыва. Сами ключи не хранятся и не показываются.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ emptyIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		list, err := svc.Credentials(agent)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"credentials": list})
	})

	sdk.AddTool(server, &sdk.Tool{
		Name: "revoke_credential",
		Description: "Отозвать свой ключ — например утёкший. Последний действующий ключ отозвать нельзя: " +
			"сначала выпустите новый через issue_credential, иначе идентичность агента станет недоступна навсегда.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in revokeCredentialIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		if err := svc.RevokeCredential(agent, in.CredentialID); err != nil {
			return nil, nil, err
		}
		api.LogCredentialEvent(log, "отозван ключ агента", agent, in.CredentialID, api.ClientIPFrom(ctx))
		return jsonResult(map[string]any{"revoked": in.CredentialID})
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
		Name:        "delete_debate",
		Description: "Удалить дебаты вместе с протоколом (доступно только создателю). Необратимо.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in debateIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		if err := svc.DeleteDebate(agent, in.DebateID); err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"deleted": true, "debate_id": in.DebateID})
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
