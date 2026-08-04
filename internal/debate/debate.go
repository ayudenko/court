// Package debate — оркестрация дебатов: раунды, протокол, модератор.
package debate

import (
	"context"
	"fmt"
	"strings"

	"court/internal/llm"
)

// Agent — участник дебатов: имя, персона и провайдер модели.
type Agent struct {
	Name     string
	Persona  string
	Provider llm.Provider
}

// Entry — одна реплика в протоколе.
type Entry struct {
	Round   int
	Speaker string
	Text    string
}

// Events — колбэки для отображения хода дискуссии (CLI, веб и т.д.).
type Events struct {
	OnRoundStart func(round, total int)
	OnTurnStart  func(speaker string)
	OnDelta      func(speaker, delta string)
	OnTurnEnd    func(speaker string)
}

// Debate — состояние одной дискуссии.
type Debate struct {
	Question   string
	Rounds     int
	Agents     []Agent
	Moderator  Agent
	Transcript []Entry
	Events     Events
}

// Result — итог дебатов.
type Result struct {
	Consensus  bool
	FinalRound int
	Verdict    string
	Transcript []Entry
}

const consensusMarker = "КОНСЕНСУС: ДА"

// Run проводит дебаты: раунды высказываний, проверка консенсуса модератором
// после каждого раунда и финальный вердикт.
func (d *Debate) Run(ctx context.Context) (*Result, error) {
	total := d.Rounds
	lastRound := 0
	consensus := false

	for round := 1; round <= total; round++ {
		lastRound = round
		if d.Events.OnRoundStart != nil {
			d.Events.OnRoundStart(round, total)
		}
		for _, agent := range d.Agents {
			text, err := d.agentTurn(ctx, agent, round, total)
			if err != nil {
				return nil, fmt.Errorf("реплика агента %q (раунд %d): %w", agent.Name, round, err)
			}
			d.Transcript = append(d.Transcript, Entry{Round: round, Speaker: agent.Name, Text: text})
		}
		if round < total {
			done, summary, err := d.moderatorCheck(ctx, round)
			if err != nil {
				return nil, fmt.Errorf("проверка консенсуса (раунд %d): %w", round, err)
			}
			d.Transcript = append(d.Transcript, Entry{Round: round, Speaker: d.Moderator.Name, Text: summary})
			if done {
				consensus = true
				break
			}
		}
	}

	verdict, err := d.moderatorVerdict(ctx)
	if err != nil {
		return nil, fmt.Errorf("итоговый вердикт: %w", err)
	}
	d.Transcript = append(d.Transcript, Entry{Round: lastRound, Speaker: d.Moderator.Name, Text: verdict})

	return &Result{
		Consensus:  consensus,
		FinalRound: lastRound,
		Verdict:    verdict,
		Transcript: d.Transcript,
	}, nil
}

func (d *Debate) agentTurn(ctx context.Context, agent Agent, round, total int) (string, error) {
	system := fmt.Sprintf(
		`Ты участник экспертных дебатов. Твоё имя: %s.
Твоя позиция и характер: %s

Правила:
- Говори только от своего имени, не пиши реплики за других участников.
- Реагируй на конкретные аргументы других: соглашайся, оспаривай, уточняй.
- Если чужой аргумент убедителен — честно признай это и скорректируй позицию.
- Цель дебатов — найти лучшее решение вопроса, а не победить любой ценой.
- Отвечай ёмко: 2–4 абзаца, без воды и повторов уже сказанного.`,
		agent.Name, agent.Persona,
	)

	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Вопрос на обсуждение:\n%s\n\n", d.Question)
	if len(d.Transcript) == 0 {
		prompt.WriteString("Дискуссия только начинается — ты выступаешь первым.\n")
	} else {
		prompt.WriteString("Протокол дискуссии на данный момент:\n\n")
		prompt.WriteString(d.renderTranscript())
		prompt.WriteString("\n")
	}
	fmt.Fprintf(&prompt, "\nИдёт раунд %d из %d. Твоя очередь высказаться.", round, total)

	return d.stream(ctx, agent, system, prompt.String())
}

func (d *Debate) moderatorCheck(ctx context.Context, round int) (bool, string, error) {
	system := fmt.Sprintf(
		`Ты модератор экспертных дебатов. Твоё имя: %s. %s
Твоя задача — следить за дискуссией, подводить промежуточные итоги и определять, достигнут ли консенсус.`,
		d.Moderator.Name, d.Moderator.Persona,
	)

	prompt := fmt.Sprintf(
		`Вопрос на обсуждение:
%s

Протокол дискуссии:

%s

Завершился раунд %d. Сделай следующее:
1. Кратко (3–5 предложений) подведи итог раунда: по каким пунктам участники сходятся, по каким спорят.
2. Реши, достигнут ли содержательный консенсус — участники сошлись в главном и новые раунды ничего не добавят.
3. Последней строкой ответа напиши ровно одно из двух: "КОНСЕНСУС: ДА" или "КОНСЕНСУС: НЕТ".`,
		d.Question, d.renderTranscript(), round,
	)

	text, err := d.stream(ctx, d.Moderator, system, prompt)
	if err != nil {
		return false, "", err
	}
	return strings.Contains(strings.ToUpper(text), consensusMarker), text, nil
}

func (d *Debate) moderatorVerdict(ctx context.Context) (string, error) {
	system := fmt.Sprintf(
		`Ты модератор экспертных дебатов. Твоё имя: %s. %s
Дискуссия завершена — твоя задача вынести итоговое решение.`,
		d.Moderator.Name, d.Moderator.Persona,
	)

	prompt := fmt.Sprintf(
		`Вопрос на обсуждение:
%s

Полный протокол дискуссии:

%s

Дискуссия завершена. Сформулируй итог:
1. Финальный ответ на вопрос — согласованное решение или, если консенсуса нет, наиболее обоснованная позиция.
2. Ключевые аргументы, на которых оно основано (укажи, кто их высказал).
3. Оставшиеся разногласия и открытые вопросы, если они есть.`,
		d.Question, d.renderTranscript(),
	)

	return d.stream(ctx, d.Moderator, system, prompt)
}

func (d *Debate) stream(ctx context.Context, agent Agent, system, prompt string) (string, error) {
	if d.Events.OnTurnStart != nil {
		d.Events.OnTurnStart(agent.Name)
	}
	text, err := agent.Provider.Stream(ctx, system,
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}},
		func(delta string) {
			if d.Events.OnDelta != nil {
				d.Events.OnDelta(agent.Name, delta)
			}
		},
	)
	if err != nil {
		return "", err
	}
	if d.Events.OnTurnEnd != nil {
		d.Events.OnTurnEnd(agent.Name)
	}
	return text, nil
}

func (d *Debate) renderTranscript() string {
	var sb strings.Builder
	currentRound := 0
	for _, e := range d.Transcript {
		if e.Round != currentRound {
			currentRound = e.Round
			fmt.Fprintf(&sb, "--- Раунд %d ---\n\n", currentRound)
		}
		fmt.Fprintf(&sb, "[%s]:\n%s\n\n", e.Speaker, strings.TrimSpace(e.Text))
	}
	return sb.String()
}
