package core_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"court/internal/core"
	"court/internal/store"
)

// Потолок расхода LLM-модератора на одни дебаты
// (docs/adr/0004-moderator-spend-ceiling.md).
//
// Инвариант, который здесь охраняется: стоимость одних дебатов конечна и
// известна заранее. Дебаты после старта едут сами на дедлайнах ходов, инициатору
// не нужен ни один дальнейший запрос, поэтому лимиты на частоту запросов эту
// стоимость не ограничивают.

// spendingModerator сообщает заданный расход на каждый вызов и считает вызовы.
// Billed=true с нулевыми токенами означает «вызов оплачен, расход не сообщён»;
// Billed=false — «вызов в счёт не вошёл».
type spendingModerator struct {
	mu        sync.Mutex
	usage     core.ModerationUsage
	err       error
	consensus bool
	checks    int
	verdicts  int
}

func (*spendingModerator) Name() string { return "spending moderator" }

func (m *spendingModerator) CheckRound(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	m.mu.Lock()
	m.checks++
	m.mu.Unlock()
	if m.err != nil {
		return core.RoundSummary{}, m.usage, m.err
	}
	if m.consensus {
		return core.RoundSummary{
			Summary:   "Участники согласовали решение.",
			Decisions: []string{"Принять предложение."},
			Consensus: true,
		}, m.usage, nil
	}
	return core.RoundSummary{
		Summary:             "Раунд подытожен.",
		UnresolvedQuestions: []string{"Вопрос остаётся открытым."},
	}, m.usage, nil
}

func (m *spendingModerator) Summary(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	m.mu.Lock()
	m.checks++
	m.mu.Unlock()
	return core.RoundSummary{Summary: "Резюме раунда."}, m.usage, m.err
}

func (m *spendingModerator) Verdict(
	context.Context, string, string, []int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	m.mu.Lock()
	m.verdicts++
	m.mu.Unlock()
	if m.err != nil {
		return core.ModerationVerdict{}, m.usage, m.err
	}
	return core.ModerationVerdict{FinalAnswer: "Итог модели."}, m.usage, nil
}

func (m *spendingModerator) calls() (checks, verdicts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checks, m.verdicts
}

func TestModeratorSpendCeilingDegradesToDeterministicVerdict(t *testing.T) {
	moderator := &spendingModerator{}
	// Потолок в один токен исчерпан любой оценкой: ни один вызов не допускается.
	service := newTestServiceWithOptions(t, moderator,
		core.WithModeratorBudget(core.ModeratorBudget{DebateTokens: 1, OutputPerCall: 4096}))
	debateID, agents := startedDebate(t, service, core.ModeModerator, 1)

	// Оба участника голосуют за создателя: в режиме moderator подсчёт голосов не
	// имеет права стать механизмом консенсуса, и единогласие не должно ни на что
	// повлиять — иначе исход чужих дебатов решают те, кто в них вошёл.
	playRound(t, service, debateID, agents, agents[0].ID)

	concluded := waitForStatus(t, service, debateID, core.StatusConcluded)
	if checks, verdicts := moderator.calls(); checks != 0 || verdicts != 0 {
		t.Fatalf("вызовов модератора при исчерпанном бюджете: checks=%d verdicts=%d, ожидалось 0/0",
			checks, verdicts)
	}
	if concluded.Consensus {
		t.Fatal("единогласие участников не может дать консенсус в режиме moderator")
	}

	messages, err := service.Messages(debateID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	var verdicts, notices int
	for _, message := range messages {
		switch {
		case message.Kind == core.KindVerdict:
			verdicts++
			// Читатель протокола обязан видеть, что итог подведён не моделью.
			if message.SpeakerName != "система" {
				t.Fatalf("вердикт при деградации подписан %q, ожидалось «система»", message.SpeakerName)
			}
			if !strings.Contains(message.Text, "бюджет модератора") &&
				!strings.Contains(message.Text, "Бюджет модератора") {
				t.Fatalf("вердикт при деградации не называет причину: %s", message.Text)
			}
			// Вердикт не имеет права переносить в себя текст участников.
			for _, agent := range agents {
				if strings.Contains(message.Text, "позиция "+agent.Name) {
					t.Fatalf("вердикт содержит реплику участника %s: %s", agent.Name, message.Text)
				}
			}
		case message.Kind == core.KindSystem && strings.Contains(message.Text, "Бюджет модератора"):
			notices++
		}
	}
	if verdicts != 1 {
		t.Fatalf("вердиктов в протоколе: %d, ожидался 1", verdicts)
	}
	if notices != 1 {
		t.Fatalf("уведомлений об исчерпанном бюджете: %d, ожидалось 1", notices)
	}
}

func TestModeratorSpendCeilingKeepsConsensusFoundBeforeExhaustion(t *testing.T) {
	// Итог раунда, за который уже заплатили, зафиксировал консенсус. Деградация
	// вердикта по бюджету не имеет права его отменить: иначе протокол
	// противоречит сам себе — резюме говорит «согласовано», вердикт «нет».
	moderator := &spendingModerator{
		usage:     core.ModerationUsage{Billed: true, InputTokens: 9_000},
		consensus: true,
	}
	service := newTestServiceWithOptions(t, moderator,
		core.WithModeratorBudget(core.ModeratorBudget{DebateTokens: 10_000}))
	debateID, agents := startedDebate(t, service, core.ModeModerator, 3)

	playRound(t, service, debateID, agents, "")

	concluded := waitForStatus(t, service, debateID, core.StatusConcluded)
	if checks, verdicts := moderator.calls(); checks != 1 || verdicts != 0 {
		t.Fatalf("вызовов модератора: checks=%d verdicts=%d, ожидалось 1/0", checks, verdicts)
	}
	if !concluded.Consensus {
		t.Fatal("консенсус, найденный оплаченным итогом раунда, потерян при деградации")
	}
}

func TestModeratorSpendCeilingSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "court.db")
	budget := core.ModeratorBudget{DebateTokens: 10_000, OutputPerCall: 0}

	// Первый процесс: один вызов модератора съедает весь бюджет дебатов.
	first := &spendingModerator{usage: core.ModerationUsage{Billed: true, InputTokens: 10_000}}
	service, closeStore := serviceAt(t, path, first, core.WithModeratorBudget(budget))
	debateID, agents := startedDebate(t, service, core.ModeModerator, 2)
	playRound(t, service, debateID, agents, "")
	waitForRound(t, service, debateID, core.StatusRunning, 2)
	if checks, _ := first.calls(); checks != 1 {
		t.Fatalf("вызовов итога раунда до рестарта: %d, ожидался 1", checks)
	}
	closeStore()

	// Рестарт: новый процесс поверх той же базы. Накопленный расход обязан
	// приехать из хранилища, иначе перезапуск машины выдаёт свежий бюджет.
	second := &spendingModerator{usage: core.ModerationUsage{Billed: true, InputTokens: 10_000}}
	restarted, closeRestarted := serviceAt(t, path, second, core.WithModeratorBudget(budget))
	defer closeRestarted()
	debate, err := restarted.GetDebate(debateID)
	if err != nil {
		t.Fatalf("GetDebate после рестарта: %v", err)
	}
	if debate.CurrentRound != 2 || debate.Status != core.StatusRunning {
		t.Fatalf("состояние после рестарта: %s раунд %d", debate.Status, debate.CurrentRound)
	}

	playRound(t, restarted, debateID, agents, "")
	waitForStatus(t, restarted, debateID, core.StatusConcluded)
	if checks, verdicts := second.calls(); checks != 0 || verdicts != 0 {
		t.Fatalf("после рестарта модератор вызван: checks=%d verdicts=%d — бюджет сбросился",
			checks, verdicts)
	}
}

func TestModeratorSpendCeilingChargesUnreportedUsageAsEstimate(t *testing.T) {
	// Провайдер не сообщает usage. Если считать такой вызов бесплатным, потолок
	// снимается целиком, поэтому сервис списывает собственную оценку.
	moderator := &spendingModerator{usage: core.ModerationUsage{Billed: true}}
	budget := core.ModeratorBudget{DebateTokens: 6_000}
	service := newTestServiceWithOptions(t, moderator, core.WithModeratorBudget(budget))
	debateID, agents := startedDebate(t, service, core.ModeModerator, 3)

	playRound(t, service, debateID, agents, "")
	waitForRound(t, service, debateID, core.StatusRunning, 2)
	if checks, _ := moderator.calls(); checks != 1 {
		t.Fatalf("вызовов в первом раунде: %d, ожидался 1", checks)
	}

	// Второй вызов уже не укладывается в остаток: списанная за первый оценка
	// плюс оценка второго превышают потолок.
	playRound(t, service, debateID, agents, "")
	waitForRound(t, service, debateID, core.StatusRunning, 3)
	if checks, _ := moderator.calls(); checks != 1 {
		t.Fatalf("вызовов после списания неизвестного расхода: %d, ожидался 1", checks)
	}

	playRound(t, service, debateID, agents, "")
	waitForStatus(t, service, debateID, core.StatusConcluded)
	if _, verdicts := moderator.calls(); verdicts != 0 {
		t.Fatalf("вердикт запрошен у модели при исчерпанном бюджете (%d вызовов)", verdicts)
	}
}

func TestModeratorSpendCeilingChargesFailedCalls(t *testing.T) {
	// Неудачный вызов оплачен владельцем ключа так же, как удачный. Если его не
	// списывать, провайдер, стабильно возвращающий мусор, тратит бюджет вечно.
	moderator := &spendingModerator{
		usage: core.ModerationUsage{Billed: true, InputTokens: 3_000, OutputTokens: 1_000},
		err:   errNoStructuredResult,
	}
	// Потолка хватает на один вызов: второй отсекается только потому, что
	// неудачный первый был списан.
	budget := core.ModeratorBudget{DebateTokens: 8_000}
	service := newTestServiceWithOptions(t, moderator, core.WithModeratorBudget(budget))
	debateID, agents := startedDebate(t, service, core.ModeModerator, 3)

	playRound(t, service, debateID, agents, "")
	waitForRound(t, service, debateID, core.StatusRunning, 2)
	if checks, _ := moderator.calls(); checks != 1 {
		t.Fatalf("вызовов в первом раунде: %d, ожидался 1", checks)
	}

	playRound(t, service, debateID, agents, "")
	waitForRound(t, service, debateID, core.StatusRunning, 3)
	if checks, _ := moderator.calls(); checks != 1 {
		t.Fatalf("вызовов после неудачного, но оплаченного вызова: %d, ожидался 1", checks)
	}
}

func TestModeratorSpendCeilingDoesNotChargeUnbilledCalls(t *testing.T) {
	// Запрос, не дошедший до провайдера, в счёт не вошёл. Списывать за него
	// оценку нельзя: иначе недоступность провайдера исчерпывает бюджет дебатов,
	// не потративших ничего, и в протокол попадает ложное «бюджет исчерпан».
	// Отмену с нашей стороны провайдеры помечают оплаченной — это отдельный
	// случай, покрытый TestAnthropicCallToolChargesOurOwnCancellation.
	moderator := &spendingModerator{
		usage: core.ModerationUsage{Billed: false},
		err:   errProviderUnreachable,
	}
	budget := core.ModeratorBudget{DebateTokens: 8_000}
	service := newTestServiceWithOptions(t, moderator, core.WithModeratorBudget(budget))
	debateID, agents := startedDebate(t, service, core.ModeModerator, 3)

	playRound(t, service, debateID, agents, "")
	waitForRound(t, service, debateID, core.StatusRunning, 2)
	playRound(t, service, debateID, agents, "")
	waitForRound(t, service, debateID, core.StatusRunning, 3)

	// Оба итога раунда были запрошены: бюджет не тратился на вызовы без ответа.
	if checks, _ := moderator.calls(); checks != 2 {
		t.Fatalf("вызовов итога раунда: %d, ожидалось 2 — недоступный провайдер списал бюджет", checks)
	}
	playRound(t, service, debateID, agents, "")
	waitForStatus(t, service, debateID, core.StatusConcluded)
	if _, verdicts := moderator.calls(); verdicts != 1 {
		t.Fatalf("вердикт запрошен %d раз, ожидался 1", verdicts)
	}

	messages, err := service.Messages(debateID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	for _, message := range messages {
		if message.Kind == core.KindSystem && strings.Contains(message.Text, "Бюджет модератора") {
			t.Fatalf("протокол сообщает об исчерпанном бюджете, хотя расхода не было: %s", message.Text)
		}
	}
}

// serviceAt собирает сервис поверх базы по заданному пути: рестарт процесса
// имитируется вторым сервисом над той же базой.
func serviceAt(
	t *testing.T,
	path string,
	moderator core.Moderator,
	options ...core.ServiceOption,
) (*core.Service, func()) {
	t.Helper()
	database, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	closed := false
	closeStore := func() {
		if closed {
			return
		}
		closed = true
		if err := database.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	}
	t.Cleanup(closeStore)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return core.NewService(database, core.NewHub(), moderator, logger, options...), closeStore
}

// startedDebate создаёт дебаты с двумя участниками и доводит их до первого хода.
func startedDebate(
	t *testing.T,
	service *core.Service,
	mode core.DebateMode,
	rounds int,
) (string, []core.Agent) {
	t.Helper()
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question:       "Какой подход выбрать?",
		Mode:           mode,
		Rounds:         rounds,
		TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "возражаю"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	return created.ID, []core.Agent{creator, challenger}
}

// playRound проводит один полный раунд: каждый участник отвечает в свой ход.
func playRound(t *testing.T, service *core.Service, debateID string, agents []core.Agent, support string) {
	t.Helper()
	for _, agent := range agents {
		waitForTurn(t, service, debateID, agent.ID)
		if _, err := service.PostArgument(context.Background(), agent, debateID, "позиция "+agent.Name, support); err != nil {
			t.Fatalf("PostArgument(%s): %v", agent.Name, err)
		}
	}
}

// errNoStructuredResult — типичная ошибка модератора: ответ получен и оплачен,
// но не прошёл валидацию структуры.
var errNoStructuredResult = errors.New("структурированное резюме: обязательное поле отсутствует")

// errProviderUnreachable — ошибка до ответа: запрос не был выполнен и не оплачен.
var errProviderUnreachable = errors.New("anthropic tool call: context deadline exceeded")

// TestModeratorSpendCeilingRecordsOneNoticePerRoundAcrossRetries охраняет
// читаемость деградации. Уведомление о бюджете пишется до вердикта, поэтому
// сбой записи вердикта оставляет уведомление в протоколе; повторный проход
// модерации того же раунда не имеет права записать его второй раз.
//
// Сервис здесь один и не перезапускается: повтор — свойство работающего
// процесса, а не рестарта (issue #40).
func TestModeratorSpendCeilingRecordsOneNoticePerRoundAcrossRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "court.db")
	database, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	budget := core.WithModeratorBudget(core.ModeratorBudget{DebateTokens: 1})
	moderator := &spendingModerator{}

	failing := &verdictWriteFailure{Storage: database}
	service := core.NewService(failing, core.NewHub(), moderator, logger, budget,
		core.WithBackgroundTuningForTest(testTick, testRetryDelay, testPaidCap))
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go service.Run(runCtx)

	debateID, agents := startedDebate(t, service, core.ModeModerator, 1)
	playRound(t, service, debateID, agents, "")
	waitForStatus(t, service, debateID, core.StatusConcluded)

	if !failing.failedOnce() {
		t.Fatal("сбой записи вердикта не сработал: повторять было нечего")
	}
	if writes := failing.verdictWrites(); writes < 2 {
		t.Fatalf("попыток записи вердикта: %d, ожидался повтор после сбоя", writes)
	}
	messages, err := service.Messages(debateID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	notices, verdicts := 0, 0
	for _, message := range messages {
		switch {
		case message.Kind == core.KindSystem && strings.Contains(message.Text, "Бюджет модератора"):
			notices++
		case message.Kind == core.KindVerdict:
			verdicts++
		}
	}
	if notices != 1 {
		t.Fatalf("уведомлений о бюджете после повтора: %d, ожидалось 1", notices)
	}
	if verdicts != 1 {
		t.Fatalf("вердиктов после повтора: %d, ожидался 1", verdicts)
	}
}

// verdictWriteFailure роняет первую запись вердикта, оставляя дебаты в статусе
// moderating — состояние, из которого фоновый проход повторяет модерацию.
type verdictWriteFailure struct {
	core.Storage
	once   sync.Once
	mu     sync.Mutex
	failed bool
	writes int
}

func (s *verdictWriteFailure) AddMessage(message core.Message) (int64, error) {
	if message.Kind == core.KindVerdict {
		s.mu.Lock()
		s.writes++
		s.mu.Unlock()
		failed := false
		s.once.Do(func() { failed = true })
		if failed {
			s.mu.Lock()
			s.failed = true
			s.mu.Unlock()
			return 0, errors.New("injected verdict write failure")
		}
	}
	return s.Storage.AddMessage(message)
}

func (s *verdictWriteFailure) failedOnce() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// verdictWrites — сколько раз сервис пробовал записать вердикт. Больше одного
// означает, что повтор действительно случился.
func (s *verdictWriteFailure) verdictWrites() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}
