// Package core — доменная модель и логика дебатов.
package core

import "time"

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

// Типы событий для подписчиков (SSE, long-poll).
const (
	EventJoined    = "participant_joined"
	EventStarted   = "debate_started"
	EventTurn      = "turn_started"
	EventMessage   = "message"
	EventSkipped   = "turn_skipped"
	EventConcluded = "debate_concluded"
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
