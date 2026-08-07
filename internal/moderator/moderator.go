// Package moderator — серверный LLM-модератор дебатов.
package moderator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"court/internal/core"
	"court/internal/llm"
)

const (
	roundSummaryTool = "submit_round_summary"
	verdictTool      = "submit_verdict"
)

// MaxNameLen ограничивает отображаемое имя модератора. Имя попадает в системную
// инструкцию каждого вызова, а её размер входит в фиксированный резерв, из
// которого считается верхняя граница расхода: имя без предела делало бы этот
// резерв необязательным (docs/adr/0004-moderator-spend-ceiling.md).
const MaxNameLen = 100

// Moderator подводит итоги раундов и выносит вердикт через LLM.
type Moderator struct {
	name     string
	provider llm.Provider
}

// New создаёт модератора поверх любого llm.Provider. Слишком длинное имя
// обрезается: см. MaxNameLen.
func New(name string, provider llm.Provider) *Moderator {
	if name == "" {
		name = "Модератор"
	}
	if runes := []rune(name); len(runes) > MaxNameLen {
		name = string(runes[:MaxNameLen])
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

// usage переводит расход провайдера в доменную единицу учёта.
func usage(u llm.Usage) core.ModerationUsage {
	return core.ModerationUsage{
		Billed:       u.Billed,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
	}
}

// CheckRound подводит итог раунда и определяет, достигнут ли консенсус.
func (m *Moderator) CheckRound(ctx context.Context, question, transcript string, round int, allowedSeqs []int64) (core.RoundSummary, core.ModerationUsage, error) {
	prompt := fmt.Sprintf(
		`Вопрос на обсуждение:
%s

Протокол дискуссии:

%s

Завершился раунд %d. Сделай следующее:
1. Кратко подведи итог раунда.
2. Выдели проверяемые тезисы и для каждого укажи seq исходных реплик из заголовков вида [#seq, участник].
3. Перечисли согласованные решения и открытые предметные вопросы. «Детали, которые можно уточнить позже» остаются открытыми, пока участники явно их не согласовали.
4. Отметь consensus=true только если открытых вопросов нет и участники явно согласовали решение.
Верни результат только вызовом предоставленного инструмента.`,
		question, transcript, round,
	)
	return m.roundSummary(ctx, allowedSeqs,
		m.system("Твоя задача — подводить промежуточные итоги и определять, достигнут ли консенсус."),
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}})
}

// Summary подводит итог раунда без решения о консенсусе (режим hybrid,
// где консенсус определяют голоса участников).
func (m *Moderator) Summary(ctx context.Context, question, transcript string, round int, allowedSeqs []int64) (core.RoundSummary, core.ModerationUsage, error) {
	prompt := fmt.Sprintf(
		`Вопрос на обсуждение:
%s

Протокол дискуссии:

%s

Завершился раунд %d. Кратко (3–5 предложений) подведи итог раунда:
по каким пунктам участники сходятся, по каким спорят, чьи позиции получают
поддержку (голоса участников указаны в протоколе). Решение о консенсусе
принимают сами участники голосованием — не выноси его за них и установи
consensus=false. Для каждого тезиса укажи seq исходных реплик из заголовков
вида [#seq, участник]. Верни результат только вызовом предоставленного инструмента.`,
		question, transcript, round,
	)
	return m.roundSummary(ctx, allowedSeqs,
		m.system("Твоя задача — подводить промежуточные итоги дискуссии."),
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}})
}

// Verdict выносит итоговое решение по завершённой дискуссии.
func (m *Moderator) Verdict(ctx context.Context, question, transcript string, allowedSeqs []int64) (core.ModerationVerdict, core.ModerationUsage, error) {
	prompt := fmt.Sprintf(
		`Вопрос на обсуждение:
%s

Полный протокол дискуссии:

%s

Дискуссия завершена. Сформулируй итог:
1. Финальный ответ на вопрос — согласованное решение или, если консенсуса нет, наиболее обоснованная позиция.
2. Ключевые тезисы, на которых оно основано; для каждого укажи seq исходных реплик из заголовков вида [#seq, участник].
3. Согласованные решения и оставшиеся разногласия или открытые вопросы.
4. Установи consensus=true только если открытых вопросов нет и участники явно согласовали решение.
Верни результат только вызовом предоставленного инструмента.`,
		question, transcript,
	)
	tool := verdictSchema()
	raw, spent, err := m.provider.CallTool(ctx,
		m.system("Дискуссия завершена — твоя задача вынести итоговое решение."),
		[]llm.Message{{Role: llm.RoleUser, Content: prompt}}, tool)
	if err != nil {
		return core.ModerationVerdict{}, usage(spent), err
	}
	result, err := decodeStructured[core.ModerationVerdict](raw, tool.Required)
	if err != nil {
		return core.ModerationVerdict{}, usage(spent), fmt.Errorf("структурированный вердикт: %w", err)
	}
	if err := validateVerdict(result, allowedSeqs); err != nil {
		return core.ModerationVerdict{}, usage(spent), fmt.Errorf("структурированный вердикт: %w", err)
	}
	return result, usage(spent), nil
}

func (m *Moderator) roundSummary(ctx context.Context, allowedSeqs []int64, system string, msgs []llm.Message) (core.RoundSummary, core.ModerationUsage, error) {
	tool := roundSummarySchema()
	raw, spent, err := m.provider.CallTool(ctx, system, msgs, tool)
	if err != nil {
		return core.RoundSummary{}, usage(spent), err
	}
	result, err := decodeStructured[core.RoundSummary](raw, tool.Required)
	if err != nil {
		return core.RoundSummary{}, usage(spent), fmt.Errorf("структурированное резюме: %w", err)
	}
	if err := validateRoundSummary(result, allowedSeqs); err != nil {
		return core.RoundSummary{}, usage(spent), fmt.Errorf("структурированное резюме: %w", err)
	}
	return result, usage(spent), nil
}

func roundSummarySchema() llm.Tool {
	return llm.Tool{
		Name:        roundSummaryTool,
		Description: "Return the typed summary of a completed debate round.",
		Properties: map[string]any{
			"summary":              map[string]any{"type": "string", "description": "Concise round summary."},
			"claims":               claimsSchema(),
			"unresolved_questions": stringArraySchema("Substantive questions the participants have not resolved."),
			"decisions":            stringArraySchema("Decisions explicitly agreed by the participants."),
			"consensus":            map[string]any{"type": "boolean", "description": "True only when the unresolved_questions array is empty and the participants explicitly agree."},
		},
		Required: []string{"summary", "claims", "unresolved_questions", "decisions", "consensus"},
	}
}

func verdictSchema() llm.Tool {
	return llm.Tool{
		Name:        verdictTool,
		Description: "Return the typed final verdict for a completed debate.",
		Properties: map[string]any{
			"final_answer":         map[string]any{"type": "string", "description": "The final answer to the debate question."},
			"claims":               claimsSchema(),
			"unresolved_questions": stringArraySchema("Remaining disagreements or open questions."),
			"decisions":            stringArraySchema("Decisions explicitly agreed by the participants."),
			"consensus":            map[string]any{"type": "boolean", "description": "True only when the unresolved_questions array is empty and the participants explicitly agree."},
		},
		Required: []string{"final_answer", "claims", "unresolved_questions", "decisions", "consensus"},
	}
}

func claimsSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "A claim grounded in the transcript."},
				"citations": map[string]any{
					"type":        "array",
					"minItems":    1,
					"items":       map[string]any{"type": "integer", "minimum": 1},
					"description": "Message seq values supporting the claim.",
				},
			},
			"required": []string{"text", "citations"},
		},
	}
}

func stringArraySchema(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": description,
	}
}

func decodeStructured[T any](raw json.RawMessage, required []string) (T, error) {
	var result T
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return result, err
	}
	for _, field := range required {
		value, ok := fields[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return result, fmt.Errorf("обязательное поле %q отсутствует или равно null", field)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return result, fmt.Errorf("лишние данные после JSON-объекта")
		}
		return result, err
	}
	return result, nil
}

func validateRoundSummary(summary core.RoundSummary, allowedSeqs []int64) error {
	if strings.TrimSpace(summary.Summary) == "" {
		return fmt.Errorf("summary обязателен")
	}
	if summary.Consensus && len(summary.UnresolvedQuestions) > 0 {
		return fmt.Errorf("consensus=true при непустом unresolved_questions")
	}
	if err := validateStrings("unresolved_questions", summary.UnresolvedQuestions); err != nil {
		return err
	}
	if err := validateStrings("decisions", summary.Decisions); err != nil {
		return err
	}
	return validateClaims(summary.Claims, allowedSeqs)
}

func validateVerdict(verdict core.ModerationVerdict, allowedSeqs []int64) error {
	if strings.TrimSpace(verdict.FinalAnswer) == "" {
		return fmt.Errorf("final_answer обязателен")
	}
	if verdict.Consensus && len(verdict.UnresolvedQuestions) > 0 {
		return fmt.Errorf("consensus=true при непустом unresolved_questions")
	}
	if err := validateStrings("unresolved_questions", verdict.UnresolvedQuestions); err != nil {
		return err
	}
	if err := validateStrings("decisions", verdict.Decisions); err != nil {
		return err
	}
	return validateClaims(verdict.Claims, allowedSeqs)
}

func validateStrings(field string, values []string) error {
	for i, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s[%d] пуст", field, i)
		}
	}
	return nil
}

func validateClaims(claims []core.ModerationClaim, allowedSeqs []int64) error {
	available := make(map[int64]struct{}, len(allowedSeqs))
	for _, seq := range allowedSeqs {
		available[seq] = struct{}{}
	}
	for i, claim := range claims {
		if strings.TrimSpace(claim.Text) == "" {
			return fmt.Errorf("claims[%d].text пуст", i)
		}
		if len(claim.Citations) == 0 {
			return fmt.Errorf("claims[%d].citations пуст", i)
		}
		for _, seq := range claim.Citations {
			if _, ok := available[seq]; !ok {
				return fmt.Errorf("claims[%d] ссылается на отсутствующий seq #%d", i, seq)
			}
		}
	}
	return nil
}
