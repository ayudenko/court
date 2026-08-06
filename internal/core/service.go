package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Ограничения сервиса.
const (
	MaxParticipants   = 10
	MaxArgumentLen    = 20000
	MinTurnTimeout    = 30
	MaxTurnTimeout    = 1800
	DefaultTimeoutSec = 180
	DefaultRounds     = 3
	MaxRounds         = 10
	MaxPrepTime       = 3600

	// moderationTimeout ограничивает итог раунда и вердикт (до двух LLM-вызовов),
	// чтобы зависший провайдер не держал дебаты в статусе moderating вечно.
	moderationTimeout = 3 * time.Minute
)

// Типичные ошибки бизнес-логики (транслируются в HTTP-статусы на уровне API).
var (
	ErrNotYourTurn  = errors.New("сейчас не ваша очередь")
	ErrBadState     = errors.New("действие недопустимо в текущем статусе дебатов")
	ErrForbidden    = errors.New("действие доступно только создателю дебатов")
	ErrValidation   = errors.New("некорректные данные")
	ErrUnauthorized = errors.New("неверный API-ключ")
)

// Storage — то, что ядру нужно от хранилища.
type Storage interface {
	CreateAgent(a Agent, credential Credential, keyHash string) error
	AgentByCredentialHash(hash string) (Agent, error)
	AgentByID(id string) (Agent, error)
	CreateDebate(d Debate) error
	UpdateDebate(d Debate) error
	DeleteDebate(id string) error
	GetDebate(id string) (Debate, error)
	ListDebates(status string, limit int) ([]Debate, error)
	ActiveDebates() ([]Debate, error)
	AddParticipant(debateID, agentID, stance string, at time.Time) error
	Participants(debateID string) ([]Participant, error)
	AddMessage(m Message) (int64, error)
	Messages(debateID string, afterSeq int64) ([]Message, error)
}

// Moderator — серверный модератор дебатов.
type Moderator interface {
	Name() string
	// CheckRound подводит итог раунда и решает, достигнут ли консенсус
	// (режим moderator).
	CheckRound(ctx context.Context, question, transcript string, round int, allowedSeqs []int64) (RoundSummary, error)
	// Summary подводит итог раунда без решения о консенсусе (режим hybrid).
	Summary(ctx context.Context, question, transcript string, round int, allowedSeqs []int64) (RoundSummary, error)
	// Verdict выносит финальное решение по всей дискуссии.
	Verdict(ctx context.Context, question, transcript string, allowedSeqs []int64) (ModerationVerdict, error)
}

// Service — вся бизнес-логика дебатов. Потокобезопасен.
type Service struct {
	store     Storage
	hub       *Hub
	moderator Moderator
	log       *slog.Logger
	now       func() time.Time
	newID     func(string) string

	// mu сериализует переходы состояний дебатов.
	mu chan struct{} // семафор на 1 — mutex, совместимый с context
}

// ServiceOption настраивает заменяемые источники недетерминированности.
// Production использует криптографические ID и системные часы; record/replay
// сценарии подменяют их, чтобы golden-трассы были побитово воспроизводимы.
type ServiceOption func(*Service)

// WithClock задаёт источник текущего времени для доменных записей и дедлайнов.
func WithClock(now func() time.Time) ServiceOption {
	return func(service *Service) {
		if now != nil {
			service.now = now
		}
	}
}

// WithIDGenerator задаёт генератор непрозрачных идентификаторов доменных сущностей.
func WithIDGenerator(generator func(prefix string) string) ServiceOption {
	return func(service *Service) {
		if generator != nil {
			service.newID = generator
		}
	}
}

// NewService собирает сервис.
func NewService(store Storage, hub *Hub, mod Moderator, log *slog.Logger, options ...ServiceOption) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		store: store, hub: hub, moderator: mod, log: log,
		now: time.Now, newID: newID, mu: make(chan struct{}, 1),
	}
	for _, option := range options {
		if option != nil {
			option(s)
		}
	}
	return s
}

func (s *Service) nowUTC() time.Time { return s.now().UTC() }

func (s *Service) lock()   { s.mu <- struct{}{} }
func (s *Service) unlock() { <-s.mu }

// Run запускает фоновые процессы: тикер дедлайнов и восстановление
// зависших модераций после рестарта. Блокируется до отмены ctx.
func (s *Service) Run(ctx context.Context) {
	s.recover(ctx)
	// Process deadlines once at startup so a turn that expired while the
	// process was stopped is not left pending until the first ticker tick.
	s.expireTurns(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.expireTurns(ctx)
		}
	}
}

// recover перезапускает модерацию для дебатов, застрявших в статусе
// moderating после рестарта сервера.
func (s *Service) recover(ctx context.Context) {
	debates, err := s.store.ActiveDebates()
	if err != nil {
		s.log.Error("восстановление после рестарта", "err", err)
		return
	}
	for _, d := range debates {
		if d.Status == StatusModerating {
			s.log.Info("возобновляю модерацию", "debate", d.ID, "round", d.CurrentRound)
			go s.moderate(ctx, d.ID)
		}
	}
}

// expireTurns пропускает ходы, по которым истёк дедлайн.
func (s *Service) expireTurns(ctx context.Context) {
	s.lock()
	defer s.unlock()
	debates, err := s.store.ActiveDebates()
	if err != nil {
		s.log.Error("проверка дедлайнов", "err", err)
		return
	}
	now := s.nowUTC()
	for _, d := range debates {
		if d.Status == StatusPreparing && !d.TurnDeadline.After(now) {
			parts, err := s.store.Participants(d.ID)
			if err != nil || len(parts) == 0 {
				s.log.Error("окончание подготовки: участники", "debate", d.ID, "err", err)
				continue
			}
			s.log.Info("подготовка завершена, начинаю раунд 1", "debate", d.ID)
			if err := s.beginFirstRound(&d, parts); err != nil {
				s.log.Error("окончание подготовки", "debate", d.ID, "err", err)
			}
			continue
		}
		if d.Status != StatusRunning || d.TurnAgentID == "" || d.TurnDeadline.After(now) {
			continue
		}
		agent, err := s.store.AgentByID(d.TurnAgentID)
		name := d.TurnAgentID
		if err == nil {
			name = agent.Name
		}
		if _, err := s.appendMessage(d.ID, d.CurrentRound, "", "система", KindSystem,
			fmt.Sprintf("%s пропустил ход (истекло время ответа).", name)); err != nil {
			s.log.Error("сохранение пропущенного хода", "debate", d.ID, "err", err)
			continue
		}
		s.hub.Publish(Event{Type: EventSkipped, DebateID: d.ID, Round: d.CurrentRound,
			AgentID: d.TurnAgentID, AgentName: name})
		s.log.Info("ход пропущен по таймауту", "debate", d.ID, "agent", d.TurnAgentID)
		if err := s.advanceTurn(ctx, d); err != nil {
			s.log.Error("продвижение хода", "debate", d.ID, "err", err)
		}
	}
}

// --- Агенты ---

// RegisterAgent создаёт агента и возвращает его вместе с API-ключом.
// Ключ показывается один раз, хранится только его хэш.
func (s *Service) RegisterAgent(name, persona string) (Agent, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return Agent{}, "", fmt.Errorf("%w: имя обязательно, до 100 символов", ErrValidation)
	}
	if len(persona) > 2000 {
		return Agent{}, "", fmt.Errorf("%w: persona до 2000 символов", ErrValidation)
	}
	agent := Agent{ID: s.newID("agt"), Name: name, Persona: persona, CreatedAt: s.nowUTC()}
	key := "ck_" + randHex(32)
	credential := Credential{ID: s.newID("crd"), AgentID: agent.ID, CreatedAt: agent.CreatedAt}
	if err := s.store.CreateAgent(agent, credential, hashKey(key)); err != nil {
		return Agent{}, "", err
	}
	return agent, key, nil
}

// Authenticate находит агента по API-ключу.
func (s *Service) Authenticate(apiKey string) (Agent, error) {
	a, err := s.store.AgentByCredentialHash(hashKey(apiKey))
	if err != nil {
		return Agent{}, ErrUnauthorized
	}
	return a, nil
}

// --- Жизненный цикл дебатов ---

// DebateView — дискуссия с участниками для выдачи наружу.
// Votes заполняется в режиме hybrid — текущие голоса активных спикеров.
type DebateView struct {
	Debate
	TurnAgentID   string        `json:"turn_agent_id,omitempty"`
	TurnAgentName string        `json:"turn_agent_name,omitempty"`
	TurnDeadline  *time.Time    `json:"turn_deadline,omitempty"`
	Participants  []Participant `json:"participants"`
	Votes         []Vote        `json:"votes,omitempty"`
}

// CreateDebateParams — параметры новой дискуссии.
type CreateDebateParams struct {
	Question       string     // вопрос (обязателен, до 4000 символов)
	Description    string     // контекст: предыстория, ограничения, критерии решения (до 8000)
	Stance         string     // публичная позиция создателя
	Mode           DebateMode // moderator (по умолчанию) | hybrid
	Rounds         int
	TurnTimeoutSec int
	PrepTimeSec    int  // фаза подготовки перед раундом 1 (0 — без неё, до 3600)
	Observer       bool // создатель — организатор-наблюдатель, не участвует в дискуссии
}

// CreateDebate создаёт дискуссию в статусе open; создатель сразу участник,
// кроме режима Observer — тогда он лишь организатор (может запустить дебаты,
// но хода не получает).
func (s *Service) CreateDebate(creator Agent, p CreateDebateParams) (DebateView, error) {
	question := strings.TrimSpace(p.Question)
	if question == "" || len(question) > 4000 {
		return DebateView{}, fmt.Errorf("%w: вопрос обязателен, до 4000 символов", ErrValidation)
	}
	description := strings.TrimSpace(p.Description)
	if len(description) > 8000 {
		return DebateView{}, fmt.Errorf("%w: description до 8000 символов", ErrValidation)
	}
	mode := p.Mode
	if mode == "" {
		mode = ModeModerator
	}
	if mode != ModeModerator && mode != ModeHybrid {
		return DebateView{}, fmt.Errorf("%w: mode — moderator или hybrid", ErrValidation)
	}
	rounds := p.Rounds
	if rounds == 0 {
		rounds = DefaultRounds
	}
	if rounds < 1 || rounds > MaxRounds {
		return DebateView{}, fmt.Errorf("%w: раундов от 1 до %d", ErrValidation, MaxRounds)
	}
	turnTimeoutSec := p.TurnTimeoutSec
	if turnTimeoutSec == 0 {
		turnTimeoutSec = DefaultTimeoutSec
	}
	if turnTimeoutSec < MinTurnTimeout || turnTimeoutSec > MaxTurnTimeout {
		return DebateView{}, fmt.Errorf("%w: таймаут хода от %d до %d секунд", ErrValidation, MinTurnTimeout, MaxTurnTimeout)
	}
	if p.PrepTimeSec < 0 || p.PrepTimeSec > MaxPrepTime {
		return DebateView{}, fmt.Errorf("%w: prep_time_sec от 0 до %d секунд", ErrValidation, MaxPrepTime)
	}
	d := Debate{
		ID:          s.newID("dbt"),
		Question:    question,
		Description: description,
		Mode:        mode,
		Status:      StatusOpen,
		Rounds:      rounds,
		TurnTimeout: turnTimeoutSec,
		PrepTime:    p.PrepTimeSec,
		CreatorID:   creator.ID,
		CreatedAt:   s.nowUTC(),
	}
	s.lock()
	defer s.unlock()
	if err := s.store.CreateDebate(d); err != nil {
		return DebateView{}, err
	}
	if !p.Observer {
		if err := s.store.AddParticipant(d.ID, creator.ID, p.Stance, s.nowUTC()); err != nil {
			return DebateView{}, err
		}
	}
	return s.view(d)
}

// subject — «о чём дебаты» для промптов модератора: вопрос + контекст.
func subject(d Debate) string {
	if d.Description == "" {
		return d.Question
	}
	return d.Question + "\n\nКонтекст дискуссии:\n" + d.Description
}

// JoinDebate присоединяет агента к открытой дискуссии.
func (s *Service) JoinDebate(agent Agent, debateID, stance string) (DebateView, error) {
	s.lock()
	defer s.unlock()
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		return DebateView{}, err
	}
	if d.Status != StatusOpen {
		return DebateView{}, fmt.Errorf("%w: присоединяться можно только к открытым дебатам", ErrBadState)
	}
	parts, err := s.store.Participants(debateID)
	if err != nil {
		return DebateView{}, err
	}
	if len(parts) >= MaxParticipants {
		return DebateView{}, fmt.Errorf("%w: достигнут максимум участников (%d)", ErrBadState, MaxParticipants)
	}
	for _, p := range parts {
		if p.AgentID == agent.ID {
			return DebateView{}, fmt.Errorf("%w: вы уже участвуете", ErrBadState)
		}
	}
	if err := s.store.AddParticipant(debateID, agent.ID, stance, s.nowUTC()); err != nil {
		return DebateView{}, err
	}
	s.hub.Publish(Event{Type: EventJoined, DebateID: debateID, AgentID: agent.ID, AgentName: agent.Name})
	return s.view(d)
}

// StartDebate запускает дискуссию (только создатель, минимум два участника).
func (s *Service) StartDebate(agent Agent, debateID string) (DebateView, error) {
	s.lock()
	defer s.unlock()
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		return DebateView{}, err
	}
	if d.CreatorID != agent.ID {
		return DebateView{}, ErrForbidden
	}
	if d.Status != StatusOpen {
		return DebateView{}, fmt.Errorf("%w: дебаты уже запущены или завершены", ErrBadState)
	}
	parts, err := s.store.Participants(debateID)
	if err != nil {
		return DebateView{}, err
	}
	if len(parts) < 2 {
		return DebateView{}, fmt.Errorf("%w: нужно минимум два участника", ErrBadState)
	}
	if d.PrepTime > 0 {
		// Фаза подготовки: участники изучают материалы, ходов нет.
		d.Status = StatusPreparing
		d.TurnAgentID = ""
		d.TurnDeadline = s.nowUTC().Add(time.Duration(d.PrepTime) * time.Second)
		if err := s.store.UpdateDebate(d); err != nil {
			return DebateView{}, err
		}
		s.hub.Publish(Event{Type: EventStarted, DebateID: d.ID, Deadline: d.TurnDeadline})
		return s.view(d)
	}
	if err := s.beginFirstRound(&d, parts); err != nil {
		return DebateView{}, err
	}
	return s.view(d)
}

// DeleteDebate удаляет дискуссию вместе с протоколом (только создатель).
// Ожидающие очереди агенты и SSE-наблюдатели получают событие debate_deleted;
// их последующие запросы к дебатам вернут «не найдено».
func (s *Service) DeleteDebate(agent Agent, debateID string) error {
	s.lock()
	defer s.unlock()
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		return err
	}
	if d.CreatorID != agent.ID {
		return ErrForbidden
	}
	if err := s.store.DeleteDebate(debateID); err != nil {
		return err
	}
	s.hub.Publish(Event{Type: EventDeleted, DebateID: debateID})
	return nil
}

// beginFirstRound переводит дебаты в раунд 1. Вызывается под локом.
func (s *Service) beginFirstRound(d *Debate, parts []Participant) error {
	d.Status = StatusRunning
	d.CurrentRound = 1
	d.TurnAgentID = parts[0].AgentID
	d.TurnDeadline = s.nowUTC().Add(time.Duration(d.TurnTimeout) * time.Second)
	if err := s.store.UpdateDebate(*d); err != nil {
		return err
	}
	s.hub.Publish(Event{Type: EventStarted, DebateID: d.ID, Round: 1})
	s.hub.Publish(Event{Type: EventTurn, DebateID: d.ID, Round: 1,
		AgentID: parts[0].AgentID, AgentName: parts[0].Name, Deadline: d.TurnDeadline})
	return nil
}

// PostArgument принимает реплику от агента, чья сейчас очередь.
// supportID — необязательный голос «поддерживаю позицию этого участника»
// (пустой = свою); в режиме hybrid голоса определяют консенсус.
func (s *Service) PostArgument(ctx context.Context, agent Agent, debateID, text, supportID string) (Message, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > MaxArgumentLen {
		return Message{}, fmt.Errorf("%w: текст обязателен, до %d символов", ErrValidation, MaxArgumentLen)
	}
	s.lock()
	defer s.unlock()
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		return Message{}, err
	}
	if d.Status != StatusRunning {
		return Message{}, fmt.Errorf("%w: дебаты не в стадии дискуссии (%s)", ErrBadState, d.Status)
	}
	if d.TurnAgentID != agent.ID {
		return Message{}, ErrNotYourTurn
	}
	supportName := ""
	if supportID != "" {
		parts, err := s.store.Participants(debateID)
		if err != nil {
			return Message{}, err
		}
		for _, p := range parts {
			if p.AgentID == supportID {
				supportName = p.Name
				break
			}
		}
		if supportName == "" {
			return Message{}, fmt.Errorf("%w: support_agent_id должен указывать на участника дебатов", ErrValidation)
		}
	}
	msg, err := s.appendArgument(debateID, d.CurrentRound, agent, text, supportID, supportName)
	if err != nil {
		return Message{}, err
	}
	if err := s.advanceTurn(ctx, d); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// advanceTurn передаёт ход следующему участнику или запускает модерацию.
// Вызывается под локом.
func (s *Service) advanceTurn(ctx context.Context, d Debate) error {
	parts, err := s.store.Participants(d.ID)
	if err != nil {
		return err
	}
	idx := -1
	for i, p := range parts {
		if p.AgentID == d.TurnAgentID {
			idx = i
			break
		}
	}
	if idx >= 0 && idx+1 < len(parts) {
		next := parts[idx+1]
		d.TurnAgentID = next.AgentID
		d.TurnDeadline = s.nowUTC().Add(time.Duration(d.TurnTimeout) * time.Second)
		if err := s.store.UpdateDebate(d); err != nil {
			return err
		}
		s.hub.Publish(Event{Type: EventTurn, DebateID: d.ID, Round: d.CurrentRound,
			AgentID: next.AgentID, AgentName: next.Name, Deadline: d.TurnDeadline})
		return nil
	}
	// Раунд завершён — модерация.
	d.Status = StatusModerating
	d.TurnAgentID = ""
	d.TurnDeadline = time.Time{}
	if err := s.store.UpdateDebate(d); err != nil {
		return err
	}
	// Контекст здесь — обычно контекст HTTP-запроса агента, закрывшего раунд;
	// он отменяется сразу после ответа, поэтому модерация живёт без его отмены.
	go s.moderate(context.WithoutCancel(ctx), d.ID)
	return nil
}

// moderate подводит итог раунда. Запускается в отдельной горутине,
// лок берёт только на запись результата.
func (s *Service) moderate(ctx context.Context, debateID string) {
	ctx, cancel := context.WithTimeout(ctx, moderationTimeout)
	defer cancel()
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		s.log.Error("модерация: чтение дебатов", "debate", debateID, "err", err)
		return
	}
	if d.Mode == ModeHybrid {
		s.moderateHybrid(ctx, d)
		return
	}
	msgs, err := s.store.Messages(debateID, 0)
	if err != nil {
		s.log.Error("модерация: чтение протокола", "debate", debateID, "err", err)
		return
	}
	transcript := renderTranscriptText(msgs)
	allowedSeqs := messageSeqs(msgs)
	storedSummary, storedVerdict, err := moderationMessagesForRound(msgs, d.CurrentRound)
	if err != nil {
		s.log.Error("модерация: неоднозначные сохранённые результаты", "debate", debateID, "err", err)
		return
	}

	consensus := false
	lastRound := d.CurrentRound >= d.Rounds
	if storedVerdict != nil {
		consensus = storedVerdict.Verdict.Consensus
	} else if !lastRound {
		if storedSummary != nil {
			consensus = roundSummaryReachedConsensus(*storedSummary.RoundSummary)
		} else {
			summary, err := s.moderator.CheckRound(ctx, subject(d), transcript, d.CurrentRound, allowedSeqs)
			s.lock()
			if err != nil {
				s.log.Error("модерация: итог раунда", "debate", debateID, "err", err)
				_, _ = s.appendMessage(debateID, d.CurrentRound, "", "система", KindSystem,
					"Модератор недоступен, дискуссия продолжается без промежуточного итога.")
			} else {
				summary.Consensus = roundSummaryReachedConsensus(summary)
				consensus = summary.Consensus
				if _, err := s.appendSummary(debateID, d.CurrentRound, s.moderator.Name(), summary); err != nil {
					s.log.Error("модерация: сохранение итога раунда", "debate", debateID, "err", err)
					s.unlock()
					return
				}
			}
			s.unlock()
			msgs, err = s.store.Messages(debateID, 0)
			if err != nil {
				s.log.Error("модерация: повторное чтение протокола", "debate", debateID, "err", err)
				return
			}
			transcript = renderTranscriptText(msgs)
			allowedSeqs = messageSeqs(msgs)
		}
	}

	if lastRound || consensus || storedVerdict != nil {
		var verdict ModerationVerdict
		if storedVerdict != nil {
			verdict = *storedVerdict.Verdict
		} else {
			verdict, err = s.moderator.Verdict(ctx, subject(d), transcript, allowedSeqs)
		}
		s.lock()
		defer s.unlock()
		if err != nil {
			s.log.Error("модерация: вердикт", "debate", debateID, "err", err)
			_, _ = s.appendMessage(debateID, d.CurrentRound, "", "система", KindSystem,
				"Модератор недоступен, дебаты завершены без вердикта.")
		} else if storedVerdict == nil {
			consensus = verdict.Consensus
			if _, err := s.appendVerdict(debateID, d.CurrentRound, s.moderator.Name(), verdict); err != nil {
				s.log.Error("модерация: сохранение вердикта", "debate", debateID, "err", err)
				return
			}
		} else {
			consensus = verdict.Consensus
		}
		d.Status = StatusConcluded
		d.Consensus = consensus
		d.TurnAgentID = ""
		d.TurnDeadline = time.Time{}
		if err := s.store.UpdateDebate(d); err != nil {
			s.log.Error("модерация: сохранение статуса", "debate", debateID, "err", err)
			return
		}
		s.hub.Publish(Event{Type: EventConcluded, DebateID: debateID, Round: d.CurrentRound, Consensus: consensus})
		return
	}

	// Следующий раунд.
	s.lock()
	defer s.unlock()
	parts, err := s.store.Participants(debateID)
	if err != nil || len(parts) == 0 {
		s.log.Error("модерация: участники", "debate", debateID, "err", err)
		return
	}
	d.Status = StatusRunning
	d.CurrentRound++
	d.TurnAgentID = parts[0].AgentID
	d.TurnDeadline = s.nowUTC().Add(time.Duration(d.TurnTimeout) * time.Second)
	if err := s.store.UpdateDebate(d); err != nil {
		s.log.Error("модерация: сохранение раунда", "debate", debateID, "err", err)
		return
	}
	s.hub.Publish(Event{Type: EventTurn, DebateID: d.ID, Round: d.CurrentRound,
		AgentID: parts[0].AgentID, AgentName: parts[0].Name, Deadline: d.TurnDeadline})
}

func roundSummaryReachedConsensus(summary RoundSummary) bool {
	return summary.Consensus && len(summary.UnresolvedQuestions) == 0
}

// moderateHybrid — режим hybrid: консенсус определяют голоса участников
// (единогласие активных спикеров), LLM-модератор опционален.
func (s *Service) moderateHybrid(ctx context.Context, d Debate) {
	parts, err := s.store.Participants(d.ID)
	if err != nil {
		s.log.Error("гибрид: участники", "debate", d.ID, "err", err)
		return
	}
	msgs, err := s.store.Messages(d.ID, 0)
	if err != nil {
		s.log.Error("гибрид: протокол", "debate", d.ID, "err", err)
		return
	}
	votes := currentVotes(parts, msgs)
	consensus := unanimity(votes)
	lastRound := d.CurrentRound >= d.Rounds
	storedSummary, storedVerdict, err := moderationMessagesForRound(msgs, d.CurrentRound)
	if err != nil {
		s.log.Error("гибрид: неоднозначные сохранённые результаты", "debate", d.ID, "err", err)
		return
	}

	if storedVerdict == nil && !lastRound && !consensus {
		// Промежуточное резюме — опциональный слой: без LLM просто едем дальше.
		if storedSummary == nil {
			transcript := renderTranscriptText(msgs)
			if summary, err := s.moderator.Summary(ctx, subject(d), transcript, d.CurrentRound, messageSeqs(msgs)); err != nil {
				s.log.Warn("гибрид: резюме раунда недоступно", "debate", d.ID, "err", err)
			} else {
				// In hybrid mode only participant votes decide consensus. Preserve that
				// invariant even if a provider ignores the structured prompt.
				summary.Consensus = false
				s.lock()
				if _, err := s.appendSummary(d.ID, d.CurrentRound, s.moderator.Name(), summary); err != nil {
					s.log.Error("гибрид: сохранение резюме раунда", "debate", d.ID, "err", err)
					s.unlock()
					return
				}
				s.unlock()
			}
		}
		s.lock()
		defer s.unlock()
		d.Status = StatusRunning
		d.CurrentRound++
		d.TurnAgentID = parts[0].AgentID
		d.TurnDeadline = s.nowUTC().Add(time.Duration(d.TurnTimeout) * time.Second)
		if err := s.store.UpdateDebate(d); err != nil {
			s.log.Error("гибрид: сохранение раунда", "debate", d.ID, "err", err)
			return
		}
		s.hub.Publish(Event{Type: EventTurn, DebateID: d.ID, Round: d.CurrentRound,
			AgentID: parts[0].AgentID, AgentName: parts[0].Name, Deadline: d.TurnDeadline})
		return
	}

	// Завершение: вердикт LLM, при недоступности — детерминированный по голосам.
	var verdict ModerationVerdict
	verdictText := ""
	speaker := s.moderator.Name()
	if storedVerdict != nil {
		verdict = *storedVerdict.Verdict
		verdictText = storedVerdict.Text
		speaker = storedVerdict.SpeakerName
	} else {
		verdict, err = s.moderator.Verdict(ctx, subject(d), renderTranscriptText(msgs), messageSeqs(msgs))
		verdictText = verdict.Text()
		if err != nil {
			s.log.Warn("гибрид: LLM-вердикт недоступен, использую подсчёт голосов", "debate", d.ID, "err", err)
			verdict, verdictText = hybridVerdict(votes, msgs, consensus)
			speaker = "система"
		}
	}
	// В hybrid исход консенсуса определяют только голоса участников. Модель
	// формулирует решение, но не может переопределить этот протокольный факт.
	verdict.Consensus = consensus
	s.lock()
	defer s.unlock()
	if storedVerdict == nil {
		if _, err := s.appendVerdictText(d.ID, d.CurrentRound, speaker, verdict, verdictText); err != nil {
			s.log.Error("гибрид: сохранение вердикта", "debate", d.ID, "err", err)
			return
		}
	}
	d.Status = StatusConcluded
	d.Consensus = consensus
	d.TurnAgentID = ""
	d.TurnDeadline = time.Time{}
	if err := s.store.UpdateDebate(d); err != nil {
		s.log.Error("гибрид: сохранение статуса", "debate", d.ID, "err", err)
		return
	}
	s.hub.Publish(Event{Type: EventConcluded, DebateID: d.ID, Round: d.CurrentRound, Consensus: consensus})
}

// currentVotes — последние голоса активных спикеров: чью позицию поддерживает
// каждый участник по его последней реплике (без явного голоса — свою).
func currentVotes(parts []Participant, msgs []Message) []Vote {
	names := make(map[string]string, len(parts))
	for _, p := range parts {
		names[p.AgentID] = p.Name
	}
	last := make(map[string]string) // speakerID -> supportID
	var order []string
	for _, m := range msgs {
		if m.Kind != KindArgument || m.SpeakerID == "" {
			continue
		}
		if _, seen := last[m.SpeakerID]; !seen {
			order = append(order, m.SpeakerID)
		}
		target := m.SupportID
		if target == "" {
			target = m.SpeakerID // без явного голоса — стоит на своей позиции
		}
		last[m.SpeakerID] = target
	}
	votes := make([]Vote, 0, len(last))
	for _, id := range order {
		votes = append(votes, Vote{
			AgentID:      id,
			AgentName:    names[id],
			SupportsID:   last[id],
			SupportsName: names[last[id]],
		})
	}
	return votes
}

// unanimity — все активные спикеры (минимум два) поддерживают одну позицию.
func unanimity(votes []Vote) bool {
	if len(votes) < 2 {
		return false
	}
	target := votes[0].SupportsID
	for _, v := range votes[1:] {
		if v.SupportsID != target {
			return false
		}
	}
	return true
}

// hybridVerdict — детерминированный вердикт по голосам, когда LLM недоступен.
func hybridVerdict(votes []Vote, msgs []Message, consensus bool) (ModerationVerdict, string) {
	var sb strings.Builder
	if consensus {
		sb.WriteString("Консенсус достигнут голосованием участников.\n\n")
	} else {
		sb.WriteString("Консенсус не достигнут. Дебаты завершены по исчерпанию раундов.\n\n")
	}
	sb.WriteString("Голоса участников:\n")
	tally := make(map[string]int)
	for _, v := range votes {
		fmt.Fprintf(&sb, "- %s → %s\n", v.AgentName, v.SupportsName)
		tally[v.SupportsID]++
	}
	// Позиция с наибольшей поддержкой (если лидер единственный).
	best, bestCount, unique := "", 0, false
	for id, n := range tally {
		switch {
		case n > bestCount:
			best, bestCount, unique = id, n, true
		case n == bestCount:
			unique = false
		}
	}
	if unique {
		var name, text string
		for _, m := range msgs {
			if m.Kind == KindArgument && m.SpeakerID == best {
				name, text = m.SpeakerName, m.Text // последняя реплика победителя
			}
		}
		if text != "" {
			fmt.Fprintf(&sb, "\nНаибольшую поддержку получила позиция участника %s (голосов: %d).\nЕго итоговая реплика:\n\n%s\n", name, bestCount, text)
		}
	} else if len(tally) > 0 {
		sb.WriteString("\nГолоса разделились поровну — итоговая позиция не определена.\n")
	}
	text := sb.String()
	return ModerationVerdict{
		FinalAnswer:         strings.TrimSpace(text),
		Claims:              []ModerationClaim{},
		UnresolvedQuestions: []string{},
		Decisions:           []string{},
		Consensus:           consensus,
	}, text
}

// appendArgument сохраняет реплику участника с голосом. Вызывается под локом.
func (s *Service) appendArgument(debateID string, round int, agent Agent, text, supportID, supportName string) (Message, error) {
	m := Message{
		DebateID:    debateID,
		Round:       round,
		SpeakerID:   agent.ID,
		SpeakerName: agent.Name,
		Kind:        KindArgument,
		Text:        text,
		SupportID:   supportID,
		SupportName: supportName,
		CreatedAt:   s.nowUTC(),
	}
	seq, err := s.store.AddMessage(m)
	if err != nil {
		s.log.Error("сохранение сообщения", "debate", debateID, "err", err)
		return Message{}, err
	}
	m.Seq = seq
	s.hub.Publish(Event{Type: EventMessage, DebateID: debateID, Round: round,
		AgentID: agent.ID, AgentName: agent.Name, Message: &m})
	return m, nil
}

// appendMessage сохраняет сообщение и публикует событие. Вызывается под локом.
func (s *Service) appendMessage(debateID string, round int, speakerID, speakerName, kind, text string) (Message, error) {
	m := Message{
		DebateID:    debateID,
		Round:       round,
		SpeakerID:   speakerID,
		SpeakerName: speakerName,
		Kind:        kind,
		Text:        text,
		CreatedAt:   s.nowUTC(),
	}
	return s.appendProtocolMessage(m)
}

// appendSummary сохраняет и текст для старых клиентов, и типизированный
// результат для экспорта, replay и проверки citations.
func (s *Service) appendSummary(debateID string, round int, speakerName string, summary RoundSummary) (Message, error) {
	m := Message{
		DebateID:     debateID,
		Round:        round,
		SpeakerName:  speakerName,
		Kind:         KindSummary,
		Text:         summary.Text(),
		RoundSummary: &summary,
		CreatedAt:    s.nowUTC(),
	}
	return s.appendProtocolMessage(m)
}

// appendVerdict сохраняет обе совместимые формы итогового решения.
func (s *Service) appendVerdict(debateID string, round int, speakerName string, verdict ModerationVerdict) (Message, error) {
	return s.appendVerdictText(debateID, round, speakerName, verdict, verdict.Text())
}

func (s *Service) appendVerdictText(
	debateID string,
	round int,
	speakerName string,
	verdict ModerationVerdict,
	text string,
) (Message, error) {
	m := Message{
		DebateID:    debateID,
		Round:       round,
		SpeakerName: speakerName,
		Kind:        KindVerdict,
		Text:        text,
		Verdict:     &verdict,
		CreatedAt:   s.nowUTC(),
	}
	return s.appendProtocolMessage(m)
}

func (s *Service) appendProtocolMessage(m Message) (Message, error) {
	seq, err := s.store.AddMessage(m)
	if err != nil {
		s.log.Error("сохранение сообщения", "debate", m.DebateID, "err", err)
		return Message{}, err
	}
	m.Seq = seq
	s.hub.Publish(Event{Type: EventMessage, DebateID: m.DebateID, Round: m.Round,
		AgentID: m.SpeakerID, AgentName: m.SpeakerName, Message: &m})
	return m, nil
}

// --- Чтение ---

// GetDebate возвращает дискуссию с участниками.
func (s *Service) GetDebate(debateID string) (DebateView, error) {
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		return DebateView{}, err
	}
	return s.view(d)
}

// ListDebates возвращает список дискуссий.
func (s *Service) ListDebates(status string, limit int) ([]DebateView, error) {
	debates, err := s.store.ListDebates(status, limit)
	if err != nil {
		return nil, err
	}
	out := make([]DebateView, 0, len(debates))
	for _, d := range debates {
		v, err := s.view(d)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Messages возвращает протокол после указанного seq.
func (s *Service) Messages(debateID string, afterSeq int64) ([]Message, error) {
	if _, err := s.store.GetDebate(debateID); err != nil {
		return nil, err
	}
	return s.store.Messages(debateID, afterSeq)
}

// Subscribe/Unsubscribe — доступ к хабу событий.
func (s *Service) Subscribe(debateID string) chan Event       { return s.hub.Subscribe(debateID) }
func (s *Service) Unsubscribe(debateID string, ch chan Event) { s.hub.Unsubscribe(debateID, ch) }

// WaitTurn блокируется, пока не настанет очередь агента, дебаты не завершатся
// или не истечёт maxWait. Возвращает актуальный статус очереди.
func (s *Service) WaitTurn(ctx context.Context, agent Agent, debateID string, maxWait time.Duration) (TurnStatus, error) {
	ch := s.hub.Subscribe(debateID)
	defer s.hub.Unsubscribe(debateID, ch)
	deadline := time.Now().Add(maxWait)
	for {
		st, err := s.TurnStatus(agent, debateID)
		if err != nil {
			return TurnStatus{}, err
		}
		if st.YourTurn || st.Status == StatusConcluded || time.Now().After(deadline) {
			return st, nil
		}
		wait := min(time.Until(deadline), 2*time.Second)
		select {
		case <-ctx.Done():
			return st, nil
		case <-ch:
		case <-time.After(wait):
		}
	}
}

// TurnStatus возвращает состояние очереди для агента.
func (s *Service) TurnStatus(agent Agent, debateID string) (TurnStatus, error) {
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		return TurnStatus{}, err
	}
	st := TurnStatus{
		DebateID:     d.ID,
		Status:       d.Status,
		CurrentRound: d.CurrentRound,
		TotalRounds:  d.Rounds,
		YourTurn:     d.Status == StatusRunning && d.TurnAgentID == agent.ID,
	}
	if d.TurnAgentID != "" {
		if a, err := s.store.AgentByID(d.TurnAgentID); err == nil {
			st.TurnAgent = a.Name
		}
	}
	// Дедлайн: в running — конец хода, в preparing — момент старта раунда 1.
	if !d.TurnDeadline.IsZero() {
		st.DeadlineSec = max(0, int(d.TurnDeadline.Sub(s.nowUTC()).Seconds()))
	}
	return st, nil
}

func moderationMessagesForRound(msgs []Message, round int) (*Message, *Message, error) {
	var summary, verdict *Message
	for i := range msgs {
		message := &msgs[i]
		if message.Round != round {
			continue
		}
		switch {
		case message.Kind == KindSummary && message.RoundSummary != nil:
			if summary != nil {
				return nil, nil, fmt.Errorf("multiple typed summaries in round %d", round)
			}
			summary = message
		case message.Kind == KindVerdict && message.Verdict != nil:
			if verdict != nil {
				return nil, nil, fmt.Errorf("multiple typed verdicts in round %d", round)
			}
			verdict = message
		}
	}
	return summary, verdict, nil
}

func messageSeqs(msgs []Message) []int64 {
	seqs := make([]int64, 0, len(msgs))
	for _, msg := range msgs {
		seqs = append(seqs, msg.Seq)
	}
	return seqs
}

func renderTranscriptText(msgs []Message) string {
	var sb strings.Builder
	round := 0
	for _, m := range msgs {
		if m.Round != round {
			round = m.Round
			fmt.Fprintf(&sb, "--- Раунд %d ---\n\n", round)
		}
		header := m.SpeakerName
		if m.SupportName != "" && m.SupportID != m.SpeakerID {
			header += " (поддерживает позицию: " + m.SupportName + ")"
		}
		fmt.Fprintf(&sb, "[#%d, %s]:\n%s\n\n", m.Seq, header, strings.TrimSpace(m.Text))
	}
	return sb.String()
}

func (s *Service) view(d Debate) (DebateView, error) {
	parts, err := s.store.Participants(d.ID)
	if err != nil {
		return DebateView{}, err
	}
	if parts == nil {
		parts = []Participant{} // в JSON — [], не null: клиенты считают participants.length
	}
	v := DebateView{Debate: d, Participants: parts}
	if d.Status == StatusOpen {
		// Контекст дискуссии раскрывается только со старта (фаза подготовки
		// или раунд 1): до него участники видят один вопрос и не получают
		// форы за раннее присоединение.
		v.Description = ""
	}
	if d.TurnAgentID != "" {
		v.TurnAgentID = d.TurnAgentID
		for _, p := range parts {
			if p.AgentID == d.TurnAgentID {
				v.TurnAgentName = p.Name
			}
		}
		if !d.TurnDeadline.IsZero() {
			t := d.TurnDeadline
			v.TurnDeadline = &t
		}
	}
	if d.Status == StatusPreparing && !d.TurnDeadline.IsZero() {
		t := d.TurnDeadline
		v.TurnDeadline = &t
	}
	if d.Mode == ModeHybrid && d.Status != StatusOpen {
		if msgs, err := s.store.Messages(d.ID, 0); err == nil {
			v.Votes = currentVotes(parts, msgs)
		}
	}
	return v, nil
}

// --- Утилиты ---

func newID(prefix string) string { return prefix + "_" + randHex(12) }

func randHex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand не должен отказывать
	}
	return hex.EncodeToString(b)
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
