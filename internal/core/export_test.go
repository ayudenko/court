package core_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"court/internal/core"
	"court/internal/store"
)

// TestExportSnapshotDoesNotMixTwoMoments — экспорт обязан быть одним состоянием
// дебатов, а не состоянием до перехода вместе с протоколом после него.
//
// Тест провоцирует ровно этот стык: пока чтение идёт, соседняя горутина
// пытается довести дебаты до вердикта. Замок переходов обязан удержать её до
// конца чтения, поэтому в результате не может оказаться вердикта при статусе
// «идёт дискуссия». Без замка переход успевает пройти между двумя чтениями, и
// артефакт описывает момент, которого не было, ничем об этом не сообщая.
func TestExportSnapshotDoesNotMixTwoMoments(t *testing.T) {
	interleaving := &storageWithReadHook{}
	service := newServiceOver(t, interleaving)
	debateID, challenger := debateAwaitingItsLastTurn(t, service)

	// Ход, закрывающий последний раунд, запускается из чтения: он обязан ждать.
	posted := make(chan error, 1)
	interleaving.armOnce(func() {
		go func() {
			_, err := service.PostArgument(context.Background(), challenger, debateID, "second position", "")
			posted <- err
		}()
		waitForConclusionAttempt(service, debateID)
	})

	interleaving.observing.Store(true)
	exported, err := service.ExportSnapshot(context.Background(), debateID)
	interleaving.observing.Store(false)
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	// Иначе тест зелен и тогда, когда стык не воспроизвёлся: перехват мог
	// сработать на чужом чтении, а экспорт — увидеть одно чистое состояние.
	if !interleaving.firedDuringExport() {
		t.Fatal("the concurrent transition was never attempted inside the export; the test proves nothing")
	}

	verdicts := 0
	for _, message := range exported.Messages {
		if message.Kind == core.KindVerdict {
			verdicts++
		}
	}
	concluded := exported.Debate.Status == core.StatusConcluded
	if (verdicts > 0) != concluded {
		t.Fatalf("status %q with %d verdict messages: the artifact mixes two moments",
			exported.Debate.Status, verdicts)
	}
	if len(exported.Participants) != len(exported.Debate.Participants) {
		t.Fatalf("participant metadata count = %d, participants = %d",
			len(exported.Participants), len(exported.Debate.Participants))
	}

	if err := <-posted; err != nil {
		t.Fatalf("blocked PostArgument: %v", err)
	}
	// Дебаты доводятся до конца до закрытия хранилища в t.Cleanup.
	waitForStatus(t, service, debateID, core.StatusConcluded)
}

// TestExportSnapshotFailsInsteadOfDroppingVotes — сорванное чтение протокола
// обязано провалить экспорт. Молчаливая деградация здесь неразличима: артефакт
// без голосов выглядит ровно как дебаты, в которых никто не голосовал, и
// проходит любую проверку формата.
func TestExportSnapshotFailsInsteadOfDroppingVotes(t *testing.T) {
	unreadable := &storageWithReadHook{transcriptErr: errors.New("transcript unavailable")}
	service := newServiceOver(t, unreadable)
	debateID, _ := debateAwaitingItsLastTurn(t, service)

	if _, err := service.ExportSnapshot(context.Background(), debateID); err == nil {
		t.Fatal("export succeeded while the transcript could not be read")
	}
}

// TestExportSnapshotDoesNotReportAMissingAgentAsAMissingDebate — участник без
// агента это расхождение внутри хранилища. Если ошибка донесёт до границы HTTP
// признак «не найдено», клиент получит 404 на существующие дебаты и уйдёт
// чинить свой идентификатор вместо того, чтобы сообщить о поломке.
func TestExportSnapshotDoesNotReportAMissingAgentAsAMissingDebate(t *testing.T) {
	inconsistent := &storageWithReadHook{agentErr: store.ErrNotFound}
	service := newServiceOver(t, inconsistent)
	debateID, _ := debateAwaitingItsLastTurn(t, service)

	_, err := service.ExportSnapshot(context.Background(), debateID)
	if err == nil {
		t.Fatal("export succeeded without participant metadata")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error carries a not-found marker and would answer 404: %v", err)
	}
}

// TestExportSnapshotLeavesTheQueueWhenTheCallerIsGone — ожидание замка обязано
// быть отменяемым: маршрут публичный, и запрос отключившегося клиента не должен
// ни занимать очередь переходов, ни делать работу в пустоту.
//
// Проверяются оба пути. Отменённый заранее вызов не должен даже пытаться взять
// свободный замок; отменённый в ожидании — обязан выйти из очереди, и это
// главный случай: именно он снимает нагрузку с очереди переходов.
func TestExportSnapshotLeavesTheQueueWhenTheCallerIsGone(t *testing.T) {
	blocking := &storageWithReadHook{}
	service := newServiceOver(t, blocking)
	debateID, _ := debateAwaitingItsLastTurn(t, service)

	cancelledEarly, cancelEarly := context.WithCancel(context.Background())
	cancelEarly()
	if _, err := service.ExportSnapshot(cancelledEarly, debateID); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled ExportSnapshot error = %v, want context.Canceled", err)
	}

	// Замок занимает соседнее чтение и держит, пока тест не отпустит.
	holding, release := make(chan struct{}), make(chan struct{})
	blocking.armOnce(func() {
		close(holding)
		<-release
	})
	holder := make(chan error, 1)
	go func() {
		_, err := service.ExportSnapshot(context.Background(), debateID)
		holder <- err
	}()
	<-holding

	waiting, cancelWaiting := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancelWaiting()
	}()
	if _, err := service.ExportSnapshot(waiting, debateID); !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("ExportSnapshot waiting for a held lock: error = %v, want context.Canceled", err)
	}

	close(release)
	if err := <-holder; err != nil {
		t.Fatalf("holding ExportSnapshot: %v", err)
	}
}

// debateAwaitingItsLastTurn доводит дебаты до первой поданной реплики
// последнего раунда: следующий ход завершает дискуссию.
func debateAwaitingItsLastTurn(t *testing.T, service *core.Service) (string, core.Agent) {
	t.Helper()
	debateID, agents := startedDebate(t, service, core.ModeHybrid, 1)
	if _, err := service.PostArgument(context.Background(), agents[0], debateID, "first position", ""); err != nil {
		t.Fatalf("creator PostArgument: %v", err)
	}
	return debateID, agents[1]
}

// waitForConclusionAttempt даёт заблокированному переходу время пройти. Если
// замок работает, ожидание всегда истекает; если нет — переход успевает, и
// проверяемый инвариант ломается.
func waitForConclusionAttempt(service *core.Service, debateID string) {
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if view, err := service.GetDebate(debateID); err == nil && view.Status == core.StatusConcluded {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// storageWithReadHook подменяет чтения хранилища: запускает подготовленное
// действие внутри первого чтения дебатов после взведения и умеет отказывать на
// путях, которые экспорт обязан довести до ошибки, а не проглотить.
type storageWithReadHook struct {
	core.Storage
	armed         atomic.Bool
	observing     atomic.Bool
	insideExport  atomic.Bool
	action        func()
	transcriptErr error
	agentErr      error
}

func (s *storageWithReadHook) armOnce(action func()) {
	s.action = action
	s.armed.Store(true)
}

// firedDuringExport сообщает, что перехват сработал именно внутри
// наблюдаемого вызова, а не на постороннем чтении.
func (s *storageWithReadHook) firedDuringExport() bool { return s.insideExport.Load() }

func (s *storageWithReadHook) GetDebate(id string) (core.Debate, error) {
	debate, err := s.Storage.GetDebate(id)
	if s.armed.CompareAndSwap(true, false) {
		s.insideExport.Store(s.observing.Load())
		s.action()
	}
	return debate, err
}

func (s *storageWithReadHook) Messages(debateID string, afterSeq int64) ([]core.Message, error) {
	if s.transcriptErr != nil {
		return nil, s.transcriptErr
	}
	return s.Storage.Messages(debateID, afterSeq)
}

func (s *storageWithReadHook) AgentByID(id string) (core.Agent, error) {
	if s.agentErr != nil {
		return core.Agent{}, s.agentErr
	}
	return s.Storage.AgentByID(id)
}

func newServiceOver(t *testing.T, hook *storageWithReadHook) *core.Service {
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
	hook.Storage = database
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return core.NewService(hook, core.NewHub(), consensusModerator{}, logger)
}
