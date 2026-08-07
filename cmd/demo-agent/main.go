// demo-agent — тестовый агент-участник дебатов для локального окружения.
// Регистрируется через REST, создаёт дебаты (если задан DEBATE_QUESTION)
// или присоединяется к первым открытым, и участвует, генерируя аргументы
// своей LLM.
//
// Конфигурация через переменные окружения:
//
//	COURT_URL           адрес сервиса (по умолчанию http://localhost:8080)
//	AGENT_NAME          имя агента (обязательно)
//	AGENT_PERSONA       позиция/характер для системного промпта
//	AGENT_STANCE        публичная позиция в дебатах
//	AGENT_PROVIDER      anthropic | openai (по умолчанию anthropic)
//	AGENT_MODEL         модель (по умолчанию claude-opus-5)
//	AGENT_BASE_URL      base URL для openai-совместимых API
//	DEBATE_QUESTION     если задан — агент создаёт и запускает дебаты
//	DEBATE_DESCRIPTION  контекст дискуссии (для создателя)
//	DEBATE_MODE         moderator | hybrid (по умолчанию moderator)
//	DEBATE_PREP_SEC     фаза подготовки перед раундом 1 (по умолчанию 0)
//	DEBATE_ROUNDS       число раундов (по умолчанию 2)
//	DEBATE_PARTICIPANTS сколько участников ждать перед стартом (по умолчанию 3)
//	TURN_TIMEOUT_SEC    таймаут хода (по умолчанию 120)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"court/internal/core"
	"court/internal/llm"
)

type client struct {
	base string
	key  string
	http *http.Client
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name := os.Getenv("AGENT_NAME")
	if name == "" {
		log.Error("AGENT_NAME обязателен")
		os.Exit(1)
	}
	persona := os.Getenv("AGENT_PERSONA")
	stance := os.Getenv("AGENT_STANCE")

	provider, err := buildProvider()
	if err != nil {
		log.Error("настройка LLM", "err", err)
		os.Exit(1)
	}

	c := &client{
		base: envOr("COURT_URL", "http://localhost:8080"),
		http: &http.Client{Timeout: 150 * time.Second},
	}

	// Регистрация с ретраями — сервер может ещё подниматься.
	var reg struct {
		Agent  core.Agent `json:"agent"`
		APIKey string     `json:"api_key"`
	}
	err = retry(ctx, 30, 2*time.Second, func() error {
		return c.do(ctx, "POST", "/api/agents",
			map[string]string{"name": name, "persona": persona}, &reg)
	})
	if err != nil {
		log.Error("регистрация", "err", err)
		os.Exit(1)
	}
	c.key = reg.APIKey
	log = log.With("agent", name)
	log.Info("зарегистрирован", "id", reg.Agent.ID)

	debateID, err := findOrCreateDebate(ctx, c, stance, log)
	if err != nil {
		log.Error("подключение к дебатам", "err", err)
		os.Exit(1)
	}
	log.Info("участвую в дебатах", "debate", debateID)

	if err := debateLoop(ctx, c, provider, debateID, name, persona, stance, log); err != nil {
		log.Error("участие в дебатах", "err", err)
		os.Exit(1)
	}
}

// findOrCreateDebate: создатель создаёт дебаты, ждёт кворум и стартует;
// остальные ищут первые открытые дебаты и присоединяются.
func findOrCreateDebate(ctx context.Context, c *client, stance string, log *slog.Logger) (string, error) {
	question := os.Getenv("DEBATE_QUESTION")
	if question != "" {
		rounds := envInt("DEBATE_ROUNDS", 2)
		timeout := envInt("TURN_TIMEOUT_SEC", 120)
		want := envInt("DEBATE_PARTICIPANTS", 3)

		var d core.DebateView
		if err := c.do(ctx, "POST", "/api/debates", map[string]any{
			"question": question, "stance": stance,
			"description":   os.Getenv("DEBATE_DESCRIPTION"),
			"mode":          envOr("DEBATE_MODE", "moderator"),
			"prep_time_sec": envInt("DEBATE_PREP_SEC", 0),
			"rounds":        rounds, "turn_timeout_sec": timeout,
		}, &d); err != nil {
			return "", err
		}
		log.Info("дебаты созданы, жду участников", "debate", d.ID, "нужно", want)
		err := retry(ctx, 60, 2*time.Second, func() error {
			var cur core.DebateView
			if err := c.do(ctx, "GET", "/api/debates/"+d.ID, nil, &cur); err != nil {
				return err
			}
			if len(cur.Participants) < want {
				return fmt.Errorf("участников %d из %d", len(cur.Participants), want)
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("не дождался участников: %w", err)
		}
		if err := c.do(ctx, "POST", "/api/debates/"+d.ID+"/start", nil, nil); err != nil {
			return "", fmt.Errorf("старт дебатов: %w", err)
		}
		log.Info("дебаты запущены")
		return d.ID, nil
	}

	// Присоединение к первым открытым дебатам.
	var debateID string
	err := retry(ctx, 60, 2*time.Second, func() error {
		var list struct {
			Debates []core.DebateView `json:"debates"`
		}
		if err := c.do(ctx, "GET", "/api/debates?status=open", nil, &list); err != nil {
			return err
		}
		if len(list.Debates) == 0 {
			return fmt.Errorf("открытых дебатов пока нет")
		}
		id := list.Debates[len(list.Debates)-1].ID // самые старые — в конце списка
		if err := c.do(ctx, "POST", "/api/debates/"+id+"/join",
			map[string]string{"stance": stance}, nil); err != nil {
			return err
		}
		debateID = id
		return nil
	})
	return debateID, err
}

// debateLoop — основной цикл: ждать очередь → прочитать протокол →
// сгенерировать аргумент → отправить.
func debateLoop(ctx context.Context, c *client, provider llm.Provider,
	debateID, name, persona, stance string, log *slog.Logger) error {
	for {
		var st core.TurnStatus
		if err := c.do(ctx, "GET", "/api/debates/"+debateID+"/turn?wait_sec=60", nil, &st); err != nil {
			return err
		}
		switch {
		case st.Status == core.StatusConcluded:
			log.Info("дебаты завершены", "consensus", st.Status)
			printTranscript(ctx, c, debateID)
			return nil
		case !st.YourTurn:
			continue
		}

		var msgs struct {
			Messages []core.Message `json:"messages"`
		}
		if err := c.do(ctx, "GET", "/api/debates/"+debateID+"/messages", nil, &msgs); err != nil {
			return err
		}
		var d core.DebateView
		if err := c.do(ctx, "GET", "/api/debates/"+debateID, nil, &d); err != nil {
			return err
		}

		log.Info("моя очередь, генерирую аргумент", "round", st.CurrentRound, "mode", d.Mode)
		text, err := generateArgument(ctx, provider, d, name, persona, stance,
			msgs.Messages, st.CurrentRound, st.TotalRounds)
		if err != nil {
			// Пауза, чтобы не молотить LLM в цикле: ход либо получится
			// со следующей попытки, либо истечёт по таймауту на сервере.
			log.Error("генерация аргумента", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
			}
			continue
		}
		body := map[string]string{"text": text}
		if d.Mode == core.ModeHybrid {
			cleanText, supportID, supportName := extractVote(text, name, d.Participants)
			body["text"] = cleanText
			if supportID != "" {
				body["support_agent_id"] = supportID
				log.Info("голосую", "за", supportName)
			}
		}
		if err := c.do(ctx, "POST", "/api/debates/"+debateID+"/messages", body, nil); err != nil {
			log.Error("отправка аргумента", "err", err)
			continue
		}
		log.Info("аргумент отправлен", "round", st.CurrentRound, "len", len(body["text"]))
	}
}

func generateArgument(ctx context.Context, provider llm.Provider, d core.DebateView,
	name, persona, stance string, msgs []core.Message, round, total int) (string, error) {
	question := d.Question
	system := fmt.Sprintf(
		`Ты участник экспертных дебатов. Твоё имя: %s.
Твоя позиция и характер: %s

Правила:
- Говори только от своего имени, не пиши реплики за других участников.
- Реагируй на конкретные аргументы других: соглашайся, оспаривай, уточняй.
- Если чужой аргумент убедителен — честно признай это и скорректируй позицию.
- Цель дебатов — найти лучшее решение вопроса, а не победить любой ценой.
- Отвечай ёмко: 2–4 абзаца, без воды и повторов уже сказанного.`,
		name, persona)
	if stance != "" {
		system += "\nТвоя заявленная позиция в этих дебатах: " + stance
	}

	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Вопрос на обсуждение:\n%s\n\n", question)
	if d.Description != "" {
		fmt.Fprintf(&prompt, "Контекст дискуссии:\n%s\n\n", d.Description)
	}
	if len(msgs) == 0 {
		prompt.WriteString("Дискуссия только начинается — ты выступаешь первым.\n")
	} else {
		prompt.WriteString("Протокол дискуссии на данный момент:\n\n")
		prompt.WriteString(renderTranscript(msgs))
	}
	fmt.Fprintf(&prompt, "\nИдёт раунд %d из %d. Твоя очередь высказаться.", round, total)
	if d.Mode == core.ModeHybrid {
		names := make([]string, 0, len(d.Participants))
		for _, p := range d.Participants {
			names = append(names, p.Name)
		}
		fmt.Fprintf(&prompt, "\n\nЭти дебаты завершаются, когда все участники поддержат одну позицию."+
			" Участники: %s. Последней строкой ответа напиши ровно: \"ПОДДЕРЖИВАЮ: <имя участника>\" —"+
			" чью позицию ты сейчас считаешь верной (своё имя, если остаёшься при своей).",
			strings.Join(names, ", "))
	}

	return provider.Stream(ctx, system,
		[]llm.Message{{Role: llm.RoleUser, Content: prompt.String()}}, nil)
}

// extractVote вырезает из ответа LLM маркер "ПОДДЕРЖИВАЮ: <имя>" и
// возвращает очищенный текст + голос (agent_id участника с этим именем).
func extractVote(text, selfName string, parts []core.Participant) (string, string, string) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-3; i-- {
		line := strings.TrimSpace(lines[i])
		rest, ok := cutPrefixFold(line, "ПОДДЕРЖИВАЮ:")
		if !ok {
			continue
		}
		votedName := strings.Trim(strings.TrimSpace(rest), `"'.«»`)
		if strings.EqualFold(votedName, "себя") {
			votedName = selfName
		}
		clean := strings.TrimSpace(strings.Join(append(lines[:i:i], lines[i+1:]...), "\n"))
		for _, p := range parts {
			if strings.EqualFold(p.Name, votedName) {
				return clean, p.AgentID, p.Name
			}
		}
		return clean, "", "" // имя не распознано — голос не отправляем
	}
	return text, "", ""
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

func renderTranscript(msgs []core.Message) string {
	var sb strings.Builder
	round := 0
	for _, m := range msgs {
		if m.Round != round {
			round = m.Round
			fmt.Fprintf(&sb, "--- Раунд %d ---\n\n", round)
		}
		fmt.Fprintf(&sb, "[%s]:\n%s\n\n", m.SpeakerName, strings.TrimSpace(m.Text))
	}
	return sb.String()
}

func printTranscript(ctx context.Context, c *client, debateID string) {
	var msgs struct {
		Messages []core.Message `json:"messages"`
	}
	if err := c.do(ctx, "GET", "/api/debates/"+debateID+"/messages", nil, &msgs); err != nil {
		return
	}
	fmt.Println("\n========== Итоговый протокол ==========")
	fmt.Print(renderTranscript(msgs.Messages))
}

// --- HTTP-клиент ---

func (c *client) do(ctx context.Context, method, path string, body, out any) (err error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, resp.Body.Close())
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// --- Утилиты ---

func buildProvider() (llm.Provider, error) {
	model := envOr("AGENT_MODEL", "claude-opus-5")
	switch envOr("AGENT_PROVIDER", "anthropic") {
	case "anthropic":
		return llm.NewAnthropicProvider("", model, 4096), nil
	case "openai":
		return llm.NewOpenAICompatProvider("", os.Getenv("AGENT_BASE_URL"), model, 4096), nil
	default:
		return nil, fmt.Errorf("AGENT_PROVIDER: ожидается anthropic или openai")
	}
}

func retry(ctx context.Context, attempts int, delay time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}
