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
	"sync"
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

	// moderationTimeout ограничивает один вызов модератора, чтобы зависший
	// провайдер не держал дебаты в статусе moderating вечно. Именно на вызов, а не
	// на весь проход: общий дедлайн на два вызова означал, что медленный итог
	// раунда оставлял вердикту мёртвый контекст, и тот списывал оценку за запрос,
	// который вообще не был отправлен.
	moderationTimeout = 3 * time.Minute

	// backgroundTick — период фонового прохода: истёкшие дедлайны ходов и
	// возобновление зависшей модерации.
	backgroundTick = 2 * time.Second

	// moderationRetryDelay — пауза перед первым повтором зависшего прохода
	// модерации. Зависает он на отказе хранилища, поэтому повтор без паузы упёрся
	// бы в тот же отказ на следующем тике, а в режиме moderator ещё и оплачивал бы
	// этим вызов модели каждые две секунды.
	moderationRetryDelay = time.Minute

	// moderationRetryMaxDelay — потолок паузы. Пауза удваивается с каждой
	// неудачей: число повторов не ограничено, а каждый читает протокол целиком, и
	// отказ хранилища задевает сразу все пишущие дебаты. Постоянная минута
	// означала бы, что сломанный том получает полное чтение каждых зависших
	// дебатов каждую минуту неограниченно долго; потолок в четверть часа оставляет
	// восстановление быстрым, а долгую аварию — дешёвой.
	moderationRetryMaxDelay = 15 * time.Minute

	// moderationMaxPaidPasses — сколько проходов одного раунда вправе заплатить за
	// вызов модели. Ограничены именно платные проходы: считать надо деньги, а не
	// живость, и повтор, который модель не зовёт, ничего не стоит и потому не
	// ограничен ничем. Достигнув потолка, раунд продолжает повторяться бесплатно
	// и доезжает до детерминированного итога — так же, как при исчерпанном
	// бюджете дебатов.
	//
	// Страховка на случай снятого потолка расхода: с потолком платные повторы
	// ограничивает он сам (docs/adr/0004-moderator-spend-ceiling.md), без него —
	// только это число.
	moderationMaxPaidPasses = 10

	// ModerationPromptOverheadBytes — запас на системную инструкцию, обвязку
	// промпта и схему инструмента, которых нет ни в вопросе, ни в протоколе.
	// Экспортируется, чтобы обвязка могла отвергнуть бюджет, которого не хватает
	// даже на пустые дебаты.
	ModerationPromptOverheadBytes = 4096
)

// Сообщения протокола о деградации. Читатель протокола обязан отличать вердикт
// модели от вердикта по голосам и пропущенный итог от потерянного, поэтому
// каждая деградация оставляет след в протоколе, а не только в логах — в обоих
// режимах и по обеим причинам. Асимметрия между режимами была бы неотличима от
// усечённого протокола: см. docs/adr/0007-protocol-conformance-suite.md.
//
// Экспортируются не ради вызывающего кода, а потому что SPEC.md публикует эти
// строки как то, что потребитель артефакта сопоставляет. Расхождение документа с
// кодом обязано падать в CI, а не у потребителя:
// TestSpecPublishesTheDegradationNoticesTheServiceEmits.
const (
	NoticeBudgetSummary = "Бюджет модератора на эти дебаты исчерпан, " +
		"дискуссия продолжается без промежуточного итога."
	// Тексты вердикта различаются по режиму, потому что различается механизм:
	// в hybrid исход определяют голоса участников, в moderator — нет.
	NoticeBudgetVerdictModerator = "Бюджет модератора на эти дебаты исчерпан, " +
		"итог зафиксирован без вердикта модели."
	NoticeBudgetVerdictHybrid = "Бюджет модератора на эти дебаты исчерпан, " +
		"итог подведён детерминированно по голосам участников."
	// Недоступность модератора: провайдер вернул ошибку либо ключа нет вовсе.
	// Развёртывание hybrid без LLM-ключа — поддерживаемый сценарий, в котором
	// таким оказывается каждый раунд, и именно там молчание протокола дороже
	// всего.
	NoticeUnavailableSummary = "Модератор недоступен, " +
		"дискуссия продолжается без промежуточного итога."
	NoticeUnavailableVerdictModerator = "Модератор недоступен, " +
		"дебаты завершены без вердикта."
	NoticeUnavailableVerdictHybrid = "Модератор недоступен, " +
		"итог подведён детерминированно по голосам участников."
)

// DegradationNotices — полный набор уведомлений о деградации. Существует, чтобы
// SPEC.md проверялся против всех строк, а не против тех, которые автор документа
// вспомнил.
func DegradationNotices() []string {
	return []string{
		NoticeBudgetSummary,
		NoticeBudgetVerdictModerator,
		NoticeBudgetVerdictHybrid,
		NoticeUnavailableSummary,
		NoticeUnavailableVerdictModerator,
		NoticeUnavailableVerdictHybrid,
	}
}

// ModeratorBudget ограничивает суммарный расход LLM-модератора на одни дебаты.
// Смысл потолка: стоимость одних дебатов должна быть конечной и известной
// заранее, потому что дебаты после старта едут сами на дедлайнах ходов и не
// требуют от инициатора ни одного запроса.
type ModeratorBudget struct {
	// DebateTokens — потолок суммарного расхода на одни дебаты в токенах.
	// 0 или меньше отключает потолок (учёт расхода при этом продолжается).
	DebateTokens int
	// OutputPerCall — резерв на ответ модели, добавляемый к оценке каждого
	// вызова. Должен совпадать с max_tokens, заданным провайдеру.
	OutputPerCall int
}

// WithModeratorBudget задаёт потолок расхода LLM-модератора на одни дебаты.
func WithModeratorBudget(budget ModeratorBudget) ServiceOption {
	return func(service *Service) {
		service.budget = budget
	}
}

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
	CreateCredential(credential Credential, keyHash string, maxActive int) error
	Credentials(agentID string) ([]Credential, error)
	RevokeCredential(agentID, credentialID string, at time.Time) error
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
	// AddModeratorTokens увеличивает накопленный расход модератора на дебаты.
	// Именно инкремент, а не запись значения: UpdateDebate пишет состояние из
	// возможно устаревшей копии Debate и затёр бы уже учтённый расход.
	AddModeratorTokens(debateID string, tokens int) error
}

// ModerationUsage — фактический расход одного вызова модератора в токенах.
//
// Billed означает «вызов вошёл в счёт владельца ключа»: ответ получен либо ждать
// перестали мы сами. Запрос, не дошедший до провайдера, в счёт не входит, и
// списывать за него оценку нельзя — иначе недоступность провайдера исчерпывала
// бы бюджет дебатов, которые ничего не потратили.
type ModerationUsage struct {
	Billed       bool
	InputTokens  int
	OutputTokens int
}

// Total — суммарный расход вызова. Отрицательные значения от провайдера
// отбрасываются: иначе ответ с усадкой вида {input: 1, output: -100} обнулял бы
// списание и снимал потолок.
func (u ModerationUsage) Total() int {
	return max(u.InputTokens, 0) + max(u.OutputTokens, 0)
}

// Reported сообщает, известен ли фактический расход вызова.
func (u ModerationUsage) Reported() bool { return u.Total() > 0 }

// Moderator — серверный модератор дебатов.
//
// Каждый метод возвращает фактический расход вызова. Расход возвращается и
// вместе с ошибкой: неудачный вызов тоже оплачен владельцем ключа, и
// потерянный здесь расход дал бы способ тратить бюджет, не уменьшая его
// (docs/adr/0004-moderator-spend-ceiling.md).
type Moderator interface {
	Name() string
	// CheckRound подводит итог раунда и решает, достигнут ли консенсус
	// (режим moderator).
	CheckRound(ctx context.Context, question, transcript string, round int, allowedSeqs []int64) (RoundSummary, ModerationUsage, error)
	// Summary подводит итог раунда без решения о консенсусе (режим hybrid).
	Summary(ctx context.Context, question, transcript string, round int, allowedSeqs []int64) (RoundSummary, ModerationUsage, error)
	// Verdict выносит финальное решение по всей дискуссии.
	Verdict(ctx context.Context, question, transcript string, allowedSeqs []int64) (ModerationVerdict, ModerationUsage, error)
}

// Service — вся бизнес-логика дебатов. Потокобезопасен.
type Service struct {
	store     Storage
	hub       *Hub
	moderator Moderator
	log       *slog.Logger
	now       func() time.Time
	newID     func(string) string
	budget    ModeratorBudget

	// mu сериализует переходы состояний дебатов.
	mu chan struct{} // семафор на 1 — mutex, совместимый с context

	// moderationMu защищает moderations. Замок отдельный от mu намеренно: решение
	// «начинать ли проход модерации» принимается на каждом фоновом тике и не
	// должно вставать в очередь за переходами состояний, которые ждут хранилище.
	moderationMu sync.Mutex
	moderations  map[string]*moderationAttempts

	// Параметры фонового прохода. Поля, а не константы, потому что тест обязан
	// увидеть повтор за миллисекунды, а не за минуту; в собранном сервисе они
	// всегда равны константам выше.
	tick              time.Duration
	moderationRetry   time.Duration
	moderationPaidCap int
}

// moderationAttempts — состояние повторов модерации одних дебатов.
// Живёт только в памяти и только про этот процесс: это «сколько раз я уже
// пробовал», а не факт дебатов. Рестарт обнуляет счёт намеренно — новый процесс
// мог подняться именно потому, что причину отказа устранили.
//
// Цена этого решения: на развёртывании с auto_stop (fly.toml) процесс встаёт и
// поднимается сам, и зависшие дебаты получают новый счёт на каждом цикле. Расход
// при этом остаётся ограничен потолком на дебаты: он лежит в хранилище и цикл
// его не сбрасывает. Сбрасывается только страховка на случай снятого потолка и
// признак сломанного учёта — то есть при сломанном учёте цена одного цикла
// простоя равна одному оплаченному вызову на раунд.
type moderationAttempts struct {
	round   int
	running bool
	tries   int
	lastTry time.Time
	// pass — номер запущенного прохода. Завершившийся проход снимает признак
	// «идёт» только со своего: горутина прошлого раунда иначе сняла бы его с
	// живого прохода следующего, и фоновый проход запустил бы второй поверх.
	pass int
	// paid — сколько проходов этого раунда заплатили за вызов модели. Повтор,
	// который не платит, ничем не ограничен: ограничивать надо деньги, а не
	// живость.
	paid int
	// hopeless — раунд отвергнут как неоднозначный: повтор не изменит ничего.
	hopeless bool
	// chargeLost — расход состоялся, а записать его не вышло. С этого момента
	// платить за эти дебаты нельзя: остаток бюджета в хранилище не сдвинулся, и
	// каждый следующий проход получал бы его заново.
	chargeLost bool
}

// moderationTrigger — кто запустил проход модерации. Нужен только логу:
// возобновление зависшего прохода обязано быть видно оператору, а закрытие
// раунда — рядовое событие.
type moderationTrigger string

const (
	triggerRoundClosed moderationTrigger = "round_closed"
	triggerResumed     moderationTrigger = "resumed"
)

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
		moderations:       make(map[string]*moderationAttempts),
		tick:              backgroundTick,
		moderationRetry:   moderationRetryDelay,
		moderationPaidCap: moderationMaxPaidPasses,
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

// lockContext ждёт замок переходов, пока жив контекст вызова. Нужен на путях,
// которые вызываются с публичной границы: отменённый запрос обязан выйти из
// очереди, а не удерживать её и не делать работу, результат которой уже некому
// прочитать.
func (s *Service) lockContext(ctx context.Context) error {
	// Проверка до select: при свободном замке и уже отменённом контексте select
	// выбрал бы ветку случайно, и отменённый запрос иногда всё равно делал бы
	// работу.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run запускает фоновые процессы: истёкшие дедлайны ходов и возобновление
// зависшей модерации. Блокируется до отмены ctx.
func (s *Service) Run(ctx context.Context) {
	// Первый проход сразу, без ожидания тика: ни ход, истёкший пока процесс
	// стоял, ни модерация, не дошедшая до записи перед остановкой, не должны
	// ждать лишние секунды.
	s.resumeStuckModeration(ctx)
	s.expireTurns(ctx)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.resumeStuckModeration(ctx)
			s.expireTurns(ctx)
		}
	}
}

// resumeStuckModeration возвращает к жизни дебаты, застрявшие в статусе
// moderating.
//
// Застревают они на отказе хранилища: ветка, которая не смогла записать
// объяснение исхода, намеренно не завершает дебаты — протокол без этой записи
// неотличим от усечённого. Пока повтор делался только при старте процесса,
// выйти из такого состояния можно было единственным способом — перезапустить
// сервер, а участники всё это время не могли ничего: ход не принадлежит никому
// и дедлайна нет. Ни одно правило conformance об этом не сообщает, потому что
// экспорт таких дебатов валиден (issue #40).
func (s *Service) resumeStuckModeration(ctx context.Context) {
	debates, err := s.store.ActiveDebates()
	if err != nil {
		s.log.Error("возобновление модерации: чтение дебатов", "err", err)
		return
	}
	active := make(map[string]struct{}, len(debates))
	for _, d := range debates {
		active[d.ID] = struct{}{}
		if d.Status == StatusModerating {
			s.moderateAsync(ctx, d.ID, d.CurrentRound, triggerResumed)
		}
	}
	s.forgetSettledModeration(active)
}

// moderateAsync запускает проход модерации, если его ещё не ведёт этот процесс
// и с прошлой попытки прошла пауза. Единственная точка запуска: и закрытие
// раунда, и возобновление зависшего проходят через неё, поэтому «модерация уже
// идёт» — свойство одного счётчика, а не двух независимых путей.
//
// Число проходов не ограничено ничем: повтор, который не зовёт модель, ничего не
// стоит, а ограничить его значило бы снова оставить дебаты ждать рестарта после
// любого отказа хранилища длиннее лимита. Ограничены платные вызовы — см.
// affordModelCall.
func (s *Service) moderateAsync(ctx context.Context, debateID string, round int, trigger moderationTrigger) {
	pass, ok := s.beginModerationAttempt(debateID, round, trigger)
	if !ok {
		return
	}
	go func() {
		defer s.endModerationAttempt(debateID, pass)
		s.moderate(ctx, debateID)
	}()
}

// beginModerationAttempt решает, начинать ли проход, и отмечает его начало.
// Второе значение — начинаем ли; первое — номер прохода для endModerationAttempt.
func (s *Service) beginModerationAttempt(debateID string, round int, trigger moderationTrigger) (int, bool) {
	s.moderationMu.Lock()
	defer s.moderationMu.Unlock()
	attempts := s.moderations[debateID]
	if attempts == nil {
		attempts = &moderationAttempts{round: round}
		s.moderations[debateID] = attempts
	}
	if round < attempts.round {
		// Вызов устарел: фоновый проход прочитал список дебатов до того, как они
		// ушли вперёд. Начать по нему значит запустить второй проход поверх
		// живого — с задвоенным расходом модератора и двумя записями на раунд,
		// вторая из которых сделает раунд неоднозначным навсегда (ADR 0002).
		return 0, false
	}
	// Закрытие нового раунда авторитетнее признака «идёт»: признак снимает
	// отложенный вызов горутины прошлого прохода, и между её последней записью и
	// этим вызовом есть окно. Дебаты, доигравшие в нём целый раунд, доказывают,
	// что прошлый проход своё дело сделал. На сервере с фоновым проходом окно
	// стоило бы одного тика, а без него — например в записи эталонных трасс, где
	// Run не запускают, — модерация не началась бы вовсе.
	//
	// Держится это на том, что раунд продвигает только сам проход модерации и
	// только под замком переходов. Путь, продвигающий раунд снаружи прохода —
	// ручное «следующий раунд», ремонтная утилита, — сломает исключение и даст
	// второй проход поверх живого: критерий отката 1 из
	// docs/adr/0008-in-process-moderation-retry.md.
	takeover := trigger == triggerRoundClosed && round > attempts.round
	if attempts.running && !takeover {
		return 0, false
	}
	if round > attempts.round {
		// Новый раунд — свой счёт: и платных проходов, и попыток. Потолок платных
		// повторов считается на раунд, иначе долгие дебаты доезжали бы до
		// последнего раунда с уже израсходованным лимитом из-за давно устранённого
		// сбоя; а несброшенный счёт попыток заставил бы закрытие следующего раунда
		// ждать паузу между повторами. chargeLost не сбрасывается: он про дебаты
		// целиком.
		attempts.round = round
		attempts.paid = 0
		attempts.tries = 0
		attempts.hopeless = false
	}
	if attempts.hopeless {
		// Повторять нечего: раунд отвергнут как неоднозначный, и это состояние не
		// лечится ни повтором, ни рестартом — только вмешательством в данные.
		// Повторять его значит читать протокол целиком раз в минуту вечно и
		// печатать ту же ошибку, топя строку, по которой её замечают.
		return 0, false
	}
	now := s.nowUTC()
	if attempts.tries > 0 {
		if now.Sub(attempts.lastTry) < s.retryBackoff(attempts.tries) {
			return 0, false
		}
		s.log.Warn("модерация: повторяю зависший проход",
			"debate", debateID, "round", round, "try", attempts.tries+1)
	} else if trigger == triggerResumed {
		// Первый проход после рестарта: раньше о нём сообщал recover, и без этой
		// строки возобновление стало бы бесшумным.
		s.log.Info("модерация: возобновляю зависший проход", "debate", debateID, "round", round)
	}
	attempts.running = true
	attempts.tries++
	attempts.lastTry = now
	attempts.pass++
	return attempts.pass, true
}

// retryBackoff — пауза перед попыткой номер tries+1. Удвоение с потолком:
// первые повторы быстрые, потому что типичный отказ хранилища короткий, а долгая
// авария не должна стоить полного чтения протокола каждую минуту.
func (s *Service) retryBackoff(tries int) time.Duration {
	delay := s.moderationRetry
	if delay <= 0 {
		// Пауза, отключённая тестом: удваивать нечего, и цикл ниже крутился бы
		// вхолостую столько раз, сколько было попыток.
		return 0
	}
	for try := 1; try < tries && delay < moderationRetryMaxDelay; try++ {
		delay *= 2
	}
	return min(delay, moderationRetryMaxDelay)
}

// giveUpOnModeration помечает раунд как неповторяемый.
func (s *Service) giveUpOnModeration(debateID string) {
	s.moderationMu.Lock()
	defer s.moderationMu.Unlock()
	if attempts := s.moderations[debateID]; attempts != nil {
		attempts.hopeless = true
	}
}

func (s *Service) endModerationAttempt(debateID string, pass int) {
	s.moderationMu.Lock()
	defer s.moderationMu.Unlock()
	if attempts := s.moderations[debateID]; attempts != nil && attempts.pass == pass {
		attempts.running = false
	}
}

// noteModerationAdmitted отмечает, что проход получил право на платный вызов.
func (s *Service) noteModerationAdmitted(debateID string) {
	s.moderationMu.Lock()
	defer s.moderationMu.Unlock()
	if attempts := s.moderations[debateID]; attempts != nil {
		attempts.paid++
	}
}

// noteChargeLost отмечает, что расход состоялся, а записать его не вышло.
// Заводит запись, если её нет: молча потерять этот факт значит продолжать
// платить вслепую — ровно тот отказ, ради которого он и запоминается.
func (s *Service) noteChargeLost(debateID string) {
	s.moderationMu.Lock()
	defer s.moderationMu.Unlock()
	attempts := s.moderations[debateID]
	if attempts == nil {
		attempts = &moderationAttempts{}
		s.moderations[debateID] = attempts
	}
	attempts.chargeLost = true
}

// chargeLost сообщает, сломан ли учёт расхода этих дебатов. Отдельное поле в
// логе, а не вывод из деградации: критерий отката 1 из
// docs/adr/0004-moderator-spend-ceiling.md считает деградации по исчерпанному
// потолку, и деградация из-за сломанного учёта в этот счёт попадать не должна —
// потолок там не срабатывал.
func (s *Service) chargeLost(debateID string) bool {
	s.moderationMu.Lock()
	defer s.moderationMu.Unlock()
	attempts := s.moderations[debateID]
	return attempts != nil && attempts.chargeLost
}

// affordModelCall сообщает, вправе ли проход ещё платить за вызов модели, и
// называет причину отказа для лога. Ответ «нет» не останавливает проход — он
// уводит его в ту же деградацию, что и недоступный модератор.
func (s *Service) affordModelCall(debateID string) (reason string, affordable bool) {
	s.moderationMu.Lock()
	defer s.moderationMu.Unlock()
	attempts := s.moderations[debateID]
	if attempts == nil {
		return "", true
	}
	// Учёт расхода сломан: следующий вызов невозможно ни ограничить, ни
	// посчитать. Платить вслепую нельзя — это единственный способ, которым
	// повтор мог бы превысить потолок дебатов.
	if attempts.chargeLost {
		return "charge_lost", false
	}
	// Страховка на случай снятого потолка: с ним платные повторы ограничивает
	// сам потолок, без него — только этот счёт.
	if attempts.paid >= s.moderationPaidCap {
		return "paid_cap", false
	}
	return "", true
}

// forgetSettledModeration убирает состояние дебатов, которых уже нет среди
// активных. Без этого карта росла бы на каждые прошедшие через процесс дебаты:
// завершённые в ActiveDebates не возвращаются и сами о себе сообщить не могут.
// Признак «активные», а не «в moderating»: счёт платных повторов и признак
// сломанного учёта обязаны пережить смену раунда.
func (s *Service) forgetSettledModeration(active map[string]struct{}) {
	s.moderationMu.Lock()
	defer s.moderationMu.Unlock()
	for id, attempts := range s.moderations {
		if attempts.running {
			continue
		}
		if _, still := active[id]; !still {
			delete(s.moderations, id)
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
		if _, err := s.appendMessage(d.ID, d.CurrentRound, "", SystemSpeakerName, KindSystem,
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

// MaxActiveCredentials — потолок одновременно действующих ключей агента.
// Лимит частоты ограничивает скорость появления секретов, но не их число;
// набор действующих ключей должен быть конечным (ADR 0005).
const MaxActiveCredentials = 10

// IssueCredential выпускает агенту дополнительный ключ. Ключ возвращается
// открытым один раз; дальше существует только его хэш.
//
// Выпуск и отзыв логируются на границе транспорта, а не здесь: единственный
// признак, отличающий ротацию владельцем от угона украденным ключом, — адрес
// клиента, а он ядру намеренно неизвестен (ADR 0003). См. LogCredentialEvent.
func (s *Service) IssueCredential(agent Agent) (Credential, string, error) {
	key := "ck_" + randHex(32)
	credential := Credential{ID: s.newID("crd"), AgentID: agent.ID, CreatedAt: s.nowUTC()}
	if err := s.store.CreateCredential(credential, hashKey(key), MaxActiveCredentials); err != nil {
		return Credential{}, "", err
	}
	return credential, key, nil
}

// Credentials отдаёт ключи агента — метаданные без секретов.
func (s *Service) Credentials(agent Agent) ([]Credential, error) {
	return s.store.Credentials(agent.ID)
}

// RevokeCredential отзывает ключ агента. Владение, отсутствие и отзыв
// последнего действующего ключа проверяет хранилище: только там проверка и
// запись атомарны.
func (s *Service) RevokeCredential(agent Agent, credentialID string) error {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID == "" {
		return fmt.Errorf("%w: нужен credential_id", ErrValidation)
	}
	return s.store.RevokeCredential(agent.ID, credentialID, s.nowUTC())
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
	s.moderateAsync(context.WithoutCancel(ctx), d.ID, d.CurrentRound, triggerRoundClosed)
	return nil
}

// moderate подводит итог раунда. Запускается в отдельной горутине,
// лок берёт только на запись результата.
func (s *Service) moderate(ctx context.Context, debateID string) {
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		s.log.Error("модерация: чтение дебатов", "debate", debateID, "err", err)
		return
	}
	if d.Status != StatusModerating {
		// Возобновление опоздало: проход, начатый раньше, уже увёл дебаты дальше.
		// Без этой проверки повтор переписал бы завершённые дебаты и опубликовал
		// второе событие о завершении — состояние гонки, которого не было, пока
		// повтор случался только при старте процесса.
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
		s.giveUpOnModeration(debateID)
		return
	}

	consensus := false
	lastRound := d.CurrentRound >= d.Rounds
	if storedVerdict != nil {
		consensus = storedVerdict.Verdict.Consensus
	} else if !lastRound {
		if storedSummary != nil {
			consensus = roundSummaryReachedConsensus(*storedSummary.RoundSummary)
		} else if committed := committedDegradation(msgs, d.CurrentRound,
			NoticeBudgetSummary, NoticeUnavailableSummary); committed != "" {
			// Раунд уже объявил, что итога в нём не будет. Записать его теперь —
			// значит опровергнуть собственное уведомление: см. committedDegradation.
			s.log.Info("модерация: раунд уже объявил пропуск итога",
				"debate", debateID, "round", d.CurrentRound)
		} else if admission := s.admitModeration(d, subject(d), transcript); admission != admissionAllowed {
			// Вызова не будет: дискуссия продолжается без промежуточных итогов.
			// Ходы участников сервису ничего не стоят, поэтому дебаты не рвутся
			// на середине — они доедут до последнего раунда и завершатся
			// детерминированным вердиктом.
			notice := NoticeBudgetSummary
			if admission == admissionUnaffordable {
				notice = NoticeUnavailableSummary
			} else {
				s.log.Warn("модерация: бюджет дебатов исчерпан, итог раунда пропущен",
					"debate", debateID, "round", d.CurrentRound, "spent", d.ModeratorTokens)
			}
			if !noticeRecorded(msgs, d.CurrentRound, notice) {
				s.lock()
				_, _ = s.appendMessage(debateID, d.CurrentRound, "", SystemSpeakerName, KindSystem, notice)
				s.unlock()
			}
		} else {
			callCtx, cancelCall := moderationCall(ctx)
			summary, spent, err := s.moderator.CheckRound(callCtx, subject(d), transcript, d.CurrentRound, allowedSeqs)
			cancelCall()
			s.chargeModeration(&d, subject(d), transcript, spent)
			s.lock()
			if err != nil {
				s.log.Error("модерация: итог раунда", "debate", debateID, "err", err)
				if !noticeRecorded(msgs, d.CurrentRound, NoticeUnavailableSummary) {
					_, _ = s.appendMessage(debateID, d.CurrentRound, "", SystemSpeakerName, KindSystem, NoticeUnavailableSummary)
				}
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
		degraded := false
		// modelWithheld — модель не высказалась не из-за бюджета этих дебатов:
		// платить нельзя, либо раунд уже объявил об этом. В этом режиме тогда не
		// будет и вердикта.
		modelWithheld := false
		committed := committedDegradation(msgs, d.CurrentRound,
			NoticeBudgetVerdictModerator, NoticeUnavailableVerdictModerator)
		switch {
		case storedVerdict != nil:
			verdict = *storedVerdict.Verdict
		case committed == NoticeBudgetVerdictModerator:
			// Раунд уже объявил причину и обещал детерминированный вердикт.
			degraded = true
		case committed == NoticeUnavailableVerdictModerator:
			modelWithheld = true
		default:
			switch s.admitModeration(d, subject(d), transcript) {
			case admissionBudgetExhausted:
				// Деградация по бюджету: вердикта модели не будет. Консенсус остаётся
				// тем, что определили оплаченные итоги раундов, и голоса участников в
				// этом режиме на исход не влияют.
				degraded = true
				s.log.Warn("модерация: бюджет дебатов исчерпан, вердикт модели не запрашивается",
					"debate", debateID, "spent", d.ModeratorTokens, "budget", s.budget.DebateTokens)
			case admissionUnaffordable:
				modelWithheld = true
			default:
				var spent ModerationUsage
				callCtx, cancelCall := moderationCall(ctx)
				verdict, spent, err = s.moderator.Verdict(callCtx, subject(d), transcript, allowedSeqs)
				cancelCall()
				s.chargeModeration(&d, subject(d), transcript, spent)
			}
		}
		s.lock()
		defer s.unlock()
		// Причина, по которой итог подведён не моделью. Пустая строка — вердикт
		// модели. Отдельно от degraded: см. degradationCause.
		verdictNotice := ""
		switch {
		case degraded:
			// consensus здесь остаётся тем, что определили оплаченные итоги
			// раундов. Пересчитывать его по голосам нельзя: в режиме moderator
			// голоса не являются механизмом консенсуса — участник, не указавший
			// поддержку, считается голосующим за себя, — и подсчёт отдал бы исход
			// чужих дебатов любому, кто в них вошёл.
			degradedVerdict, verdictText := budgetExhaustedVerdict(consensus)
			verdictNotice = NoticeBudgetVerdictModerator
			if !noticeRecorded(msgs, d.CurrentRound, NoticeBudgetVerdictModerator) {
				if _, err := s.appendMessage(debateID, d.CurrentRound, "", SystemSpeakerName, KindSystem, NoticeBudgetVerdictModerator); err != nil {
					s.log.Error("модерация: сохранение уведомления о бюджете", "debate", debateID, "err", err)
					return
				}
			}
			if _, err := s.appendVerdictText(debateID, d.CurrentRound, SystemSpeakerName, degradedVerdict, verdictText); err != nil {
				s.log.Error("модерация: сохранение детерминированного вердикта", "debate", debateID, "err", err)
				return
			}
		// err здесь — только ошибка вызова вердикта: блок промежуточного итога
		// объявляет свою err через `:=`, поэтому его сбой сюда не доходит. Если
		// это когда-нибудь станет одной переменной, вердикт по сохранённому итогу
		// уедет в эту ветку и допишет второе, ложное уведомление.
		case err != nil || modelWithheld:
			if err != nil {
				s.log.Error("модерация: вердикт", "debate", debateID, "err", err)
			}
			verdictNotice = NoticeUnavailableVerdictModerator
			// Отказ, а не завершение: без вердикта уведомление — единственная
			// запись, объясняющая исход, и дебаты, завершённые без неё, ничем не
			// отличаются от усечённого протокола. Дебаты остаются в moderating, и
			// проход повторит resumeStuckModeration.
			if !noticeRecorded(msgs, d.CurrentRound, NoticeUnavailableVerdictModerator) {
				if _, err := s.appendMessage(debateID, d.CurrentRound, "", SystemSpeakerName, KindSystem,
					NoticeUnavailableVerdictModerator); err != nil {
					s.log.Error("модерация: сохранение уведомления о недоступности", "debate", debateID, "err", err)
					return
				}
			}
		case storedVerdict == nil:
			consensus = verdict.Consensus
			if _, err := s.appendVerdict(debateID, d.CurrentRound, s.moderator.Name(), verdict); err != nil {
				s.log.Error("модерация: сохранение вердикта", "debate", debateID, "err", err)
				return
			}
		default:
			consensus = verdict.Consensus
		}
		// attribution — откуда взялась причина деградации в итоговой строке.
		// «recovered» значит, что её восстановил повтор из протокола, а не наблюдал
		// сам проход: тогда `charge_lost` относится к этому процессу, а деградацию
		// произвёл предыдущий, и читать их как одно нельзя.
		attribution := "observed"
		switch {
		case storedVerdict != nil:
			attribution = "recovered"
			degraded, verdictNotice = storedVerdictDegradation(msgs, d.CurrentRound, *storedVerdict,
				NoticeBudgetVerdictModerator, NoticeUnavailableVerdictModerator)
		case committed != "":
			// Причину этот проход тоже не наблюдал: он подчинился уведомлению,
			// записанному предыдущим, и решения о допуске вызова не принимал.
			attribution = "recovered"
		}
		d.Status = StatusConcluded
		d.Consensus = consensus
		d.TurnAgentID = ""
		d.TurnDeadline = time.Time{}
		if err := s.store.UpdateDebate(d); err != nil {
			s.log.Error("модерация: сохранение статуса", "debate", debateID, "err", err)
			return
		}
		// Итог расхода — после успешного завершения, а не до него. Строка одна на
		// дебаты: по её полю `degraded` считается критерий отката 1 из
		// docs/adr/0004-moderator-spend-ceiling.md, и повтор незавершившегося
		// прохода не имеет права её удваивать.
		s.log.Info("расход модератора за дебаты", "debate", debateID,
			"tokens", d.ModeratorTokens, "budget", s.budget.DebateTokens,
			"degraded", degraded, "verdict_degradation", degradationCause(degraded, verdictNotice),
			"charge_lost", s.chargeLost(debateID), "attribution", attribution)
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

// budgetExhaustedVerdict — детерминированный итог для режима moderator, когда
// бюджет исчерпан до вердикта. Сохраняет определение консенсуса, данное
// оплаченными итогами раундов, и намеренно не переносит в себя ни голоса, ни
// текст участников: в этом режиме исход определяет модератор, а вердикт,
// собранный из реплик, дал бы участникам способ вписать в протокол свой ответ.
func budgetExhaustedVerdict(consensus bool) (ModerationVerdict, string) {
	const answer = "Итог не сформулирован: бюджет модератора на эти дебаты исчерпан до вердикта."
	var sb strings.Builder
	sb.WriteString(answer)
	sb.WriteString("\n\n")
	if consensus {
		sb.WriteString("Консенсус зафиксирован промежуточным итогом раунда до исчерпания бюджета.\n")
	} else {
		sb.WriteString("Консенсус не был зафиксирован ни одним промежуточным итогом.\n")
	}
	sb.WriteString("Протокол дискуссии сохранён полностью — выводы можно сделать по нему.\n")
	// Пустые срезы, а не nil: эти поля уезжают в протокол и в экспорт, где все
	// остальные производители вердикта дают [], и потребитель не обязан
	// разбирать ещё и null.
	return ModerationVerdict{
		FinalAnswer:         answer,
		Claims:              []ModerationClaim{},
		UnresolvedQuestions: []string{},
		Decisions:           []string{},
		Consensus:           consensus,
	}, sb.String()
}

// degradationCause — причина, по которой вердикт подведён не моделью, для лога.
// Отдельный ключ, потому что `degraded` в той же строке значит именно «сработал
// потолок расхода»: по нему считается критерий отката 1 из
// docs/adr/0004-moderator-spend-ceiling.md, и расширение его смысла до «модель не
// сработала» сделало бы этот счётчик в развёртывании без LLM-ключа всегда
// положительным.
//
// Говорит только о вердикте — отсюда имя ключа `verdict_degradation`. Дебаты,
// потерявшие резюме раундов, но получившие вердикт модели, дают здесь `none`, и
// искать по этому ключу все деградировавшие дебаты нельзя.
func degradationCause(budgetExhausted bool, verdictNotice string) string {
	switch {
	case budgetExhausted:
		return "budget"
	case verdictNotice != "":
		return "unavailable"
	default:
		return "none"
	}
}

// storedVerdictDegradation восстанавливает причину, по которой итог подвела не
// модель, для вердикта, записанного прошлым проходом модерации.
//
// Нужна потому, что итоговая строка расхода пишется после успешного перехода в
// concluded: проход, записавший вердикт и упавший на переходе, не пишет её
// вовсе, и всё, что оператор увидит, — строка повтора. Повтор же берёт готовый
// вердикт и сам ничего не деградирует, поэтому без восстановления он отчитался
// бы «решила модель» о вердикте, которого модель не выносила, и критерий отката
// 1 из docs/adr/0004-moderator-spend-ceiling.md недосчитался бы сработавшего
// потолка.
//
// Источник истины — протокол: у деградировавшего вердикта спикер «система», а
// рядом лежит уведомление, называющее причину. Проверка спикера обязательна:
// уведомление о недоступности пишется и там, где вердикта не будет вовсе, и
// следующий проход может добавить к нему вердикт модели.
func storedVerdictDegradation(
	msgs []Message,
	round int,
	stored Message,
	budgetNotice, unavailableNotice string,
) (budgetExhausted bool, notice string) {
	if stored.SpeakerName != SystemSpeakerName {
		return false, ""
	}
	switch {
	case noticeRecorded(msgs, round, budgetNotice):
		return true, budgetNotice
	case noticeRecorded(msgs, round, unavailableNotice):
		return false, unavailableNotice
	default:
		return false, ""
	}
}

// SystemSpeakerName — имя говорящего у записей, которые делает сам сервис:
// пропуск хода, уведомление о деградации, детерминированный вердикт. Константа,
// а не литерал по месту, потому что по этому имени читается вопрос «вердикт
// вынесла модель или сервис» (см. storedVerdictDegradation).
//
// Экспортируется, чтобы обвязка могла отказать модератору с таким именем: имя
// модератора задаёт оператор, и совпадение стёрло бы это различие и в логике, и
// в протоколе.
const SystemSpeakerName = "система"

// committedDegradation — уведомление о деградации, которое раунд уже записал в
// протокол, или пустая строка.
//
// Уведомление — обещание потребителю: SPEC.md публикует для каждого из них, что
// именно потеряно и следует ли за ним запись. Проход, повторяющий раунд после
// сбоя записи, обязан это обещание выполнить, даже если модель к тому моменту
// снова отвечает. Иначе за «модератор недоступен, итог подведён по голосам»
// встаёт вердикт модели, а за «в этом раунде нет резюме» — резюме, и артефакт
// говорит о раунде две разные вещи. Ни одно правило conformance этого не ловит:
// оба утверждения по отдельности допустимы.
//
// До появления фонового повтора такое расхождение требовало рестарта между двумя
// проходами; с ним это обычный путь того самого сбоя, ради которого повтор и
// добавлен.
func committedDegradation(msgs []Message, round int, budgetNotice, unavailableNotice string) string {
	switch {
	case noticeRecorded(msgs, round, budgetNotice):
		return budgetNotice
	case noticeRecorded(msgs, round, unavailableNotice):
		return unavailableNotice
	default:
		return ""
	}
}

// noticeRecorded сообщает, есть ли уже в протоколе это уведомление о деградации
// за этот раунд. Повторный проход модерации — возобновлённый после сбоя записи
// или после рестарта — не должен дублировать запись, на которой держится
// читаемость деградации.
func noticeRecorded(msgs []Message, round int, notice string) bool {
	for _, message := range msgs {
		if message.Round == round && message.Kind == KindSystem && message.Text == notice {
			return true
		}
	}
	return false
}

// estimateModerationTokens — верхняя граница расхода одного вызова модератора,
// вычисляемая до вызова из того, что сервис уже держит в руках.
//
// Считается один токен на байт. Это не средний расход, а именно верхняя
// граница: byte-level BPE (Claude, cl100k/o200k) начинает с побайтового
// разбиения и только склеивает пары, поэтому число токенов не превышает числа
// байт ни на каком входе. Делить байты на средний коэффициент нельзя — реплики
// пишет недоверенная сторона, и подобрать текст, который токенизируется хуже
// среднего, ничего не стоит.
//
// Обычный текст расходует заметно меньше границы (кириллица в UTF-8 — около
// пяти байт на токен, латиница около четырёх), поэтому потолок нужно выбирать с
// запасом на этот коэффициент: см. docs/adr/0004-moderator-spend-ceiling.md.
func (s *Service) estimateModerationTokens(question, transcript string) int {
	return len(question) + len(transcript) + ModerationPromptOverheadBytes + s.budget.OutputPerCall
}

// MinimumViableBudget — потолок, ниже которого модерация не состоится вообще:
// оценка любого вызова включает запас на промпт и на ответ модели, поэтому
// меньший бюджет отвергает первый же вызов в каждых дебатах. Такой бюджет — это
// отключение модерации под видом ограничения расхода, и обвязка обязана
// отличать одно от другого.
func (b ModeratorBudget) MinimumViableBudget() int {
	return ModerationPromptOverheadBytes + b.OutputPerCall
}

// summaryCall — вызов промежуточного резюме с собственным дедлайном. Отдельная
// функция потому, что вызов стоит в инициализаторе if и отменить контекст на
// месте иначе некуда.
func (s *Service) summaryCall(
	ctx context.Context,
	d Debate,
	transcript string,
	msgs []Message,
) (RoundSummary, ModerationUsage, error) {
	callCtx, cancelCall := moderationCall(ctx)
	defer cancelCall()
	return s.moderator.Summary(callCtx, subject(d), transcript, d.CurrentRound, messageSeqs(msgs))
}

// moderationCall даёт одному вызову модератора собственный дедлайн.
func moderationCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, moderationTimeout)
}

// moderationAdmission — вердикт по вопросу «звать ли модель», и если нет, то по
// какой причине. Причин две, и путать их нельзя: в протоколе они публикуются как
// разные (SPEC.md, колонка Cause), и по одной из них считается критерий отката 1
// из docs/adr/0004-moderator-spend-ceiling.md.
type moderationAdmission int

const (
	// admissionAllowed — вызов разрешён и учтён как платный.
	admissionAllowed moderationAdmission = iota
	// admissionBudgetExhausted — на вызов не хватает бюджета этих дебатов.
	admissionBudgetExhausted
	// admissionUnaffordable — платить нельзя по причине, не связанной с бюджетом
	// дебатов: сломан учёт расхода или исчерпан потолок платных повторов раунда.
	// Для протокола это «модератор недоступен», а не «бюджет исчерпан»: бюджет
	// здесь не тратился и может быть почти нетронут, а утверждать обратное значит
	// публиковать ложную причину.
	admissionUnaffordable
)

// admitModeration решает судьбу следующего вызова модели и, разрешая его,
// сразу засчитывает как платный.
//
// Счёт ведётся на допуске, а не на списании: вызов, ушедший к провайдеру и не
// вернувший ответ, списания не даёт (ADR 0004 считает его неоплаченным), но
// провайдер мог его выполнить, и повтор, считающий только списания, повторял бы
// такой вызов вечно.
//
// Проверка до вызова, а не после: потолок обязан предотвращать расход, а не
// обнаруживать его.
func (s *Service) admitModeration(d Debate, question, transcript string) moderationAdmission {
	// Ограничения повторов спрашивают первыми: они действуют и при снятом
	// потолке, потому что ограничивают не эти дебаты, а цену собственной живости.
	if reason, affordable := s.affordModelCall(d.ID); !affordable {
		s.log.Warn("модерация: платный вызов модели невозможен",
			"debate", d.ID, "round", d.CurrentRound, "reason", reason)
		return admissionUnaffordable
	}
	if s.budget.DebateTokens > 0 &&
		d.ModeratorTokens+s.estimateModerationTokens(question, transcript) > s.budget.DebateTokens {
		return admissionBudgetExhausted
	}
	s.noteModerationAdmitted(d.ID)
	return admissionAllowed
}

// chargeModeration списывает расход вызова с бюджета дебатов.
//
//   - вызов не вошёл в счёт — списывать нечего;
//   - вошёл и расход сообщён — списывается фактический;
//   - вошёл, а расхода в отчёте нет (провайдер его не вернул или мы перестали
//     ждать ответ) — списывается верхняя граница. Считать такой вызов
//     бесплатным нельзя: провайдер, не возвращающий usage, снимал бы потолок
//     целиком, а медленный провайдер выдавал бы неучтённые вызовы.
func (s *Service) chargeModeration(d *Debate, question, transcript string, spent ModerationUsage) {
	if !spent.Billed {
		return
	}
	charge := spent.Total()
	if !spent.Reported() {
		charge = s.estimateModerationTokens(question, transcript)
	}
	if charge <= 0 {
		return
	}
	// Инкремент в хранилище, а не запись d: расход обязан переживать рестарт,
	// иначе перезапуск машины выдавал бы дебатам свежий бюджет.
	if err := s.store.AddModeratorTokens(d.ID, charge); err != nil {
		// Деньги потрачены, а записать это не вышло. Следующий проход перечитает
		// дебаты и увидит прежний остаток бюджета, поэтому дальше платить нельзя:
		// отказ хранилища, роняющий и запись результата, и учёт, иначе выдавал бы
		// каждому повтору полный бюджет заново — критерий отката 2 из
		// docs/adr/0004-moderator-spend-ceiling.md на ровном месте. Проходы при
		// этом продолжаются: они станут бесплатными и доведут дебаты до
		// детерминированного итога, как при исчерпанном бюджете.
		s.log.Error("учёт расхода модератора", "debate", d.ID, "err", err)
		s.noteChargeLost(d.ID)
	} else {
		d.ModeratorTokens += charge
	}
	s.log.Info("расход модератора", "debate", d.ID, "round", d.CurrentRound,
		"tokens", charge, "reported", spent.Reported(),
		"debate_total", d.ModeratorTokens, "budget", s.budget.DebateTokens)
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
		s.giveUpOnModeration(d.ID)
		return
	}

	if storedVerdict == nil && !lastRound && !consensus {
		// Промежуточное резюме — опциональный слой: без LLM дебаты едут дальше.
		// Но «едут дальше» не значит «молча»: пропуск записывается, иначе
		// развёртывание без ключа отдаёт протокол без единого резюме и без
		// объяснения почему.
		if storedSummary == nil {
			transcript := renderTranscriptText(msgs)
			if committed := committedDegradation(msgs, d.CurrentRound,
				NoticeBudgetSummary, NoticeUnavailableSummary); committed != "" {
				s.log.Info("гибрид: раунд уже объявил пропуск резюме",
					"debate", d.ID, "round", d.CurrentRound)
			} else if admission := s.admitModeration(d, subject(d), transcript); admission != admissionAllowed {
				notice := NoticeBudgetSummary
				if admission == admissionUnaffordable {
					notice = NoticeUnavailableSummary
				} else {
					s.log.Warn("гибрид: бюджет дебатов исчерпан, резюме пропущено",
						"debate", d.ID, "round", d.CurrentRound, "spent", d.ModeratorTokens)
				}
				if !noticeRecorded(msgs, d.CurrentRound, notice) {
					s.lock()
					_, _ = s.appendMessage(d.ID, d.CurrentRound, "", SystemSpeakerName, KindSystem, notice)
					s.unlock()
				}
			} else if summary, spent, err := s.summaryCall(ctx, d, transcript, msgs); err != nil {
				s.chargeModeration(&d, subject(d), transcript, spent)
				s.log.Warn("гибрид: резюме раунда недоступно", "debate", d.ID, "err", err)
				if !noticeRecorded(msgs, d.CurrentRound, NoticeUnavailableSummary) {
					s.lock()
					_, _ = s.appendMessage(d.ID, d.CurrentRound, "", SystemSpeakerName, KindSystem, NoticeUnavailableSummary)
					s.unlock()
				}
			} else {
				s.chargeModeration(&d, subject(d), transcript, spent)
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
	// Уведомление, которое обязано предшествовать вердикту, если его подвела не
	// модель. Пустая строка — вердикт модели, объяснять нечего. Одна переменная
	// на обе причины: читателю протокола нужна причина, а не только тот факт,
	// что говорит «система».
	verdictNotice := ""
	// budgetExhausted — отдельный флаг, а не производная от verdictNotice: по нему
	// считается критерий отката 1 из docs/adr/0004-moderator-spend-ceiling.md, и
	// он обязан значить «сработал потолок расхода», а не «вердикт подвела не
	// модель». Иначе развёртывание без ключа отчиталось бы о сработавшем потолке
	// на каждых дебатах.
	budgetExhausted := false
	transcript := renderTranscriptText(msgs)
	committed := committedDegradation(msgs, d.CurrentRound,
		NoticeBudgetVerdictHybrid, NoticeUnavailableVerdictHybrid)
	switch {
	case storedVerdict != nil:
		verdict = *storedVerdict.Verdict
		verdictText = storedVerdict.Text
		speaker = storedVerdict.SpeakerName
	case committed != "":
		// Раунд уже объявил причину: итог обязан ей соответствовать.
		budgetExhausted = committed == NoticeBudgetVerdictHybrid
		verdictNotice = committed
		verdict, verdictText = hybridVerdict(votes, msgs, consensus, !budgetExhausted)
		speaker = SystemSpeakerName
	default:
		switch admission := s.admitModeration(d, subject(d), transcript); admission {
		case admissionBudgetExhausted, admissionUnaffordable:
			// Платного вызова не будет. Причина в протоколе — та, что верна: потолок
			// расхода этих дебатов или недоступность модели по любой другой причине.
			// Цитата лидера голосования уместна во втором случае и неуместна в
			// первом. Правило ADR 0004 — цитировать нельзя тогда, когда триггер мог
			// выбрать участник: исчерпать бюджет длинными репликами может любой, кто
			// вошёл в дебаты. Отказ платить участник так выбрать не может — для него
			// нужно сломать запись в хранилище, то есть уронить сервис целиком, — и
			// потому он стоит рядом с отказом провайдера, а не рядом с бюджетом.
			budgetExhausted = admission == admissionBudgetExhausted
			if budgetExhausted {
				s.log.Warn("гибрид: бюджет дебатов исчерпан, вердикт по голосам",
					"debate", d.ID, "spent", d.ModeratorTokens, "budget", s.budget.DebateTokens)
				verdictNotice = NoticeBudgetVerdictHybrid
			} else {
				verdictNotice = NoticeUnavailableVerdictHybrid
			}
			verdict, verdictText = hybridVerdict(votes, msgs, consensus, !budgetExhausted)
			speaker = SystemSpeakerName
		default:
			var spent ModerationUsage
			callCtx, cancelCall := moderationCall(ctx)
			verdict, spent, err = s.moderator.Verdict(callCtx, subject(d), transcript, messageSeqs(msgs))
			cancelCall()
			s.chargeModeration(&d, subject(d), transcript, spent)
			verdictText = verdict.Text()
			if err != nil {
				s.log.Warn("гибрид: LLM-вердикт недоступен, использую подсчёт голосов", "debate", d.ID, "err", err)
				verdictNotice = NoticeUnavailableVerdictHybrid
				verdict, verdictText = hybridVerdict(votes, msgs, consensus, true)
				speaker = SystemSpeakerName
			}
		}
	}
	// В hybrid исход консенсуса определяют только голоса участников. Модель
	// формулирует решение, но не может переопределить этот протокольный факт.
	verdict.Consensus = consensus
	s.lock()
	defer s.unlock()
	if storedVerdict == nil {
		if verdictNotice != "" && !noticeRecorded(msgs, d.CurrentRound, verdictNotice) {
			if _, err := s.appendMessage(d.ID, d.CurrentRound, "", SystemSpeakerName, KindSystem, verdictNotice); err != nil {
				s.log.Error("гибрид: сохранение уведомления о деградации", "debate", d.ID, "err", err)
				return
			}
		}
		if _, err := s.appendVerdictText(d.ID, d.CurrentRound, speaker, verdict, verdictText); err != nil {
			s.log.Error("гибрид: сохранение вердикта", "debate", d.ID, "err", err)
			return
		}
	}
	// attribution — см. одноимённую переменную в moderate.
	attribution := "observed"
	switch {
	case storedVerdict != nil:
		attribution = "recovered"
		budgetExhausted, verdictNotice = storedVerdictDegradation(msgs, d.CurrentRound, *storedVerdict,
			NoticeBudgetVerdictHybrid, NoticeUnavailableVerdictHybrid)
	case committed != "":
		attribution = "recovered"
	}
	d.Status = StatusConcluded
	d.Consensus = consensus
	d.TurnAgentID = ""
	d.TurnDeadline = time.Time{}
	if err := s.store.UpdateDebate(d); err != nil {
		s.log.Error("гибрид: сохранение статуса", "debate", d.ID, "err", err)
		return
	}
	// Итог расхода — после успешного завершения: см. тот же комментарий в moderate.
	s.log.Info("расход модератора за дебаты", "debate", d.ID,
		"tokens", d.ModeratorTokens, "budget", s.budget.DebateTokens,
		"degraded", budgetExhausted, "verdict_degradation", degradationCause(budgetExhausted, verdictNotice),
		"charge_lost", s.chargeLost(d.ID), "attribution", attribution)
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
// hybridVerdict собирает детерминированный итог по голосам участников.
//
// quoteLeader управляет тем, попадает ли в итог дословная реплика лидера
// голосования. При недоступности модератора — да: это лучший доступный ответ на
// вопрос дебатов, и триггер выбрал не участник. При исчерпании бюджета — нет:
// исчерпать бюджет может любой, кто вошёл в дебаты и пишет длинные реплики, а
// это дало бы участнику способ гарантированно вписать свой текст в итог чужой
// дискуссии (docs/adr/0004-moderator-spend-ceiling.md).
func hybridVerdict(votes []Vote, msgs []Message, consensus, quoteLeader bool) (ModerationVerdict, string) {
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
		switch {
		case text != "" && quoteLeader:
			fmt.Fprintf(&sb, "\nНаибольшую поддержку получила позиция участника %s (голосов: %d).\nЕго итоговая реплика:\n\n%s\n", name, bestCount, text)
		case text != "":
			// Идентификатор, а не отображаемое имя: имя задаёт участник, а
			// исчерпать бюджет может любой из них, и через имя в итог чужой
			// дискуссии уехал бы произвольный текст.
			fmt.Fprintf(&sb, "\nНаибольшую поддержку получила позиция участника %s (голосов: %d).\nЕё изложение — в протоколе дискуссии.\n", best, bestCount)
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

// ExportSnapshot — согласованный срез дебатов: состояние в том же виде, в каком
// его отдаёт публичное чтение, метаданные агентов-участников и полный протокол.
// Тип принадлежит ядру и ничего не знает о форматах: сериализацию делает
// internal/protocol.
type ExportSnapshot struct {
	Debate       DebateView
	Participants []Agent
	Messages     []Message
}

// ExportSnapshot читает дебаты целиком под замком переходов состояния.
//
// Замок здесь не про свежесть, а про согласованность: без него срез склеивает
// состояние до хода с протоколом после него и публикует артефакт, которого не
// существовало ни в один момент — и ничем об этом не сообщает. Замок никогда не
// удерживается на время вызова модератора, поэтому ожидание ограничено одной
// секцией записи в хранилище (docs/adr/0006-debate-export-endpoint.md).
//
// Ожидание замка отменяемо, и это обязательно: чтение вызывается с публичной
// неаутентифицированной границы, и запрос, чей клиент уже отключился, обязан
// покинуть очередь, а не занимать её и не делать работу в пустоту. Протокол
// читается ровно один раз и он же считает голоса: второе чтение внутри view
// молча роняет голоса при сбое, а экспорт без голосов неотличим от дебатов,
// где никто не голосовал.
func (s *Service) ExportSnapshot(ctx context.Context, debateID string) (ExportSnapshot, error) {
	if err := s.lockContext(ctx); err != nil {
		return ExportSnapshot{}, err
	}
	defer s.unlock()
	d, err := s.store.GetDebate(debateID)
	if err != nil {
		return ExportSnapshot{}, err
	}
	messages, err := s.store.Messages(debateID, 0)
	if err != nil {
		return ExportSnapshot{}, err
	}
	// Именно представление, а не строка хранилища: оно скрывает description до
	// старта дебатов, и экспорт обязан скрывать ровно то же самое.
	view, err := s.viewFrom(d, messages)
	if err != nil {
		return ExportSnapshot{}, err
	}
	agents := make([]Agent, 0, len(view.Participants))
	for _, p := range view.Participants {
		agent, err := s.store.AgentByID(p.AgentID)
		if err != nil {
			// Ошибка намеренно не оборачивается: участник без агента — это
			// расхождение внутри хранилища, а обёртка превратила бы его в
			// «дебаты не найдены» на границе HTTP.
			return ExportSnapshot{}, fmt.Errorf("метаданные участника %s недоступны: %v", p.AgentID, err)
		}
		agents = append(agents, agent)
	}
	return ExportSnapshot{Debate: view, Participants: agents, Messages: messages}, nil
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

// view собирает публичное представление, читая протокол сам. Ошибка чтения
// здесь намеренно не рвёт ответ: голоса — производная величина, а состояние и
// участники уже прочитаны. Экспорт этим путём не идёт: там пропавшие голоса
// неотличимы от дебатов, где никто не голосовал, поэтому он передаёт
// авторитетный протокол в viewFrom.
func (s *Service) view(d Debate) (DebateView, error) {
	var transcript []Message
	if d.Mode == ModeHybrid && d.Status != StatusOpen {
		transcript, _ = s.store.Messages(d.ID, 0)
	}
	return s.viewFrom(d, transcript)
}

func (s *Service) viewFrom(d Debate, transcript []Message) (DebateView, error) {
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
		v.Votes = currentVotes(parts, transcript)
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
