package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"court/internal/conformance"
	"court/internal/core"
	"court/internal/protocol"
	"court/internal/ratelimit"
	"court/internal/store"
)

// TestExportedDebateIsAValidGoldenTrace — главный инвариант эндпоинта: живые
// дебаты и эталонные трассы — один артефакт. Байты ответа обязаны пройти тот же
// разбор, что и checked-in трассы, и вернуться из него без единого изменения;
// иначе golden-трассы подтверждают формат генератора фикстур, а не сервера.
func TestExportedDebateIsAValidGoldenTrace(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	creator, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	challenger, challengerKey := registerWithPersona(t, server, "Reviewer", "Challenges silent nondeterminism.")
	debateID := concludedDebate(t, server, mux, creatorKey, challengerKey)

	recorder := export(mux, debateID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/x-ndjson; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Content-Disposition"); !strings.Contains(got, debateID+".jsonl") {
		t.Fatalf("Content-Disposition = %q, want the debate id", got)
	}

	body := recorder.Body.Bytes()
	replayed, err := protocol.DecodeJSONL(body)
	if err != nil {
		t.Fatalf("exported debate is not a replayable trace: %v", err)
	}
	roundTrip, err := protocol.MarshalJSONL(replayed)
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	if !bytes.Equal(roundTrip, body) {
		t.Fatalf("export is not in canonical order\ngot:\n%s\nwant:\n%s", body, roundTrip)
	}

	var debates, participants, messages, verdicts, votes int
	for _, record := range replayed {
		if record.SchemaVersion != core.CurrentProtocolSchemaVersion {
			t.Fatalf("record %q has schema_version %d", record.RecordType, record.SchemaVersion)
		}
		if record.DebateID != debateID {
			t.Fatalf("record %q has debate_id %q", record.RecordType, record.DebateID)
		}
		switch record.RecordType {
		case protocol.RecordDebate:
			debates++
			if record.Debate.Status != core.StatusConcluded {
				t.Fatalf("exported status = %q, want %q", record.Debate.Status, core.StatusConcluded)
			}
			if record.Debate.Rounds != 1 || record.Debate.CurrentRound != 1 {
				t.Fatalf("exported rounds = %d/%d, want 1/1", record.Debate.CurrentRound, record.Debate.Rounds)
			}
		case protocol.RecordParticipant:
			participants++
			if record.Participant.Persona == "" {
				t.Fatalf("participant %q exported without persona", record.Participant.AgentID)
			}
		case protocol.RecordMessage:
			messages++
		case protocol.RecordVerdict:
			verdicts++
			if record.Verdict.Result == nil {
				t.Fatal("verdict exported without its structured result")
			}
		case protocol.RecordVote:
			votes++
		}
	}
	if debates != 1 || participants != 2 || messages < 2 || verdicts != 1 || votes != 2 {
		t.Fatalf("record counts: debate=%d participants=%d messages=%d verdicts=%d votes=%d",
			debates, participants, messages, verdicts, votes)
	}
	if !strings.Contains(string(body), creator.ID) || !strings.Contains(string(body), challenger.ID) {
		t.Fatal("export does not name both participants")
	}
}

// TestExportedArtifactConformsToSpec проверяет живой HTTP-ответ против
// нормативных правил SPEC.md, а не только checked-in трассы. Трассы записывает
// внутренний генератор; сюда же приходит то, что реально уезжает наружу. Все
// три статуса проверяются потому, что экспорт определён для любого из них, и
// незавершённые дебаты — обычный артефакт, а не краевой случай.
func TestExportedArtifactConformsToSpec(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	_, challengerKey := registerWithPersona(t, server, "Reviewer", "Challenges silent nondeterminism.")

	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")
	requireConformingExport(t, mux, debateID, "open")

	joinDebate(t, mux, debateID, challengerKey)
	startDebate(t, mux, debateID, creatorKey)
	postArgument(t, mux, debateID, creatorKey, "Незавершённые дебаты — тоже артефакт.")
	requireConformingExport(t, mux, debateID, "running")

	postArgument(t, mux, debateID, challengerKey, "Согласен, если правила проверяемы.")
	waitForConclusion(t, server, debateID)
	requireConformingExport(t, mux, debateID, "concluded")
}

func requireConformingExport(t *testing.T, mux *http.ServeMux, debateID, stage string) {
	t.Helper()
	recorder := export(mux, debateID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export at stage %s: status = %d, body = %q", stage, recorder.Code, recorder.Body.String())
	}
	violations := conformance.Check(recorder.Body.Bytes())
	if len(violations) == 0 {
		return
	}
	lines := make([]string, 0, len(violations))
	for _, violation := range violations {
		lines = append(lines, "  "+violation.String())
	}
	t.Fatalf("export at stage %s violates SPEC.md:\n%s", stage, strings.Join(lines, "\n"))
}

// TestExportWithholdsWhatTheDebateViewWithholds — экспорт собирается из того же
// представления, что и публичное чтение, плюс явный список полей агента.
// Контекст дискуссии закрыт до старта, и артефакт не имеет права раздавать его
// в обход этого правила: иначе ранний вход в дебаты снова даёт фору. Список
// полей участника проверяется целиком, потому что persona — единственное, что
// приходит не из представления, а из строки агента: следующее такое поле
// обязано быть решением, а не побочным эффектом.
func TestExportWithholdsWhatTheDebateViewWithholds(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	_, challengerKey := registerWithPersona(t, server, "Reviewer", "Challenges silent nondeterminism.")
	const description = "Материалы, которые видны только со старта дебатов."
	debateID := createDebateWithDescription(t, mux, creatorKey, description)

	open := exportedDebateRecord(t, mux, debateID)
	if open.Status != core.StatusOpen {
		t.Fatalf("status = %q, want %q", open.Status, core.StatusOpen)
	}
	if open.Description != "" {
		t.Fatalf("export leaked the description of an open debate: %q", open.Description)
	}

	joinDebate(t, mux, debateID, challengerKey)
	startDebate(t, mux, debateID, creatorKey)

	started := exportedDebateRecord(t, mux, debateID)
	if started.Description != description {
		t.Fatalf("description after start = %q, want %q", started.Description, description)
	}

	fields := exportedParticipantFields(t, mux, debateID)
	want := map[string]bool{"agent_id": true, "name": true, "persona": true, "stance": true, "joined_at": true}
	for field := range fields {
		if !want[field] {
			t.Fatalf("participant record publishes an undeclared field %q", field)
		}
	}
	for field := range want {
		if !fields[field] {
			t.Fatalf("participant record lost the declared field %q", field)
		}
	}
}

// TestExportBytesAreExactlyTheSharedProducerOutput — маршрут обязан отдавать
// ровно то, что собирает общий продюсер, без единого собственного шага.
// Иначе «эндпоинт и эталоны — один артефакт» держится на договорённости:
// добавленное в обработчике обогащение или фильтрация оставят все проверки
// формата зелёными, а эталонные трассы перестанут описывать сервер.
func TestExportBytesAreExactlyTheSharedProducerOutput(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	_, challengerKey := registerWithPersona(t, server, "Reviewer", "Challenges silent nondeterminism.")
	debateID := concludedDebate(t, server, mux, creatorKey, challengerKey)

	recorder := export(mux, debateID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	snapshot, err := server.svc.ExportSnapshot(context.Background(), debateID)
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	records, err := protocol.Stream(snapshot)
	if err != nil {
		t.Fatalf("protocol.Stream: %v", err)
	}
	want, err := protocol.MarshalJSONL(records)
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	if !bytes.Equal(recorder.Body.Bytes(), want) {
		t.Fatalf("endpoint bytes differ from the shared producer\ngot:\n%s\nwant:\n%s",
			recorder.Body.String(), want)
	}
}

// TestExportSharesTheAddressStreamBudget — экспорт самый дорогой из читающих
// маршрутов и при этом без ключа. Отдельная квота у него означала бы обход
// общего потолка одновременных подключений с адреса.
func TestExportSharesTheAddressStreamBudget(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{StreamsPerClient: 1})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")

	release, err := server.limiter.AcquireStream("", "192.0.2.1")
	if err != nil {
		t.Fatalf("AcquireStream: %v", err)
	}
	if recorder := export(mux, debateID); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("export while the address budget is held: status = %d, want 429", recorder.Code)
	}
	release()
	if recorder := export(mux, debateID); recorder.Code != http.StatusOK {
		t.Fatalf("export after the slot was freed: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestExportIsRefusedWhenTheProcessCeilingIsFull — потолок на процесс есть
// ограничение памяти, а не политика клиента: слот на адрес не ограничивает
// сумму по всем адресам, а один экспорт держит и протокол, и его представление.
// Отказ обязан быть немедленным: очередь — тот же расход, только отложенный.
func TestExportIsRefusedWhenTheProcessCeilingIsFull(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")

	for range MaxConcurrentExports {
		server.exports <- struct{}{}
	}
	recorder := export(mux, debateID)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("export at the process ceiling: status = %d, want 503", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("refusal carries no Retry-After")
	}

	<-server.exports
	if recorder := export(mux, debateID); recorder.Code != http.StatusOK {
		t.Fatalf("export after a slot was freed: status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
}

// TestOneAddressCannotHoldTheWholeExportCeiling — потолок общий, поэтому без
// доли на адрес нескольких переставших читать соединений с одного адреса
// хватает, чтобы маршрут отвечал 503 всем остальным, включая потребителей,
// ради которых он существует.
func TestOneAddressCannotHoldTheWholeExportCeiling(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")

	const greedy = "198.51.100.7"
	for range exportsPerAddress {
		release, ok := server.exportsByAddress.acquire(greedy)
		if !ok {
			t.Fatal("the address could not take its own share")
		}
		defer release()
	}
	if recorder := exportFrom(mux, debateID, greedy); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("past its own share: status = %d, want 503", recorder.Code)
	}
	if recorder := exportFrom(mux, debateID, "203.0.113.8"); recorder.Code != http.StatusOK {
		t.Fatalf("another address was denied by a neighbour: status = %d, body = %q",
			recorder.Code, recorder.Body.String())
	}
}

// TestWorstCaseExportFitsTheDeclaredBudget — потолок одновременных экспортов
// выведен из размера одного артефакта, поэтому размер обязан быть фактом, а не
// ожиданием. Худший случай собирается по собственным лимитам сервиса: реплики
// предельной длины из управляющих символов, каждый из которых уезжает в JSON
// шестью байтами.
func TestWorstCaseExportFitsTheDeclaredBudget(t *testing.T) {
	const debateID = "dbt_worst_case"
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	worstArgument := strings.Repeat("\x01", core.MaxArgumentLen)

	records := []protocol.ExportRecord{{
		RecordType: protocol.RecordDebate, DebateID: debateID,
		Debate: &protocol.DebateRecord{
			Question: strings.Repeat("q", 4000), Description: strings.Repeat("d", 8000),
			Mode: core.ModeHybrid, Status: core.StatusConcluded, Rounds: core.MaxRounds,
			CurrentRound: core.MaxRounds, TurnTimeoutSec: core.MaxTurnTimeout,
			CreatorID: "agt_0", CreatedAt: now,
		},
	}}
	for participant := range core.MaxParticipants {
		id := fmt.Sprintf("agt_%d", participant)
		records = append(records, protocol.ExportRecord{
			RecordType: protocol.RecordParticipant, DebateID: debateID,
			Participant: &protocol.ParticipantRecord{
				AgentID: id, Name: strings.Repeat("n", 64), Persona: strings.Repeat("p", 2000),
				Stance: strings.Repeat("s", 200), JoinedAt: now,
			},
		}, protocol.ExportRecord{
			RecordType: protocol.RecordVote, DebateID: debateID,
			Vote: &protocol.VoteRecord{AgentID: id, AgentName: "n", SupportsID: id, SupportsName: "n"},
		})
	}
	seq := int64(0)
	for round := 1; round <= core.MaxRounds; round++ {
		for participant := range core.MaxParticipants {
			seq++
			records = append(records, protocol.ExportRecord{
				RecordType: protocol.RecordMessage, DebateID: debateID,
				Message: &protocol.MessageRecord{
					Seq: seq, Round: round, SpeakerID: fmt.Sprintf("agt_%d", participant),
					SpeakerName: "n", Kind: core.KindArgument, Text: worstArgument, CreatedAt: now,
				},
			})
		}
		seq++
		records = append(records, protocol.ExportRecord{
			RecordType: protocol.RecordRoundSummary, DebateID: debateID,
			RoundSummary: &protocol.RoundSummaryRecord{
				Seq: seq, Round: round, SpeakerName: "moderator",
				Text: strings.Repeat("m", 20000), Result: &core.RoundSummary{Summary: strings.Repeat("m", 20000)},
				CreatedAt: now,
			},
		})
	}

	data, err := protocol.MarshalJSONL(records)
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	t.Logf("worst-case export = %d bytes, budget %d", len(data), MaxExportBytes)
	if len(data) > MaxExportBytes {
		t.Fatalf("worst-case export is %d bytes, over the declared %d the ceiling is derived from",
			len(data), MaxExportBytes)
	}
	// Тот же артефакт обязан проходить обратно через читателя протокола.
	// Предел чтения — публичное правило C0, и сервер, отдающий больше, чем его
	// собственный ридер принимает, зажигал бы это правило на корректном
	// прогоне: не отказ клиенту, а спецификация, противоречащая реализации.
	if _, err := protocol.DecodeRecords(data); err != nil {
		t.Fatalf("worst-case export does not survive the protocol reader: %v", err)
	}
}

// TestExportCeilingRefusalIsObservable — потолок на процесс ограничивает всех
// сразу, поэтому исчерпать его — способ выключить маршрут. Без строки в логе
// это выглядит как «сервис иногда не отвечает», и критерий отката ADR 0006
// стрелять не по чему.
func TestExportCeilingRefusalIsObservable(t *testing.T) {
	server, mux, logs := newLoggingServer(t, ratelimit.Config{})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")

	for range MaxConcurrentExports {
		server.exports <- struct{}{}
	}
	if recorder := export(mux, debateID); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("export at the process ceiling: status = %d, want 503", recorder.Code)
	}
	if !strings.Contains(logs.String(), ratelimit.ScopeExportCeiling) {
		t.Fatalf("the refusal left no server-side trace: %s", logs.String())
	}
}

// TestExportWriteCannotPinTheCeilingForever — слот потолка держится до конца
// записи, и это обязательно: собранные байты живут, пока их не отдали. Значит
// у записи обязан быть предел, иначе клиент, переставший читать, занимает слот
// навсегда, и четырёх соединений хватает, чтобы выключить маршрут для всех.
// Оборванная по пределу запись при этом остаётся оборванной: длина объявлена.
func TestExportWriteCannotPinTheCeilingForever(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")

	// Обработчик вызывается напрямую: проверяется его собственный контракт
	// записи, а не маршрутизация.
	request := httptest.NewRequest(http.MethodGet, "/api/debates/"+debateID+"/export", nil)
	request.SetPathValue("id", debateID)
	writer := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	server.handleExport(writer, request)

	if writer.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", writer.Code, writer.Body.String())
	}
	if len(writer.deadlines) == 0 || writer.deadlines[0].IsZero() {
		t.Fatalf("the response write has no deadline (%v), so a slow reader holds the ceiling slot forever",
			writer.deadlines)
	}
	// Предел ставится на соединение, и без Server.WriteTimeout net/http его не
	// переармирует: оставленный предел оборвал бы следующий ответ на том же
	// keep-alive соединении — чужой маршрут, чужой клиент, ни строки в логе.
	if last := writer.deadlines[len(writer.deadlines)-1]; !last.IsZero() {
		t.Fatalf("the write deadline (%v) outlives the request and will cut the next response on this connection", last)
	}
	if length := writer.Header().Get("Content-Length"); length != strconv.Itoa(writer.Body.Len()) {
		t.Fatalf("Content-Length = %q, body = %d bytes: a truncated write would pass as a short debate",
			length, writer.Body.Len())
	}
}

// TestSlowExportIsLoggedWithItsPartsSeparated — единственный размерный сигнал
// отката. Чтение и кодирование измеряются раздельно: чтение включает ожидание
// замка переходов, и без разделения медленный чужой писатель был бы неотличим
// от разросшегося артефакта.
func TestSlowExportIsLoggedWithItsPartsSeparated(t *testing.T) {
	server, mux, logs := newLoggingServer(t, ratelimit.Config{})
	server.slowExport = 0
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")

	if recorder := export(mux, debateID); recorder.Code != http.StatusOK {
		t.Fatalf("export: status = %d", recorder.Code)
	}
	line := logs.String()
	for _, want := range []string{"экспорт: медленная сборка", "read_ms=", "encode_ms=", "bytes="} {
		if !strings.Contains(line, want) {
			t.Fatalf("slow-export log is missing %q: %s", want, line)
		}
	}
}

// TestExportOfACancelledRequestIsNotAnEmptySuccess — отменённый запрос обязан
// получить явный статус. Молчаливый возврат из обработчика отдаёт неявный 200 с
// пустым телом, а это артефакт из нуля записей: потребитель, проверяющий код
// ответа, зачтёт его как успешный экспорт.
func TestExportOfACancelledRequestIsNotAnEmptySuccess(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	_, creatorKey := registerWithPersona(t, server, "Architect", "Argues for reproducible evidence.")
	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")

	request := httptest.NewRequest(http.MethodGet, "/api/debates/"+debateID+"/export", nil)
	request.SetPathValue("id", debateID)
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	recorder := httptest.NewRecorder()
	server.handleExport(recorder, request.WithContext(ctx))

	if recorder.Code == http.StatusOK {
		t.Fatalf("cancelled export answered 200 with %d bytes", recorder.Body.Len())
	}
}

// TestExportRejectsUnknownDebate — несуществующие дебаты остаются 404, а не
// пустым, но успешным артефактом.
func TestExportRejectsUnknownDebate(t *testing.T) {
	_, mux := newLimitedServer(t, ratelimit.Config{})
	recorder := export(mux, "dbt_does_not_exist")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("error Content-Type = %q, want JSON", recorder.Header().Get("Content-Type"))
	}
}

// --- Хелперы ---

// deadlineWriter — ResponseWriter, умеющий то, что спрашивает у него
// http.NewResponseController: тестовый recorder этого не умеет, и без него
// нельзя отличить «предел записи не поставлен» от «поставить было некуда».
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func newLoggingServer(t *testing.T, cfg ratelimit.Config) (*Server, *http.ServeMux, *bytes.Buffer) {
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
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	service := core.NewService(database, core.NewHub(), unusedModerator{}, logger)
	server := New(service, logger, ratelimit.New(cfg, ratelimit.WithLogger(logger)))
	mux := http.NewServeMux()
	server.Routes(mux)
	return server, mux, logs
}

func export(mux *http.ServeMux, debateID string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/debates/"+debateID+"/export", nil)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func exportFrom(mux *http.ServeMux, debateID, clientIP string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/debates/"+debateID+"/export", nil)
	request.RemoteAddr = clientIP + ":5555"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func exportedDebateRecord(t *testing.T, mux *http.ServeMux, debateID string) protocol.DebateRecord {
	t.Helper()
	recorder := export(mux, debateID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	records, err := protocol.DecodeJSONL(recorder.Body.Bytes())
	if err != nil {
		t.Fatalf("DecodeJSONL: %v", err)
	}
	if len(records) == 0 || records[0].Debate == nil {
		t.Fatal("export does not start with a debate record")
	}
	return *records[0].Debate
}

// exportedParticipantFields возвращает набор JSON-ключей первой записи об
// участнике — именно ключей, а не типизированной структуры: типизированный
// разбор молча проглотил бы поле, которого в схеме экспорта быть не должно.
func exportedParticipantFields(t *testing.T, mux *http.ServeMux, debateID string) map[string]bool {
	t.Helper()
	recorder := export(mux, debateID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	for _, line := range strings.Split(strings.TrimSuffix(recorder.Body.String(), "\n"), "\n") {
		var record struct {
			RecordType  string         `json:"record_type"`
			Participant map[string]any `json:"participant"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if record.RecordType != string(protocol.RecordParticipant) {
			continue
		}
		fields := make(map[string]bool, len(record.Participant))
		for field := range record.Participant {
			fields[field] = true
		}
		return fields
	}
	t.Fatal("export contains no participant record")
	return nil
}

func registerWithPersona(t *testing.T, server *Server, name, persona string) (core.Agent, string) {
	t.Helper()
	agent, key, err := server.svc.RegisterAgent(name, persona)
	if err != nil {
		t.Fatalf("RegisterAgent(%q): %v", name, err)
	}
	return agent, key
}

// concludedDebate проводит одни гибридные дебаты в один раунд через публичный
// HTTP до статуса concluded. Модератор в тестовом сервере недоступен, поэтому
// вердикт строится детерминированно по голосам — ровно как на инсталляции без
// LLM-ключа.
func concludedDebate(t *testing.T, server *Server, mux *http.ServeMux, creatorKey, challengerKey string) string {
	t.Helper()
	debateID := createDebateWithDescription(t, mux, creatorKey, "Контекст дискуссии.")
	joinDebate(t, mux, debateID, challengerKey)
	startDebate(t, mux, debateID, creatorKey)
	postArgument(t, mux, debateID, creatorKey, "Экспорт обязан воспроизводиться.")
	postArgument(t, mux, debateID, challengerKey, "Согласен, если порядок записей детерминирован.")
	waitForConclusion(t, server, debateID)
	return debateID
}

func waitForConclusion(t *testing.T, server *Server, debateID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		view, err := server.svc.GetDebate(debateID)
		if err != nil {
			t.Fatalf("GetDebate: %v", err)
		}
		if view.Status == core.StatusConcluded {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("debate did not conclude, status = %q", view.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func createDebateWithDescription(t *testing.T, mux *http.ServeMux, key, description string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"question":         "Воспроизводим ли протокол дебатов?",
		"description":      description,
		"stance":           "запись",
		"mode":             string(core.ModeHybrid),
		"rounds":           1,
		"turn_timeout_sec": core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	decoded := authedRequest(t, mux, http.MethodPost, "/api/debates", key, string(body), http.StatusCreated)
	debateID, _ := decoded["id"].(string)
	if debateID == "" {
		t.Fatalf("created debate has no id: %v", decoded)
	}
	return debateID
}

func joinDebate(t *testing.T, mux *http.ServeMux, debateID, key string) {
	t.Helper()
	authedRequest(t, mux, http.MethodPost, "/api/debates/"+debateID+"/join", key,
		`{"stance":"проверка"}`, http.StatusOK)
}

func startDebate(t *testing.T, mux *http.ServeMux, debateID, key string) {
	t.Helper()
	authedRequest(t, mux, http.MethodPost, "/api/debates/"+debateID+"/start", key, "", http.StatusOK)
}

func postArgument(t *testing.T, mux *http.ServeMux, debateID, key, text string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal argument: %v", err)
	}
	authedRequest(t, mux, http.MethodPost, "/api/debates/"+debateID+"/messages", key,
		string(body), http.StatusCreated)
}

func authedRequest(
	t *testing.T,
	mux *http.ServeMux,
	method, path, key, body string,
	wantStatus int,
) map[string]any {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d, body = %q", method, path, recorder.Code, wantStatus, recorder.Body.String())
	}
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return decoded
}
