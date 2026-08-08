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
	"time"

	"court/internal/core"
	"court/internal/store"
)

// Фоновый проход в тестах: тик и пауза между попытками сжаты до миллисекунд,
// иначе тест ждал бы минуту ради одного повтора. Лимит попыток оставлен
// боевым — сжимать надо время, а не поведение.
const (
	testTick       = 5 * time.Millisecond
	testRetryDelay = time.Millisecond
	testPaidCap    = 10
)

// TestStuckModerationResumesWithoutRestart охраняет живость дебатов.
//
// Ветка модерации, не сумевшая записать результат, намеренно не завершает
// дебаты: протокол без записи, объясняющей исход, неотличим от усечённого.
// Значит дебаты остаются в статусе moderating, где ход не принадлежит никому и
// дедлайна нет, — участники в них не могут ничего. Пока повтор делался только
// при старте процесса, выйти оттуда можно было единственным способом:
// перезапустить сервер. Экспорт таких дебатов при этом валиден, поэтому ни одно
// правило conformance о них не сообщит (issue #40).
//
// Случаи перечисляют точки отказа, а не режимы: отказать может запись самого
// результата и переход состояния после неё, и восстановление у них разное —
// первый повтор переспрашивает модератора, второй обязан взять уже записанный
// результат и не запросить ничего.
func TestStuckModerationResumesWithoutRestart(t *testing.T) {
	for _, testCase := range []struct {
		name string
		mode core.DebateMode
		// rounds и playAfterResume вместе описывают, доигрываются ли дебаты после
		// возобновления или завершаются сразу на том же раунде.
		rounds          int
		playAfterResume int
		moderator       func() core.Moderator
		failing         func(core.Storage) stuckStorage
		wantSummaries   int
		wantVerdicts    int
		wantNotices     int
	}{
		{
			name: "moderator: падает запись вердикта",
			mode: core.ModeModerator, rounds: 1,
			moderator: func() core.Moderator { return &countingModerator{} },
			failing: func(storage core.Storage) stuckStorage {
				return &verdictWriteFailure{Storage: storage}
			},
			wantVerdicts: 1,
		},
		{
			name: "moderator: падает переход в concluded после записи вердикта",
			mode: core.ModeModerator, rounds: 1,
			moderator: func() core.Moderator { return &countingModerator{} },
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusConcluded, 1, storage)
			},
			wantVerdicts: 1,
		},
		{
			name: "hybrid: падает запись вердикта по голосам",
			mode: core.ModeHybrid, rounds: 1,
			moderator: func() core.Moderator { return unavailableModerator{} },
			failing: func(storage core.Storage) stuckStorage {
				return &verdictWriteFailure{Storage: storage}
			},
			wantVerdicts: 1, wantNotices: 1,
		},
		{
			name: "hybrid: падает переход к следующему раунду после записи резюме",
			mode: core.ModeHybrid, rounds: 2, playAfterResume: 1,
			moderator: func() core.Moderator { return &countingModerator{} },
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusRunning, 2, storage)
			},
			wantSummaries: 1, wantVerdicts: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := openTestStore(t)
			failing := testCase.failing(database)
			service := core.NewService(failing, core.NewHub(), testCase.moderator(),
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				core.WithBackgroundTuningForTest(testTick, testRetryDelay, testPaidCap))
			runCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go service.Run(runCtx)

			debateID, agents := startedDebate(t, service, testCase.mode, testCase.rounds)
			playRound(t, service, debateID, agents, "")
			for round := 0; round < testCase.playAfterResume; round++ {
				playRound(t, service, debateID, agents, "")
			}
			// Сервис здесь один и не перезапускался: дойти до concluded дебаты
			// могли только своим фоновым проходом.
			waitForStatus(t, service, debateID, core.StatusConcluded)

			if !failing.failedOnce() {
				t.Fatal("сбой записи не сработал: повторять было нечего, и тест ничего не проверил")
			}
			summaries, verdicts, notices := countModerationRecords(t, service, debateID)
			if summaries != testCase.wantSummaries {
				t.Fatalf("резюме в протоколе: %d, ожидалось %d", summaries, testCase.wantSummaries)
			}
			if verdicts != testCase.wantVerdicts {
				t.Fatalf("вердиктов в протоколе: %d, ожидалось %d", verdicts, testCase.wantVerdicts)
			}
			if notices != testCase.wantNotices {
				t.Fatalf("уведомлений о недоступности: %d, ожидалось %d", notices, testCase.wantNotices)
			}
		})
	}
}

// TestModerationRetryWaitsForTheModerationInFlight охраняет цену живости.
// Фоновый проход приходит каждые несколько секунд, а вызов модератора живёт до
// трёх минут: если повтор не отличает «зависло» от «ещё идёт», он запустит
// второй проход поверх первого — задвоит расход модератора, а при гонке записи
// и результат.
func TestModerationRetryWaitsForTheModerationInFlight(t *testing.T) {
	database := openTestStore(t)
	moderator := &heldModerator{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(moderator.unblock)
	service := core.NewService(database, core.NewHub(), moderator,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		core.WithBackgroundTuningForTest(testTick, testRetryDelay, testPaidCap))
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go service.Run(runCtx)

	debateID, agents := startedDebate(t, service, core.ModeModerator, 2)
	playRound(t, service, debateID, agents, "")
	select {
	case <-moderator.started:
	case <-time.After(2 * time.Second):
		t.Fatal("модерация первого раунда не началась")
	}
	// Достаточно тиков, чтобы неверный повтор успел случиться много раз.
	time.Sleep(20 * testTick)
	if calls := moderator.calls(); calls != 1 {
		t.Fatalf("вызовов модератора при живой модерации: %d, ожидался 1", calls)
	}

	moderator.unblock()
	waitForRound(t, service, debateID, core.StatusRunning, 2)
	if calls := moderator.calls(); calls != 1 {
		t.Fatalf("вызовов модератора после освобождения: %d, ожидался 1", calls)
	}
	summaries, _, _ := countModerationRecords(t, service, debateID)
	if summaries != 1 {
		t.Fatalf("резюме в протоколе: %d, ожидалось 1", summaries)
	}
}

// TestPaidRetriesAreCappedAndFreeOnesAreNot охраняет цену живости с обеих
// сторон.
//
// Повтор в режиме moderator переспрашивает модель, поэтому невосстановимый отказ
// записи мог бы выбрать весь бюджет дебатов на повторы: платные проходы одного
// раунда конечны. Но ограничивать надо деньги, а не живость — повтор, который
// модель не зовёт, не стоит ничего, и остановить его значит снова оставить
// дебаты ждать рестарта, теперь уже после любого отказа хранилища длиннее серии.
// Поэтому, упершись в потолок, раунд продолжает повторяться бесплатно и доезжает
// до конца.
//
// Режим hybrid потому, что в нём бесплатный проход всё ещё пишет вердикт по
// голосам: счёт попыток записи и есть свидетельство, что повторы продолжились.
func TestPaidRetriesAreCappedAndFreeOnesAreNot(t *testing.T) {
	const paidCap = 3
	database := openTestStore(t)
	// Записи вердикта падают, пока их не наберётся заведомо больше потолка
	// платных проходов; после этого хранилище выздоравливает.
	failing := &verdictWriteFailureTimes{Storage: database, failures: paidCap + 4}
	// Расход отличен от нуля: потолок считает допущенные платные вызовы.
	moderator := &countingModerator{usage: core.ModerationUsage{Billed: true, InputTokens: 100}}
	service := core.NewService(failing, core.NewHub(), moderator,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Потолок расхода снят: платные повторы ограничивает только этот счёт.
		core.WithBackgroundTuningForTest(testTick, testRetryDelay, paidCap))
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go service.Run(runCtx)

	debateID, agents := startedDebate(t, service, core.ModeHybrid, 1)
	playRound(t, service, debateID, agents, "")
	// Дебаты доезжают до конца, хотя отказ пережил потолок платных проходов.
	waitForStatus(t, service, debateID, core.StatusConcluded)

	if writes := failing.writes(); writes <= paidCap {
		t.Fatalf("попыток записи вердикта: %d, ожидалось больше %d — бесплатные повторы не продолжились",
			writes, paidCap)
	}
	if _, _, verdicts := moderator.calls(); verdicts != paidCap {
		t.Fatalf("платных вызовов модератора: %d, ожидалось ровно %d", verdicts, paidCap)
	}
	// Итог всё равно один и объяснён.
	_, verdicts, notices := countModerationRecords(t, service, debateID)
	if verdicts != 1 {
		t.Fatalf("вердиктов в протоколе: %d, ожидался 1", verdicts)
	}
	if notices != 1 {
		t.Fatalf("уведомлений о недоступности: %d, ожидалось 1", notices)
	}
	if budget := countBudgetNotices(t, service, debateID); budget != 0 {
		t.Fatalf("уведомлений об исчерпанном бюджете: %d — потолок расхода не участвовал", budget)
	}
}

// TestStuckModerationStopsPayingWhenTheChargeIsLost охраняет потолок расхода от
// отказа, который роняет и запись результата, и учёт расхода одновременно — так
// выглядит переполненный или недоступный на запись том.
//
// Повтор перечитывает дебаты из хранилища, поэтому расход, не доехавший до
// хранилища, для него невидим: каждый следующий проход получал бы полный бюджет
// заново, и серия повторов стоила бы столько же серий потолков. Это критерий
// отката 2 из docs/adr/0004-moderator-spend-ceiling.md, зажжённый самим
// механизмом повтора, поэтому проход, потерявший учёт, больше не платит.
//
// Причина в протоколе при этом обязана остаться правдой. Потолок расхода здесь
// не срабатывал — бюджет почти не тронут, — и заявить в протоколе «бюджет
// исчерпан» значило бы опубликовать ложную причину: SPEC.md различает их
// колонкой Cause, и ровно эту подмену запрещает
// TestUnavailableModeratorIsNotReportedAsBudgetDegradation на уровне телеметрии.
func TestStuckModerationStopsPayingWhenTheChargeIsLost(t *testing.T) {
	for _, testCase := range []struct {
		name string
		mode core.DebateMode
		// Уведомление, которое обязано объяснить исход, и следует ли за ним вердикт.
		notice       string
		wantVerdicts int
	}{
		{name: "moderator", mode: core.ModeModerator, notice: noticeVerdictWithheld},
		{name: "hybrid", mode: core.ModeHybrid, notice: noticeVotedVerdict, wantVerdicts: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := openTestStore(t)
			failing := &lostChargeStorage{Storage: database, failures: 3}
			records := &recordingHandler{}
			moderator := &spendingModerator{usage: core.ModerationUsage{Billed: true, InputTokens: 9000}}
			service := core.NewService(failing, core.NewHub(), moderator, slog.New(records),
				core.WithModeratorBudget(core.ModeratorBudget{DebateTokens: 500_000}),
				core.WithBackgroundTuningForTest(testTick, testRetryDelay, testPaidCap))
			runCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go service.Run(runCtx)

			debateID, agents := startedDebate(t, service, testCase.mode, 1)
			playRound(t, service, debateID, agents, "")
			waitForStatus(t, service, debateID, core.StatusConcluded)

			if _, verdicts := moderator.calls(); verdicts != 1 {
				t.Fatalf("платных вызовов модератора: %d, ожидался 1", verdicts)
			}
			if budget := countBudgetNotices(t, service, debateID); budget != 0 {
				t.Fatalf("протокол заявил исчерпанный бюджет %d раз, а потолок не срабатывал", budget)
			}
			messages, err := service.Messages(debateID, 0)
			if err != nil {
				t.Fatalf("Messages: %v", err)
			}
			explained := 0
			for _, message := range messages {
				if message.Kind == core.KindSystem && message.Text == testCase.notice {
					explained++
				}
			}
			if explained != 1 {
				t.Fatalf("уведомлений %q: %d, ожидалось 1", testCase.notice, explained)
			}
			if _, verdicts, _ := countModerationRecords(t, service, debateID); verdicts != testCase.wantVerdicts {
				t.Fatalf("вердиктов в протоколе: %d, ожидалось %d", verdicts, testCase.wantVerdicts)
			}
			// Телеметрия отличает потерянный учёт от сработавшего потолка: критерий
			// отката 1 из ADR 0004 считает именно потолок.
			attributes, found := records.find("расход модератора за дебаты")
			if !found {
				t.Fatal("итоговая строка расхода не записана")
			}
			if attributes["degraded"] != "false" {
				t.Fatalf("degraded = %q — потолок расхода не срабатывал", attributes["degraded"])
			}
			if attributes["charge_lost"] != "true" {
				t.Fatalf("charge_lost = %q, ожидалось true", attributes["charge_lost"])
			}
		})
	}
}

// TestARecordedDegradationBindsItsRound охраняет непротиворечивость артефакта.
//
// Уведомление о деградации — обещание потребителю: SPEC.md публикует для каждого,
// что именно потеряно и следует ли за ним запись. Проход, записавший уведомление
// и упавший на следующей записи, оставляет это обещание в протоколе; повтор
// приходит через минуту, к которой модератор может уже отвечать. Если повтор
// запишет то, что уведомление отрицает, артефакт скажет о раунде две разные вещи
// — и ни одно правило conformance этого не поймает, потому что по отдельности
// допустимы оба утверждения.
//
// До фонового повтора такое расхождение требовало рестарта между проходами.
// Именно поэтому случай проверяется здесь: повтор сделал его обычным путём того
// самого сбоя, ради которого он и добавлен.
func TestARecordedDegradationBindsItsRound(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mode   core.DebateMode
		rounds int
		// failing роняет запись, следующую за уведомлением, оставляя дебаты в
		// moderating; ко второму проходу модератор уже отвечает.
		failing func(core.Storage) stuckStorage
		notice  string
		// Чего в раунде уведомления быть не должно, что бы ни ответила модель.
		forbidSummaries bool
		forbidVerdicts  bool
		// wantSystemVerdict — вердикт есть, но его обязана вынести «система».
		wantSystemVerdict bool
	}{
		{
			name: "moderator: за пропуском резюме не появляется резюме",
			mode: core.ModeModerator, rounds: 2,
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusRunning, 2, storage)
			},
			notice: noticeSkippedSummary, forbidSummaries: true,
		},
		{
			name: "moderator: за отказом от вердикта не появляется вердикт",
			mode: core.ModeModerator, rounds: 1,
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusConcluded, 1, storage)
			},
			notice: noticeVerdictWithheld, forbidVerdicts: true,
		},
		{
			name: "hybrid: за итогом по голосам не появляется вердикт модели",
			mode: core.ModeHybrid, rounds: 1,
			failing: func(storage core.Storage) stuckStorage {
				return &verdictWriteFailure{Storage: storage}
			},
			notice: noticeVotedVerdict, wantSystemVerdict: true,
		},
		{
			name: "hybrid: за пропуском резюме не появляется резюме",
			mode: core.ModeHybrid, rounds: 2,
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusRunning, 2, storage)
			},
			notice: noticeSkippedSummary, forbidSummaries: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := openTestStore(t)
			failing := testCase.failing(database)
			// Первый проход застаёт модератора недоступным, дальше он отвечает.
			moderator := &recoveringModerator{failures: 1}
			service := core.NewService(failing, core.NewHub(), moderator,
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				core.WithBackgroundTuningForTest(testTick, testRetryDelay, testPaidCap))
			runCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go service.Run(runCtx)

			debateID, agents := startedDebate(t, service, testCase.mode, testCase.rounds)
			playRound(t, service, debateID, agents, "")
			waitForNoticeText(t, service, debateID, testCase.notice)
			for round := 2; round <= testCase.rounds; round++ {
				playRound(t, service, debateID, agents, "")
			}
			waitForStatus(t, service, debateID, core.StatusConcluded)
			if !failing.failedOnce() {
				t.Fatal("сбой записи не сработал: повторять было нечего")
			}
			if moderator.calls() == 0 {
				t.Fatal("модератор ни разу не вызывался: деградировать было нечему")
			}

			messages, err := service.Messages(debateID, 0)
			if err != nil {
				t.Fatalf("Messages: %v", err)
			}
			var noticeRound int
			for _, message := range messages {
				if message.Kind == core.KindSystem && message.Text == testCase.notice {
					noticeRound = message.Round
				}
			}
			for _, message := range messages {
				if message.Round != noticeRound {
					continue
				}
				switch {
				case testCase.forbidSummaries && message.Kind == core.KindSummary:
					t.Fatalf("раунд %d объявил пропуск резюме, но резюме в нём есть: %q",
						noticeRound, message.Text)
				case testCase.forbidVerdicts && message.Kind == core.KindVerdict:
					t.Fatalf("раунд %d объявил, что вердикта не будет, но вердикт в нём есть: %q",
						noticeRound, message.Text)
				case testCase.wantSystemVerdict && message.Kind == core.KindVerdict &&
					message.SpeakerName != "система":
					t.Fatalf("раунд %d объявил итог по голосам, но вердикт вынес %q",
						noticeRound, message.SpeakerName)
				}
			}
		})
	}
}

// recoveringModerator недоступен на первых вызовах и отвечает дальше: так
// выглядит провайдер, переживший короткий сбой, — и именно так повтор застаёт
// раунд, который уже объявил о деградации.
type recoveringModerator struct {
	failures int
	mu       sync.Mutex
	seen     int
}

func (*recoveringModerator) Name() string { return "recovering moderator" }

func (m *recoveringModerator) available() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen++
	return m.seen > m.failures
}

func (m *recoveringModerator) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen
}

func (m *recoveringModerator) CheckRound(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	if !m.available() {
		return core.RoundSummary{}, core.ModerationUsage{}, errors.New("модератор недоступен")
	}
	return core.RoundSummary{Summary: "Итог раунда."}, core.ModerationUsage{}, nil
}

func (m *recoveringModerator) Summary(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	if !m.available() {
		return core.RoundSummary{}, core.ModerationUsage{}, errors.New("модератор недоступен")
	}
	return core.RoundSummary{Summary: "Итог раунда."}, core.ModerationUsage{}, nil
}

func (m *recoveringModerator) Verdict(
	context.Context, string, string, []int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	if !m.available() {
		return core.ModerationVerdict{}, core.ModerationUsage{}, errors.New("модератор недоступен")
	}
	return core.ModerationVerdict{FinalAnswer: "Итог модели."}, core.ModerationUsage{}, nil
}

// lostChargeStorage роняет учёт расхода всегда, а первые несколько записей
// вердикта — нет: так выглядит том, который перестал принимать запись и потом
// освободился.
type lostChargeStorage struct {
	core.Storage
	failures int
	mu       sync.Mutex
	attempts int
}

func (s *lostChargeStorage) AddMessage(message core.Message) (int64, error) {
	if message.Kind == core.KindVerdict {
		s.mu.Lock()
		s.attempts++
		failed := s.attempts <= s.failures
		s.mu.Unlock()
		if failed {
			return 0, errors.New("injected verdict write failure")
		}
	}
	return s.Storage.AddMessage(message)
}

func (*lostChargeStorage) AddModeratorTokens(string, int) error {
	return errors.New("injected moderator token accounting failure")
}

// verdictWriteFailureTimes роняет заданное число первых записей вердикта.
type verdictWriteFailureTimes struct {
	core.Storage
	failures int
	mu       sync.Mutex
	attempts int
}

func (s *verdictWriteFailureTimes) AddMessage(message core.Message) (int64, error) {
	if message.Kind == core.KindVerdict {
		s.mu.Lock()
		s.attempts++
		failed := s.attempts <= s.failures
		s.mu.Unlock()
		if failed {
			return 0, errors.New("injected verdict write failure")
		}
	}
	return s.Storage.AddMessage(message)
}

func (s *verdictWriteFailureTimes) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// waitForLogLine ждёт строку лога: фоновый проход живёт в своей горутине.
func waitForLogLine(t *testing.T, records *recordingHandler, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := records.find(message); found {
			return
		}
		time.Sleep(testTick)
	}
	t.Fatalf("строка лога %q не появилась", message)
}

// TestResumedConclusionKeepsTheDegradationAttribution охраняет тот же счётчик от
// повтора. Итоговая строка расхода пишется после успешного перехода в concluded,
// поэтому проход, записавший деградировавший вердикт и упавший на переходе, не
// пишет её вовсе — оператор увидит только строку повтора. Повтор берёт готовый
// вердикт и сам ничего не деградирует, поэтому обязан восстановить причину из
// протокола: иначе сработавший потолок исчезает из счёта критерия отката 1
// (docs/adr/0004-moderator-spend-ceiling.md).
func TestResumedConclusionKeepsTheDegradationAttribution(t *testing.T) {
	for _, testCase := range []struct {
		name string
		mode core.DebateMode
		// failing роняет одну запись после уведомления о бюджете: либо переход в
		// concluded (вердикт уже сохранён — повтор восстановит причину из него),
		// либо сам вердикт (сохранён только уведомление — повтор подчинится ему).
		// Причина восстановлена в обоих случаях, и телеметрия обязана это сказать.
		failing func(core.Storage) stuckStorage
	}{
		{
			name: "moderator: падает переход в concluded",
			mode: core.ModeModerator,
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusConcluded, 1, storage)
			},
		},
		{
			name: "hybrid: падает переход в concluded",
			mode: core.ModeHybrid,
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusConcluded, 1, storage)
			},
		},
		{
			name: "moderator: падает запись вердикта после уведомления",
			mode: core.ModeModerator,
			failing: func(storage core.Storage) stuckStorage {
				return &verdictWriteFailure{Storage: storage}
			},
		},
		{
			name: "hybrid: падает запись вердикта после уведомления",
			mode: core.ModeHybrid,
			failing: func(storage core.Storage) stuckStorage {
				return &verdictWriteFailure{Storage: storage}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := openTestStore(t)
			records := &recordingHandler{}
			failing := testCase.failing(database)
			service := core.NewService(failing, core.NewHub(), &spendingModerator{}, slog.New(records),
				// Потолок ниже стоимости любого вызова: вердикт деградирует по бюджету.
				core.WithModeratorBudget(core.ModeratorBudget{DebateTokens: 1}),
				core.WithBackgroundTuningForTest(testTick, testRetryDelay, testPaidCap))
			runCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go service.Run(runCtx)

			debateID, agents := startedDebate(t, service, testCase.mode, 1)
			playRound(t, service, debateID, agents, "")
			waitForStatus(t, service, debateID, core.StatusConcluded)
			if !failing.failedOnce() {
				t.Fatal("запись не падала: повторять было нечего")
			}

			waitForLogLine(t, records, "расход модератора за дебаты")
			attributes, _ := records.find("расход модератора за дебаты")
			if attributes["degraded"] != "true" {
				t.Fatalf("degraded = %q, ожидалось true: потолок сработал", attributes["degraded"])
			}
			if attributes["verdict_degradation"] != "budget" {
				t.Fatalf("verdict_degradation = %q, ожидалось budget", attributes["verdict_degradation"])
			}
			// Причина восстановлена повтором, а не наблюдалась им: без этой пометки
			// соседние поля этой же строки читались бы как относящиеся к тому же
			// проходу, хотя деградацию произвёл предыдущий.
			if attributes["attribution"] != "recovered" {
				t.Fatalf("attribution = %q, ожидалось recovered", attributes["attribution"])
			}
		})
	}
}

// TestLateResumeDoesNotRemoderateAConcludedDebate охраняет последнюю защиту от
// повторной модерации. Фоновый проход читает список дебатов без замка переходов,
// поэтому между чтением и запуском дебаты могут успеть завершиться. Учёт попыток
// такой запуск обычно не пропускает, но опереться на это одно нельзя: цена
// ошибки — второй вердикт в раунде, после которого moderationMessagesForRound
// считает раунд неоднозначным и дебаты не модерируются уже никогда (ADR 0002).
func TestLateResumeDoesNotRemoderateAConcludedDebate(t *testing.T) {
	database := openTestStore(t)
	hub := core.NewHub()
	moderator := &countingModerator{}
	service := core.NewService(database, hub, moderator,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		core.WithBackgroundTuningForTest(testTick, testRetryDelay, testPaidCap))

	debateID, agents := startedDebate(t, service, core.ModeModerator, 1)
	// Подписка до закрытия раунда: GetDebate читает хранилище без замка, поэтому
	// waitForStatus может обогнать публикацию события, и подписка после него
	// поймала бы событие исходного прохода как чужое.
	events := hub.Subscribe(debateID)
	defer hub.Unsubscribe(debateID, events)
	playRound(t, service, debateID, agents, "")
	waitForStatus(t, service, debateID, core.StatusConcluded)
	drainConcluded(t, events)
	_, verdictsBefore, _ := countModerationRecords(t, service, debateID)
	_, _, callsBefore := moderator.calls()

	service.ModerateForTest(context.Background(), debateID)

	if _, verdicts, _ := countModerationRecords(t, service, debateID); verdicts != verdictsBefore {
		t.Fatalf("вердиктов после опоздавшего прохода: %d, было %d", verdicts, verdictsBefore)
	}
	if _, _, calls := moderator.calls(); calls != callsBefore {
		t.Fatalf("вызовов модератора после опоздавшего прохода: %d, было %d", calls, callsBefore)
	}
	select {
	case event := <-events:
		t.Fatalf("опоздавший проход опубликовал событие %s", event.Type)
	default:
	}
}

// drainConcluded вычитывает события до завершения дебатов включительно.
func drainConcluded(t *testing.T, events <-chan core.Event) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Type == core.EventConcluded {
				return
			}
		case <-deadline:
			t.Fatal("событие о завершении дебатов не пришло")
		}
	}
}

// stuckStorage — хранилище, которое роняет первую запись и умеет подтвердить,
// что она действительно упала. Без этого подтверждения тест на возобновление
// прошёл бы и на прогоне, где возобновлять было нечего.
type stuckStorage interface {
	core.Storage
	failedOnce() bool
}

func newFailingTransition(status core.DebateStatus, round int, storage core.Storage) *failingTransitionStorage {
	return &failingTransitionStorage{
		Storage: storage, status: status, round: round, attempted: make(chan struct{}),
	}
}

// heldModerator держит каждый вызов до освобождения и считает их. Нужен, чтобы
// тест наблюдал именно живую модерацию, а не быструю.
type heldModerator struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	freed   sync.Once
	mu      sync.Mutex
	seen    int
}

func (*heldModerator) Name() string { return "held moderator" }

func (m *heldModerator) hold(ctx context.Context) error {
	m.mu.Lock()
	m.seen++
	m.mu.Unlock()
	m.once.Do(func() { close(m.started) })
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *heldModerator) CheckRound(
	ctx context.Context, _, _ string, _ int, _ []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	if err := m.hold(ctx); err != nil {
		return core.RoundSummary{}, core.ModerationUsage{}, err
	}
	return core.RoundSummary{Summary: "Итог раунда."}, core.ModerationUsage{}, nil
}

func (m *heldModerator) Summary(
	ctx context.Context, _, _ string, _ int, _ []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	if err := m.hold(ctx); err != nil {
		return core.RoundSummary{}, core.ModerationUsage{}, err
	}
	return core.RoundSummary{Summary: "Итог раунда."}, core.ModerationUsage{}, nil
}

func (m *heldModerator) Verdict(
	ctx context.Context, _, _ string, _ []int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	if err := m.hold(ctx); err != nil {
		return core.ModerationVerdict{}, core.ModerationUsage{}, err
	}
	return core.ModerationVerdict{FinalAnswer: "Итог."}, core.ModerationUsage{}, nil
}

func (m *heldModerator) unblock() { m.freed.Do(func() { close(m.release) }) }

func (m *heldModerator) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return database
}

// countBudgetNotices считает заявления протокола об исчерпанном бюджете.
func countBudgetNotices(t *testing.T, service *core.Service, debateID string) int {
	t.Helper()
	messages, err := service.Messages(debateID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	count := 0
	for _, message := range messages {
		if message.Kind == core.KindSystem && strings.HasPrefix(message.Text, "Бюджет модератора") {
			count++
		}
	}
	return count
}

// countModerationRecords считает записи модератора в протоколе: повтор обязан
// довести дебаты до конца, не задвоив ни одну из них.
func countModerationRecords(t *testing.T, service *core.Service, debateID string) (summaries, verdicts, notices int) {
	t.Helper()
	messages, err := service.Messages(debateID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	for _, message := range messages {
		switch {
		case message.Kind == core.KindSummary:
			summaries++
		case message.Kind == core.KindVerdict:
			verdicts++
		case message.Kind == core.KindSystem && strings.HasPrefix(message.Text, noticeUnavailablePrefix):
			notices++
		}
	}
	return summaries, verdicts, notices
}
