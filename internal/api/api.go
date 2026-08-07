// Package api — REST API и SSE-поток событий дебатов.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"court/internal/core"
	"court/internal/ratelimit"
	"court/internal/store"
)

// MaxWaitSec — потолок long-poll ожидания очереди.
const MaxWaitSec = 120

// Server — REST-обвязка над ядром.
type Server struct {
	svc               *core.Service
	log               *slog.Logger
	limiter           *ratelimit.Limiter
	heartbeatInterval time.Duration
}

// New создаёт сервер API. Нулевой limiter означает «без лимитов».
func New(svc *core.Service, log *slog.Logger, limiter *ratelimit.Limiter) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{svc: svc, log: log, limiter: limiter, heartbeatInterval: 25 * time.Second}
}

// Routes регистрирует маршруты API на mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("POST /api/agents", s.handleRegister)
	mux.HandleFunc("GET /api/agents/me", s.auth(s.handleMe))

	mux.HandleFunc("POST /api/debates", s.auth(s.handleCreateDebate))
	mux.HandleFunc("GET /api/debates", s.handleListDebates)
	mux.HandleFunc("GET /api/debates/{id}", s.handleGetDebate)
	mux.HandleFunc("DELETE /api/debates/{id}", s.auth(s.handleDeleteDebate))
	mux.HandleFunc("GET /api/debates/{id}/messages", s.handleMessages)
	mux.HandleFunc("POST /api/debates/{id}/join", s.auth(s.handleJoin))
	mux.HandleFunc("POST /api/debates/{id}/start", s.auth(s.handleStart))
	mux.HandleFunc("GET /api/debates/{id}/turn", s.auth(s.limitAgentStream(s.handleTurn)))
	mux.HandleFunc("POST /api/debates/{id}/messages", s.auth(s.handlePost))
	mux.HandleFunc("GET /api/debates/{id}/events", s.limitIPStream(s.handleEvents))
}

// --- Лимиты ---
//
// Лимиты живут на границе HTTP, а не в ядре: ключ лимита — адрес клиента или
// стабильный agent_id, и ни то ни другое ядру не известно.
// См. docs/adr/0003-http-rate-limiting.md.

// refundInvalid возвращает потраченный лимит, если ядро отклонило запрос по
// валидации: ничего не создано, модератор не тронут, а агент, который шлёт
// кривые аргументы, иначе запирает сам себя на час. Нераспознанное тело
// (`decode`) остаётся оплаченным: иначе поток мусора на неаутентифицированную
// регистрацию бесплатен, а лимит на ней — единственная защита.
func refundInvalid(grant *ratelimit.Grant, err error) {
	if errors.Is(err, core.ErrValidation) {
		grant.Refund()
	}
}

// limitAgentStream ограничивает одновременные long-poll агента.
func (s *Server) limitAgentStream(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, agent core.Agent) {
		release, err := s.limiter.AcquireStream(agent.ID, s.limiter.ClientIP(r))
		defer release()
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r, agent)
	}
}

// limitIPStream ограничивает одновременные SSE-подписки: поток событий открыт
// всем, поэтому ключом может быть только адрес.
func (s *Server) limitIPStream(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, err := s.limiter.AcquireStream("", s.limiter.ClientIP(r))
		defer release()
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r)
	}
}

// --- Аутентификация ---

type authedHandler func(w http.ResponseWriter, r *http.Request, agent core.Agent)

// AgentFromRequest аутентифицирует запрос по заголовку Authorization.
// Используется и REST-обвязкой, и MCP-сервером.
func AgentFromRequest(svc *core.Service, r *http.Request) (core.Agent, error) {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return core.Agent{}, core.ErrUnauthorized
	}
	return svc.Authenticate(strings.TrimSpace(token))
}

func (s *Server) auth(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent, err := AgentFromRequest(s.svc, r)
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r, agent)
	}
}

// --- Хендлеры ---

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	// Лимит берётся до разбора тела: иначе поток нечитаемых запросов на
	// единственную неаутентифицированную запись ничего не стоит.
	grant, err := s.limiter.AllowRegistration(s.limiter.ClientIP(r))
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Name    string `json:"name"`
		Persona string `json:"persona"`
	}
	if !decode(w, r, &req) {
		return
	}
	agent, key, err := s.svc.RegisterAgent(req.Name, req.Persona)
	if err != nil {
		refundInvalid(&grant, err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent":   agent,
		"api_key": key,
		"note":    "Сохраните api_key: он показывается только один раз.",
	})
}

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, agent core.Agent) {
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) handleCreateDebate(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	var req struct {
		Question       string `json:"question"`
		Description    string `json:"description"`
		Stance         string `json:"stance"`
		Mode           string `json:"mode"`
		Rounds         int    `json:"rounds"`
		TurnTimeoutSec int    `json:"turn_timeout_sec"`
		PrepTimeSec    int    `json:"prep_time_sec"`
		Observer       bool   `json:"observer"`
	}
	grant, err := s.limiter.AllowDebateCreation(agent.ID, s.limiter.ClientIP(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := s.svc.CreateDebate(agent, core.CreateDebateParams{
		Question:       req.Question,
		Description:    req.Description,
		Stance:         req.Stance,
		Mode:           core.DebateMode(req.Mode),
		Rounds:         req.Rounds,
		TurnTimeoutSec: req.TurnTimeoutSec,
		PrepTimeSec:    req.PrepTimeSec,
		Observer:       req.Observer,
	})
	if err != nil {
		refundInvalid(&grant, err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleDeleteDebate(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	if err := s.svc.DeleteDebate(agent, r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDebates(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.svc.ListDebates(r.URL.Query().Get("status"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"debates": list})
}

func (s *Server) handleGetDebate(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetDebate(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	afterSeq, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
	msgs, err := s.svc.Messages(r.PathValue("id"), afterSeq)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	var req struct {
		Stance string `json:"stance"`
	}
	if r.ContentLength > 0 && !decode(w, r, &req) {
		return
	}
	v, err := s.svc.JoinDebate(agent, r.PathValue("id"), req.Stance)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	v, err := s.svc.StartDebate(agent, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	waitSec, _ := strconv.Atoi(r.URL.Query().Get("wait_sec"))
	waitSec = min(max(waitSec, 0), MaxWaitSec)
	st, err := s.svc.WaitTurn(r.Context(), agent, r.PathValue("id"), time.Duration(waitSec)*time.Second)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	var req struct {
		Text           string `json:"text"`
		SupportAgentID string `json:"support_agent_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	msg, err := s.svc.PostArgument(r.Context(), agent, r.PathValue("id"), req.Text, req.SupportAgentID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// handleEvents — SSE-поток: опционально реплей протокола с after_seq, затем live.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	debateID := r.PathValue("id")
	if _, err := s.svc.GetDebate(debateID); err != nil {
		writeError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, errors.New("SSE не поддерживается"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Отправляем заголовки сразу: клиент видит, что подключение принято,
	// не дожидаясь первого события или heartbeat.
	flusher.Flush()

	// Подписка до реплея, чтобы не потерять события между ними.
	ch := s.svc.Subscribe(debateID)
	defer s.svc.Unsubscribe(debateID, ch)

	seen := int64(0)
	if v := r.URL.Query().Get("after_seq"); v != "" {
		afterSeq, _ := strconv.ParseInt(v, 10, 64)
		msgs, err := s.svc.Messages(debateID, afterSeq)
		if err != nil {
			s.log.Error("SSE replay: чтение протокола", "debate", debateID, "err", err)
			return
		}
		for _, m := range msgs {
			msg := m
			if err := writeSSE(w, core.Event{Type: core.EventMessage, DebateID: debateID,
				Round: m.Round, AgentID: m.SpeakerID, AgentName: m.SpeakerName, Message: &msg}); err != nil {
				s.logSSEError("replay", debateID, core.EventMessage, err)
				return
			}
			seen = m.Seq
		}
		flusher.Flush()
	}

	heartbeat := time.NewTicker(s.heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if err := writeSSEHeartbeat(w); err != nil {
				s.logSSEError("heartbeat", debateID, "heartbeat", err)
				return
			}
			flusher.Flush()
		case ev := <-ch:
			// Не дублируем сообщения, уже отданные реплеем.
			if ev.Message != nil && ev.Message.Seq <= seen {
				continue
			}
			if err := writeSSE(w, ev); err != nil {
				s.logSSEError("live", debateID, ev.Type, err)
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) logSSEError(scope, debateID, eventType string, err error) {
	var protocolErr *sseProtocolError
	if errors.As(err, &protocolErr) {
		s.log.Error("SSE protocol: отклонено событие", "scope", scope, "debate", debateID, "type", eventType, "err", err)
		return
	}
	s.log.Warn("SSE transport: запись события", "scope", scope, "debate", debateID, "type", eventType, "err", err)
}

type sseProtocolError struct{ err error }

func (e *sseProtocolError) Error() string { return "marshal SSE event: " + e.err.Error() }
func (e *sseProtocolError) Unwrap() error { return e.err }

type sseTransportError struct{ err error }

func (e *sseTransportError) Error() string { return "write SSE event: " + e.err.Error() }
func (e *sseTransportError) Unwrap() error { return e.err }

func writeSSE(w http.ResponseWriter, ev core.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return &sseProtocolError{err: err}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
		return &sseTransportError{err: err}
	}
	return nil
}

func writeSSEHeartbeat(w http.ResponseWriter) error {
	if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
		return &sseTransportError{err: err}
	}
	return nil
}

// --- Утилиты ---

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, fmt.Errorf("%w: некорректный JSON: %v", core.ErrValidation, err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError отдаёт ошибку в общем для сервиса формате. Экспортирован для
// MCP-обвязки, которая отклоняет запрос до JSON-RPC-слоя.
func WriteError(w http.ResponseWriter, err error) { writeError(w, err) }

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, core.ErrValidation):
		status = http.StatusBadRequest
	case errors.Is(err, core.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, core.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, core.ErrNotYourTurn), errors.Is(err, core.ErrBadState):
		status = http.StatusConflict
	case errors.Is(err, ratelimit.ErrLimited):
		status = http.StatusTooManyRequests
		// Заголовок ставится до WriteHeader внутри writeJSON.
		var limitErr *ratelimit.LimitError
		if errors.As(err, &limitErr) && limitErr.RetryAfterSeconds() > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(limitErr.RetryAfterSeconds()))
		}
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// Контекстные хелперы (используются MCP-обвязкой).

type ctxKey struct{}

type clientIPKey struct{}

// WithAgent кладёт аутентифицированного агента в контекст.
func WithAgent(ctx context.Context, a core.Agent) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// AgentFrom достаёт агента из контекста.
func AgentFrom(ctx context.Context) (core.Agent, bool) {
	a, ok := ctx.Value(ctxKey{}).(core.Agent)
	return a, ok
}

// WithClientIP кладёт в контекст адрес клиента, разрешённый лимитером.
// У MCP-инструментов нет http.Request, а ключ лимита им нужен.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFrom достаёт адрес клиента из контекста.
func ClientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}
