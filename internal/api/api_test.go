package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"court/internal/core"
)

func TestWriteSSEMakesProtocolSerializationRejectionObservable(t *testing.T) {
	recorder := httptest.NewRecorder()
	err := writeSSE(recorder, core.Event{
		Type: core.EventMessage, DebateID: "dbt_test",
		Message: &core.Message{Kind: "future_message"},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported message kind") {
		t.Fatalf("writeSSE error = %v", err)
	}
	var protocolErr *sseProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("writeSSE error type = %T, want sseProtocolError", err)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("writeSSE wrote a partial invalid event: %q", recorder.Body.String())
	}

	if err := writeSSE(recorder, core.Event{Type: core.EventStarted, DebateID: "dbt_test"}); err != nil {
		t.Fatalf("writeSSE valid event: %v", err)
	}
	if !strings.Contains(recorder.Body.String(), `"schema_version":1`) {
		t.Fatalf("valid SSE event missing schema version: %q", recorder.Body.String())
	}

	transportFailure := errors.New("client disconnected")
	err = writeSSE(&failingResponseWriter{err: transportFailure}, core.Event{
		Type: core.EventStarted, DebateID: "dbt_test",
	})
	var transportErr *sseTransportError
	if !errors.As(err, &transportErr) || errors.As(err, &protocolErr) || !errors.Is(err, transportFailure) {
		t.Fatalf("failing writer error = %T %v, want transport-only classification", err, err)
	}
}

func TestHandleEventsTerminatesAndLogsReplayReadFailure(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	service := core.NewService(replayErrorStorage{}, core.NewHub(), unusedModerator{}, logger)
	server := New(service, logger, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/debates/dbt_test/events?after_seq=1", nil)
	request.SetPathValue("id", "dbt_test")
	recorder := httptest.NewRecorder()

	server.handleEvents(recorder, request)

	if !strings.Contains(logs.String(), "SSE replay: чтение протокола") || !strings.Contains(logs.String(), "replay failed") {
		t.Fatalf("replay failure was not logged: %s", logs.String())
	}
	if strings.Contains(recorder.Body.String(), "event:") {
		t.Fatalf("handler emitted live-looking events after incomplete replay: %q", recorder.Body.String())
	}
}

func TestHandleEventsTerminatesAndClassifiesHeartbeatWriteFailure(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	service := core.NewService(replayErrorStorage{messages: []core.Message{}}, core.NewHub(), unusedModerator{}, logger)
	server := New(service, logger, nil)
	server.heartbeatInterval = time.Millisecond
	request := httptest.NewRequest(http.MethodGet, "/api/debates/dbt_test/events", nil)
	request.SetPathValue("id", "dbt_test")
	writer := &heartbeatFailWriter{header: make(http.Header), err: errors.New("heartbeat disconnected")}
	done := make(chan struct{})
	go func() {
		server.handleEvents(writer, request)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not terminate after heartbeat write failure")
	}
	if !strings.Contains(logs.String(), "SSE transport: запись события") ||
		!strings.Contains(logs.String(), "scope=heartbeat") ||
		!strings.Contains(logs.String(), "heartbeat disconnected") {
		t.Fatalf("heartbeat transport failure was not classified and logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), "SSE protocol: отклонено событие") {
		t.Fatalf("heartbeat transport failure was misclassified as protocol: %s", logs.String())
	}
}

type failingResponseWriter struct {
	err error
}

func (*failingResponseWriter) Header() http.Header         { return make(http.Header) }
func (*failingResponseWriter) WriteHeader(int)             {}
func (w *failingResponseWriter) Write([]byte) (int, error) { return 0, w.err }

type heartbeatFailWriter struct {
	header http.Header
	err    error
}

func (w *heartbeatFailWriter) Header() http.Header       { return w.header }
func (*heartbeatFailWriter) WriteHeader(int)             {}
func (w *heartbeatFailWriter) Write([]byte) (int, error) { return 0, w.err }
func (*heartbeatFailWriter) Flush()                      {}

type replayErrorStorage struct {
	messages []core.Message
}

func (replayErrorStorage) CreateAgent(core.Agent, core.Credential, string) error { return nil }
func (replayErrorStorage) AgentByCredentialHash(string) (core.Agent, error) {
	return core.Agent{}, errors.New("unused")
}
func (replayErrorStorage) AgentByID(string) (core.Agent, error) {
	return core.Agent{}, errors.New("unused")
}
func (replayErrorStorage) CreateDebate(core.Debate) error { return nil }
func (replayErrorStorage) UpdateDebate(core.Debate) error { return nil }
func (replayErrorStorage) DeleteDebate(string) error      { return nil }
func (replayErrorStorage) GetDebate(id string) (core.Debate, error) {
	return core.Debate{ID: id, Mode: core.ModeModerator, Status: core.StatusRunning}, nil
}
func (replayErrorStorage) ListDebates(string, int) ([]core.Debate, error) { return nil, nil }
func (replayErrorStorage) ActiveDebates() ([]core.Debate, error)          { return nil, nil }
func (replayErrorStorage) AddParticipant(string, string, string, time.Time) error {
	return nil
}
func (replayErrorStorage) Participants(string) ([]core.Participant, error) { return nil, nil }
func (replayErrorStorage) AddMessage(core.Message) (int64, error)          { return 0, nil }
func (replayErrorStorage) Messages(string, int64) ([]core.Message, error) {
	return nil, errors.New("replay failed")
}
func (replayErrorStorage) AddModeratorTokens(string, int) error { return nil }

type unusedModerator struct{}

func (unusedModerator) Name() string { return "unused" }
func (unusedModerator) CheckRound(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return core.RoundSummary{}, core.ModerationUsage{}, errors.New("unused")
}
func (unusedModerator) Summary(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return core.RoundSummary{}, core.ModerationUsage{}, errors.New("unused")
}
func (unusedModerator) Verdict(
	context.Context, string, string, []int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	return core.ModerationVerdict{}, core.ModerationUsage{}, errors.New("unused")
}
