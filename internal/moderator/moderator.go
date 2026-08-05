// Package moderator — серверный LLM-модератор дебатов.
package moderator

import (
	"context"
	"fmt"
	"strings"

	"court/internal/llm"
)

const (
	consensusMarker     = "КОНСЕНСУС: ДА"
	openQuestionsHeader = "ОТКРЫТЫЕ ВОПРОСЫ"
)

// Moderator подводит итоги раундов и выносит вердикт через LLM.
type Moderator struct {
	name     string
	provider llm.Provider
}

// New создаёт модератора поверх любого llm.Provider.
func New(name string, provider llm.Provider) *Moderator {
	if name == "" {
		name = "Модератор"
	}
	return &Moderator{name: name, provider: provider}
}

// Name возвращает отображаемое имя модератора.
func (m *Moderator) Name() string { return m.name }

func (m *Moderator) system(task string) string {
	return fmt.Sprintf(
		`Ты модератор дебатов между AI-агентами. Твоё имя: %s.
Ты беспристрастен, ценишь аргументы по существу и отделяешь реальные
разногласия от споров о формулировках. %s`, m.name, task)
}

// CheckRound подводит итог раунда и определяет, достигнут ли консенсус.
func (m *Moderator) CheckRound(ctx context.Context, question, transcript string, round int) (bool, string, error) {
	prompt := fmt.Sprintf(
		`Вопрос на обсуждение:
%s

Протокол дискуссии:

%s

Завершился раунд %d. Сделай следующее:
1. Кратко (3–5 предложений) подведи итог раунда: по каким пунктам участники сходятся, по каким спорят.
2. Перечисли открытые вопросы — предметные разногласия, по которым участники ещё не сошлись. Сюда входят и «детали, которые можно уточнить позже»: пока участники не согласовали их явно, вопрос открыт. Раздел начни строкой "ОТКРЫТЫЕ ВОПРОСЫ:", пункты дай нумерованным списком. Если открытых вопросов не осталось, напиши ровно "ОТКРЫТЫЕ ВОПРОСЫ: НЕТ".
3. Последней строкой ответа напиши ровно одно из двух: "КОНСЕНСУС: ДА" или "КОНСЕНСУС: НЕТ". Консенсус возможен только при пустом списке открытых вопросов.`,
		question, transcript, round,
	)
	text, err := m.provider.Stream(ctx,
		m.system("Твоя задача — подводить промежуточные итоги и определять, достигнут ли консенсус."),
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}}, nil)
	if err != nil {
		return false, "", err
	}
	consensus := strings.Contains(strings.ToUpper(text), consensusMarker) && openQuestionsEmpty(text)
	return consensus, text, nil
}

// openQuestionsEmpty сообщает, пуст ли раздел "ОТКРЫТЫЕ ВОПРОСЫ" в ответе
// модератора. Консенсус засчитывается только при явном "ОТКРЫТЫЕ ВОПРОСЫ: НЕТ":
// раздел с пунктами или без раздела вовсе — вопросы считаются оставшимися,
// даже если модель написала "КОНСЕНСУС: ДА".
func openQuestionsEmpty(text string) bool {
	up := strings.ToUpper(text)
	idx := strings.Index(up, openQuestionsHeader)
	if idx < 0 {
		return false
	}
	for _, line := range strings.Split(up[idx+len(openQuestionsHeader):], "\n") {
		line = strings.Trim(line, " \t:*#._-")
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, "НЕТ")
	}
	return false
}

// Summary подводит итог раунда без решения о консенсусе (режим hybrid,
// где консенсус определяют голоса участников).
func (m *Moderator) Summary(ctx context.Context, question, transcript string, round int) (string, error) {
	prompt := fmt.Sprintf(
		`Вопрос на обсуждение:
%s

Протокол дискуссии:

%s

Завершился раунд %d. Кратко (3–5 предложений) подведи итог раунда:
по каким пунктам участники сходятся, по каким спорят, чьи позиции получают
поддержку (голоса участников указаны в протоколе). Решение о консенсусе
принимают сами участники голосованием — не выноси его за них.`,
		question, transcript, round,
	)
	return m.provider.Stream(ctx,
		m.system("Твоя задача — подводить промежуточные итоги дискуссии."),
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}}, nil)
}

// Verdict выносит итоговое решение по завершённой дискуссии.
func (m *Moderator) Verdict(ctx context.Context, question, transcript string) (string, error) {
	prompt := fmt.Sprintf(
		`Вопрос на обсуждение:
%s

Полный протокол дискуссии:

%s

Дискуссия завершена. Сформулируй итог:
1. Финальный ответ на вопрос — согласованное решение или, если консенсуса нет, наиболее обоснованная позиция.
2. Ключевые аргументы, на которых оно основано (укажи, кто их высказал).
3. Оставшиеся разногласия и открытые вопросы, если они есть.`,
		question, transcript,
	)
	return m.provider.Stream(ctx,
		m.system("Дискуссия завершена — твоя задача вынести итоговое решение."),
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}}, nil)
}
