// Package core — доменная модель и логика дебатов.
package core

import (
	"fmt"
	"strings"
	"time"
)

// Agent — зарегистрированный внешний агент.
type Agent struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Persona   string    `json:"persona,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// DebateMode — способ определения консенсуса.
type DebateMode string

const (
	// ModeModerator — консенсус и вердикт определяет серверный LLM-модератор.
	ModeModerator DebateMode = "moderator"
	// ModeHybrid — консенсус определяют голоса участников (единогласие
	// активных спикеров); LLM-модератор, если доступен, добавляет резюме
	// и вердикт, иначе вердикт строится детерминированно по голосам.
	ModeHybrid DebateMode = "hybrid"
)

// DebateStatus — стадия жизненного цикла дебатов.
type DebateStatus string

const (
	// StatusOpen — набор участников, можно присоединяться.
	StatusOpen DebateStatus = "open"
	// StatusPreparing — фаза подготовки: участники изучают материалы,
	// ходов нет; по истечении prep_time_sec начнётся раунд 1.
	StatusPreparing DebateStatus = "preparing"
	// StatusRunning — идёт дискуссия, агенты ходят по очереди.
	StatusRunning DebateStatus = "running"
	// StatusModerating — модератор подводит итог раунда или выносит вердикт.
	StatusModerating DebateStatus = "moderating"
	// StatusConcluded — дебаты завершены.
	StatusConcluded DebateStatus = "concluded"
)

// Debate — одна дискуссия.
type Debate struct {
	ID           string       `json:"id"`
	Question     string       `json:"question"`
	Description  string       `json:"description,omitempty"`
	Mode         DebateMode   `json:"mode"`
	Status       DebateStatus `json:"status"`
	Rounds       int          `json:"rounds"`
	CurrentRound int          `json:"current_round"`
	TurnTimeout  int          `json:"turn_timeout_sec"`
	PrepTime     int          `json:"prep_time_sec,omitempty"`
	CreatorID    string       `json:"creator_id"`
	TurnAgentID  string       `json:"-"`
	TurnDeadline time.Time    `json:"-"`
	Consensus    bool         `json:"consensus"`
	CreatedAt    time.Time    `json:"created_at"`
}

// Participant — агент, присоединившийся к дебатам.
type Participant struct {
	AgentID  string    `json:"agent_id"`
	Name     string    `json:"name"`
	Stance   string    `json:"stance,omitempty"`
	JoinedAt time.Time `json:"joined_at"`
}

// Виды сообщений в протоколе.
const (
	KindArgument = "argument" // реплика участника
	KindSummary  = "summary"  // промежуточный итог модератора
	KindVerdict  = "verdict"  // финальное решение модератора
	KindSystem   = "system"   // служебное (пропуск хода и т.п.)
)

// Message — запись в протоколе дебатов. Support* — голос спикера:
// чью позицию он поддерживает на момент этой реплики (режим hybrid).
type Message struct {
	Seq         int64     `json:"seq"`
	DebateID    string    `json:"debate_id"`
	Round       int       `json:"round"`
	SpeakerID   string    `json:"speaker_id,omitempty"`
	SpeakerName string    `json:"speaker_name"`
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	SupportID   string    `json:"support_id,omitempty"`
	SupportName string    `json:"support_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Vote — текущий голос участника (последняя заявленная поддержка).
type Vote struct {
	AgentID      string `json:"agent_id"`
	AgentName    string `json:"agent_name"`
	SupportsID   string `json:"supports_id"`
	SupportsName string `json:"supports_name"`
}

// ModerationClaim — тезис модератора со ссылками на исходные сообщения по seq.
// Ссылки сохраняют проверяемость резюме и вердикта после сокращения контекста.
type ModerationClaim struct {
	Text      string  `json:"text"`
	Citations []int64 `json:"citations"`
}

// RoundSummary — типизированный результат модерации завершившегося раунда.
type RoundSummary struct {
	Summary             string            `json:"summary"`
	Claims              []ModerationClaim `json:"claims"`
	UnresolvedQuestions []string          `json:"unresolved_questions"`
	Decisions           []string          `json:"decisions"`
	Consensus           bool              `json:"consensus"`
}

// Text возвращает человекочитаемое представление структурированного резюме.
// Бизнес-логика не разбирает этот текст: consensus берётся из отдельного поля.
func (s RoundSummary) Text() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(s.Summary))
	renderClaims(&b, s.Claims)
	renderList(&b, "Согласованные решения", s.Decisions)
	renderList(&b, "Открытые вопросы", s.UnresolvedQuestions)
	return strings.TrimSpace(b.String())
}

// ModerationVerdict — типизированное финальное решение модератора.
type ModerationVerdict struct {
	FinalAnswer         string            `json:"final_answer"`
	Claims              []ModerationClaim `json:"claims"`
	UnresolvedQuestions []string          `json:"unresolved_questions"`
	Decisions           []string          `json:"decisions"`
	Consensus           bool              `json:"consensus"`
}

// Text возвращает человекочитаемое представление структурированного вердикта.
func (v ModerationVerdict) Text() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(v.FinalAnswer))
	renderClaims(&b, v.Claims)
	renderList(&b, "Согласованные решения", v.Decisions)
	renderList(&b, "Оставшиеся разногласия и открытые вопросы", v.UnresolvedQuestions)
	return strings.TrimSpace(b.String())
}

func renderClaims(b *strings.Builder, claims []ModerationClaim) {
	if len(claims) == 0 {
		return
	}
	b.WriteString("\n\nКлючевые тезисы:\n")
	for _, claim := range claims {
		fmt.Fprintf(b, "- %s", strings.TrimSpace(claim.Text))
		if len(claim.Citations) > 0 {
			b.WriteString(" [")
			for i, seq := range claim.Citations {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(b, "#%d", seq)
			}
			b.WriteString("]")
		}
		b.WriteByte('\n')
	}
}

func renderList(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n", heading)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", strings.TrimSpace(item))
	}
}

// Типы событий для подписчиков (SSE, long-poll).
const (
	EventJoined    = "participant_joined"
	EventStarted   = "debate_started"
	EventTurn      = "turn_started"
	EventMessage   = "message"
	EventSkipped   = "turn_skipped"
	EventConcluded = "debate_concluded"
	EventDeleted   = "debate_deleted"
)

// Event — событие в дебатах для подписчиков.
type Event struct {
	Type      string    `json:"type"`
	DebateID  string    `json:"debate_id"`
	Round     int       `json:"round,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	AgentName string    `json:"agent_name,omitempty"`
	Deadline  time.Time `json:"deadline,omitzero"`
	Message   *Message  `json:"message,omitempty"`
	Consensus bool      `json:"consensus,omitempty"`
}

// TurnStatus — ответ на «чья сейчас очередь» для конкретного агента.
type TurnStatus struct {
	DebateID     string       `json:"debate_id"`
	Status       DebateStatus `json:"status"`
	YourTurn     bool         `json:"your_turn"`
	CurrentRound int          `json:"current_round"`
	TotalRounds  int          `json:"total_rounds"`
	TurnAgent    string       `json:"turn_agent_name,omitempty"`
	DeadlineSec  int          `json:"deadline_sec,omitempty"`
}
