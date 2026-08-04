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
	"court/internal/store"
)

// MaxWaitSec — потолок long-poll ожидания очереди.
const MaxWaitSec = 120

// Server — REST-обвязка над ядром.
type Server struct {
	svc *core.Service
	log *slog.Logger
}

// New создаёт сервер API.
func New(svc *core.Service, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{svc: svc, log: log}
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
	mux.HandleFunc("GET /api/debates/{id}/messages", s.handleMessages)
	mux.HandleFunc("POST /api/debates/{id}/join", s.auth(s.handleJoin))
	mux.HandleFunc("POST /api/debates/{id}/start", s.auth(s.handleStart))
	mux.HandleFunc("GET /api/debates/{id}/turn", s.auth(s.handleTurn))
	mux.HandleFunc("POST /api/debates/{id}/messages", s.auth(s.handlePost))
	mux.HandleFunc("GET /api/debates/{id}/events", s.handleEvents)
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
	var req struct {
		Name    string `json:"name"`
		Persona string `json:"persona"`
	}
	if !decode(w, r, &req) {
		return
	}
	agent, key, err := s.svc.RegisterAgent(req.Name, req.Persona)
	if err != nil {
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
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
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
		if err == nil {
			for _, m := range msgs {
				msg := m
				writeSSE(w, core.Event{Type: core.EventMessage, DebateID: debateID,
					Round: m.Round, AgentID: m.SpeakerID, AgentName: m.SpeakerName, Message: &msg})
				seen = m.Seq
			}
			flusher.Flush()
		}
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			// Не дублируем сообщения, уже отданные реплеем.
			if ev.Message != nil && ev.Message.Seq <= seen {
				continue
			}
			writeSSE(w, ev)
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev core.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
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
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// Контекстные хелперы (используются MCP-обвязкой).

type ctxKey struct{}

// WithAgent кладёт аутентифицированного агента в контекст.
func WithAgent(ctx context.Context, a core.Agent) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// AgentFrom достаёт агента из контекста.
func AgentFrom(ctx context.Context) (core.Agent, bool) {
	a, ok := ctx.Value(ctxKey{}).(core.Agent)
	return a, ok
}
