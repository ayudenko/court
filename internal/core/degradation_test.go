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

// Тексты повторены дословно, а не взяты из core.Notice*, намеренно. Читатель
// артефакта различает деградации именно по этим строкам, и подстановка константы
// сделала бы тест согласным с любой её переформулировкой. Вместе с
// TestSpecPublishesTheDegradationNoticesTheServiceEmits это значит, что
// переформулировка требует трёх согласованных правок: константа, этот тест и
// SPEC.md — ровно та цена, которую должно стоить изменение опубликованного
// текста протокола.
const (
	noticeSkippedSummary    = "Модератор недоступен, дискуссия продолжается без промежуточного итога."
	noticeVotedVerdict      = "Модератор недоступен, итог подведён детерминированно по голосам участников."
	noticeVerdictWithheld   = "Модератор недоступен, дебаты завершены без вердикта."
	noticeUnavailablePrefix = "Модератор недоступен"
)

// unavailableModerator — модератор, недоступный на всех вызовах. Это форма
// развёртывания hybrid без LLM-ключа на сервере, а не только сбой провайдера:
// такой сервер поднимается штатно и обязан отдавать читаемый протокол.
type unavailableModerator struct{}

func (unavailableModerator) Name() string { return "unavailable moderator" }

func (unavailableModerator) CheckRound(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return core.RoundSummary{}, core.ModerationUsage{}, errors.New("модератор недоступен: ключ не задан")
}

func (m unavailableModerator) Summary(
	ctx context.Context, question, transcript string, round int, allowedSeqs []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return m.CheckRound(ctx, question, transcript, round, allowedSeqs)
}

func (unavailableModerator) Verdict(
	context.Context, string, string, []int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	return core.ModerationVerdict{}, core.ModerationUsage{}, errors.New("модератор недоступен: ключ не задан")
}

// TestHybridRecordsEverySkippedModerationResult охраняет читаемость протокола в
// развёртывании hybrid без модератора. Раньше недоступность модератора писалась
// в протокол только в режиме moderator, а в hybrid резюме просто отсутствовало и
// вердикт молча собирался по голосам. Читатель такого артефакта не мог отличить
// «резюме не производилось» от «резюме потеряно» — то есть неотличимость
// попадала ровно в артефакт, по которому судят реализацию протокола.
func TestHybridRecordsEverySkippedModerationResult(t *testing.T) {
	const rounds = 3
	service := newTestServiceWithModerator(t, unavailableModerator{})
	debateID, agents := startedDebate(t, service, core.ModeHybrid, rounds)
	// Голоса за себя: консенсуса нет, поэтому дебаты проезжают все раунды и
	// каждый промежуточный итог оказывается пропущенным.
	for round := 1; round <= rounds; round++ {
		playRound(t, service, debateID, agents, "")
	}
	if view := waitForStatus(t, service, debateID, core.StatusConcluded); view.Consensus {
		t.Fatalf("голоса за себя не могут дать консенсус")
	}

	messages, err := service.Messages(debateID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	summaries := 0
	notices := make(map[int][]string)
	verdictSeq, verdictSpeaker := int64(-1), ""
	for _, message := range messages {
		switch {
		case message.Kind == core.KindSummary:
			summaries++
		case message.Kind == core.KindVerdict:
			verdictSeq, verdictSpeaker = message.Seq, message.SpeakerName
		case message.Kind == core.KindSystem && strings.HasPrefix(message.Text, noticeUnavailablePrefix):
			notices[message.Round] = append(notices[message.Round], message.Text)
		}
	}
	if summaries != 0 {
		t.Fatalf("резюме без модератора: %d, ожидалось 0", summaries)
	}
	if verdictSeq < 0 || verdictSpeaker != "система" {
		t.Fatalf("вердикт по голосам: seq=%d speaker=%q", verdictSeq, verdictSpeaker)
	}
	// Каждый раунд объясняет свою деградацию, и текст отличает пропущенное
	// резюме от вердикта не моделью: одного «говорит система» недостаточно.
	for round := 1; round < rounds; round++ {
		if got := notices[round]; len(got) != 1 || got[0] != noticeSkippedSummary {
			t.Fatalf("раунд %d: уведомления о пропущенном резюме = %q", round, got)
		}
	}
	if got := notices[rounds]; len(got) != 1 || got[0] != noticeVotedVerdict {
		t.Fatalf("последний раунд: уведомления о вердикте по голосам = %q", got)
	}
	// Уведомление предшествует итогу, который объясняет: иначе читатель,
	// остановившийся на вердикте, причину не увидит.
	for _, message := range messages {
		if message.Text == noticeVotedVerdict && message.Seq > verdictSeq {
			t.Fatalf("уведомление (seq=%d) записано после вердикта (seq=%d)", message.Seq, verdictSeq)
		}
	}
}

// TestUnavailableModeratorNoticeIsRecordedOncePerRound охраняет ту же
// читаемость от противоположного сбоя: повторный проход модерации раунда,
// возобновлённый после неудачной записи, не имеет права записать уведомление
// второй раз. Случаев четыре, потому что защита стоит в четырёх местах, и точка
// отказа у каждого своя: после пропущенного резюме идёт переход к следующему
// раунду, после уведомления о вердикте — запись вердикта в hybrid и переход в
// concluded в moderator.
//
// Сервис во всех случаях один: повтор обязан случиться в работающем процессе, а
// не после рестарта (issue #40).
func TestUnavailableModeratorNoticeIsRecordedOncePerRound(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mode   core.DebateMode
		rounds int
		notice string
		// failing оборачивает хранилище так, чтобы первый проход упал уже после
		// записи уведомления и оставил дебаты в статусе moderating.
		failing func(core.Storage) stuckStorage
	}{
		{
			name:   "hybrid: падает переход к следующему раунду",
			mode:   core.ModeHybrid,
			rounds: 2,
			notice: noticeSkippedSummary,
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusRunning, 2, storage)
			},
		},
		{
			name:   "hybrid: падает запись вердикта по голосам",
			mode:   core.ModeHybrid,
			rounds: 1,
			notice: noticeVotedVerdict,
			failing: func(storage core.Storage) stuckStorage {
				return &verdictWriteFailure{Storage: storage}
			},
		},
		{
			name:   "moderator: падает переход к следующему раунду",
			mode:   core.ModeModerator,
			rounds: 2,
			notice: noticeSkippedSummary,
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusRunning, 2, storage)
			},
		},
		{
			name:   "moderator: падает переход в concluded",
			mode:   core.ModeModerator,
			rounds: 1,
			notice: noticeVerdictWithheld,
			failing: func(storage core.Storage) stuckStorage {
				return newFailingTransition(core.StatusConcluded, 1, storage)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := openTestStore(t)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			failing := testCase.failing(database)

			service := core.NewService(failing, core.NewHub(), unavailableModerator{}, logger,
				core.WithBackgroundTuningForTest(testTick, testRetryDelay, testPaidCap))
			runCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go service.Run(runCtx)

			debateID, agents := startedDebate(t, service, testCase.mode, testCase.rounds)
			playRound(t, service, debateID, agents, "")
			waitForNoticeText(t, service, debateID, testCase.notice)
			// Раунд, на котором упала запись, повторяется целиком; если после него
			// дебаты ещё не кончились, оставшиеся ходы доигрываются.
			for round := 2; round <= testCase.rounds; round++ {
				playRound(t, service, debateID, agents, "")
			}
			waitForStatus(t, service, debateID, core.StatusConcluded)

			if !failing.failedOnce() {
				t.Fatal("сбой записи не сработал: повторять было нечего")
			}
			messages, err := service.Messages(debateID, 0)
			if err != nil {
				t.Fatalf("Messages: %v", err)
			}
			notices := 0
			for _, message := range messages {
				if message.Kind == core.KindSystem && message.Text == testCase.notice {
					notices++
				}
			}
			if notices != 1 {
				t.Fatalf("уведомлений после повтора: %d, ожидалось 1", notices)
			}
		})
	}
}

// TestUnavailableModeratorIsNotReportedAsBudgetDegradation охраняет счётчик, по
// которому считается критерий отката 1 из
// docs/adr/0004-moderator-spend-ceiling.md: «вердикт деградировал, а расход
// остался под потолком». ADR считает его по полю `degraded` строки «расход
// модератора за дебаты», поэтому недоступность модератора не имеет права его
// поднимать — иначе развёртывание hybrid без LLM-ключа отчитывалось бы о
// сработавшем потолке на каждых дебатах, и критерий стал бы нечитаемым именно
// там, где потолок вообще не участвует.
// Зеркальный случай с исчерпанным бюджетом здесь обязателен: без него
// утверждение «degraded=false» выполнялось бы и у поля, которое не поднимается
// никогда, и тест не отличал бы работающий счётчик от сломанного.
func TestUnavailableModeratorIsNotReportedAsBudgetDegradation(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		budget          int
		wantDegraded    string
		wantDegradation string
	}{
		// Потолок отключён: деградирует только доступность.
		{name: "недоступен", budget: 0, wantDegraded: "false", wantDegradation: "unavailable"},
		// Потолок ниже стоимости любого вызова: до провайдера дело не доходит.
		{name: "бюджет исчерпан", budget: 1, wantDegraded: "true", wantDegradation: "budget"},
	} {
		for _, mode := range []core.DebateMode{core.ModeHybrid, core.ModeModerator} {
			t.Run(testCase.name+"/"+string(mode), func(t *testing.T) {
				records := &recordingHandler{}
				database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
				if err != nil {
					t.Fatalf("store.Open: %v", err)
				}
				t.Cleanup(func() {
					if err := database.Close(); err != nil {
						t.Errorf("store.Close: %v", err)
					}
				})
				service := core.NewService(database, core.NewHub(), unavailableModerator{}, slog.New(records),
					core.WithModeratorBudget(core.ModeratorBudget{DebateTokens: testCase.budget}))
				debateID, agents := startedDebate(t, service, mode, 1)
				playRound(t, service, debateID, agents, "")
				waitForStatus(t, service, debateID, core.StatusConcluded)

				// Строка пишется после успешного перехода в concluded, поэтому
				// наблюдатель статуса может обогнать её.
				waitForLogLine(t, records, "расход модератора за дебаты")
				attributes, found := records.find("расход модератора за дебаты")
				if !found {
					t.Fatalf("итоговая строка расхода не записана; ADR 0004 считает критерий по ней")
				}
				if attributes["degraded"] != testCase.wantDegraded {
					t.Fatalf("degraded = %q, ожидалось %q", attributes["degraded"], testCase.wantDegraded)
				}
				if attributes["verdict_degradation"] != testCase.wantDegradation {
					t.Fatalf("verdict_degradation = %q, ожидалось %q",
						attributes["verdict_degradation"], testCase.wantDegradation)
				}
			})
		}
	}
}

// recordingHandler запоминает записи лога, чтобы тест мог утверждать про поле,
// на которое опирается ADR, а не только про состояние базы.
type recordingHandler struct {
	mu      sync.Mutex
	entries []map[string]string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	entry := map[string]string{"msg": record.Message}
	record.Attrs(func(attr slog.Attr) bool {
		entry[attr.Key] = attr.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, entry)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) find(message string) (map[string]string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.entries {
		if entry["msg"] == message {
			return entry, true
		}
	}
	return nil, false
}

// waitForNoticeText ждёт появления в протоколе системного уведомления с этим
// текстом. Модерация идёт в своей горутине, поэтому опрос, а не предположение.
func waitForNoticeText(t *testing.T, service *core.Service, debateID, notice string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := service.Messages(debateID, 0)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		for _, message := range messages {
			if message.Kind == core.KindSystem && message.Text == notice {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("уведомление %q не появилось в протоколе", notice)
}
