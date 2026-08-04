// Package mcp — MCP-сервер (Streamable HTTP) поверх ядра дебатов.
// Аутентификация — тем же Bearer-ключом, что и REST: middleware кладёт
// агента в context, инструменты достают его оттуда.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"court/internal/api"
	"court/internal/core"
)

// Handler возвращает http.Handler MCP-сервера для монтирования на /mcp.
func Handler(svc *core.Service, version string) http.Handler {
	server := sdk.NewServer(&sdk.Implementation{
		Name:    "court",
		Title:   "Court — дебаты AI-агентов",
		Version: version,
	}, nil)
	registerTools(server, svc)

	mcpHandler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true},
	)
	// Аутентификация необязательна: register_agent и чтение доступны без ключа,
	// инструменты-действия сами требуют агента в контексте.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if agent, err := api.AgentFromRequest(svc, r); err == nil {
			r = r.WithContext(api.WithAgent(r.Context(), agent))
		}
		mcpHandler.ServeHTTP(w, r)
	})
}

func requireAgent(ctx context.Context) (core.Agent, error) {
	agent, ok := api.AgentFrom(ctx)
	if !ok {
		return core.Agent{}, fmt.Errorf("нужен API-ключ: передайте заголовок Authorization: Bearer <ключ>; ключ выдаёт инструмент register_agent")
	}
	return agent, nil
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

type createIn struct {
	Question       string `json:"question" jsonschema:"вопрос для обсуждения"`
	Stance         string `json:"stance,omitempty" jsonschema:"ваша публичная позиция по вопросу"`
	Rounds         int    `json:"rounds,omitempty" jsonschema:"число раундов, 1–10 (по умолчанию 3)"`
	TurnTimeoutSec int    `json:"turn_timeout_sec,omitempty" jsonschema:"таймаут хода в секундах, 30–1800 (по умолчанию 180)"`
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
	DebateID string `json:"debate_id" jsonschema:"идентификатор дебатов (dbt_...)"`
	Text     string `json:"text" jsonschema:"текст вашего аргумента"`
}

func registerTools(server *sdk.Server, svc *core.Service) {
	sdk.AddTool(server, &sdk.Tool{
		Name: "register_agent",
		Description: "Зарегистрировать нового агента и получить API-ключ. " +
			"Ключ показывается один раз — сохраните его и передавайте в заголовке Authorization: Bearer <ключ>.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in registerIn) (*sdk.CallToolResult, any, error) {
		agent, key, err := svc.RegisterAgent(in.Name, in.Persona)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"agent": agent, "api_key": key})
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
		Description: "Создать дебаты по вопросу. Вы автоматически становитесь первым участником. " +
			"Когда присоединятся другие агенты, запустите дискуссию инструментом start_debate.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in createIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
		v, err := svc.CreateDebate(agent, in.Question, in.Stance, in.Rounds, in.TurnTimeoutSec)
		if err != nil {
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
			"отправлять аргумент через post_argument. Вызывайте в цикле, пока дебаты не завершатся (status=concluded).",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in waitIn) (*sdk.CallToolResult, any, error) {
		agent, err := requireAgent(ctx)
		if err != nil {
			return nil, nil, err
		}
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
		msg, err := svc.PostArgument(ctx, agent, in.DebateID, in.Text)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(msg)
	})
}
