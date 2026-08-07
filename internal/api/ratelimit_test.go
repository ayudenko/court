package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"court/internal/core"
	"court/internal/ratelimit"
	"court/internal/store"
)

// TestRegisterRateLimitRejectsBurstFromOneClient covers the only
// unauthenticated write: without a per-address limit anyone can create agent
// rows and credentials without bound.
func TestRegisterRateLimitRejectsBurstFromOneClient(t *testing.T) {
	const allowance = 3
	_, mux := newLimitedServer(t, ratelimit.Config{
		RegistrationsPerHourPerIP: allowance,
		ClientIPHeader:            "Fly-Client-IP",
	})

	for i := range allowance {
		if status, _, _ := register(mux, "203.0.113.7"); status != http.StatusCreated {
			t.Fatalf("registration %d: status = %d, want 201", i+1, status)
		}
	}

	status, body, header := register(mux, "203.0.113.7")
	if status != http.StatusTooManyRequests {
		t.Fatalf("registration over the allowance: status = %d, want 429", status)
	}
	if !strings.Contains(body["error"], "лимит") {
		t.Fatalf("rejection body does not name the limit: %q", body["error"])
	}
	if retryAfter := header.Get("Retry-After"); retryAfter == "" || retryAfter == "0" {
		t.Fatalf("Retry-After = %q, want a positive whole number of seconds", retryAfter)
	}

	// A limit that leaked across clients would be an outage, not a defence.
	if status, _, _ := register(mux, "198.51.100.4"); status != http.StatusCreated {
		t.Fatalf("unrelated client: status = %d, want 201", status)
	}
}

// TestCreateDebateRateLimitIsPerAgentKey guards the moderator budget: debate
// creation is what spends the service owner's LLM key.
func TestCreateDebateRateLimitIsPerAgentKey(t *testing.T) {
	const allowance = 2
	server, mux := newLimitedServer(t, ratelimit.Config{DebatesPerHourPerAgent: allowance})

	first := registerAgentKey(t, server.svc, "first")
	second := registerAgentKey(t, server.svc, "second")

	for i := range allowance {
		if status, body := createDebate(mux, first); status != http.StatusCreated {
			t.Fatalf("debate %d: status = %d body = %v, want 201", i+1, status, body)
		}
	}
	if status, body := createDebate(mux, first); status != http.StatusTooManyRequests {
		t.Fatalf("debate over the allowance: status = %d body = %v, want 429", status, body)
	}
	if status, body := createDebate(mux, second); status != http.StatusCreated {
		t.Fatalf("unrelated key: status = %d body = %v, want 201", status, body)
	}
}

// TestStreamLimitReleasesSlotOnDisconnect is the canary for the silent failure
// mode of a concurrency limit: a slot that is taken but never returned locks a
// client out of its own debates until the process restarts.
func TestStreamLimitReleasesSlotOnDisconnect(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{StreamsPerClient: 1})
	debateID := createDebateDirectly(t, server.svc)
	const clientIP = "203.0.113.9"

	opened, closeStream := holdStream(t, mux, debateID, clientIP)
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("held stream never opened")
	}

	blocked := finishedStream(mux, debateID, clientIP)
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent stream: status = %d, want 429", blocked.Code)
	}
	if retryAfter := blocked.Header().Get("Retry-After"); retryAfter != "" {
		t.Fatalf("concurrency rejection advertised Retry-After = %q; the wait depends on other clients "+
			"disconnecting, not on elapsed time", retryAfter)
	}
	// Another client must not inherit the first one's exhausted budget.
	if other := finishedStream(mux, debateID, "198.51.100.4"); other.Code != http.StatusOK {
		t.Fatalf("unrelated client: status = %d, want 200", other.Code)
	}

	closeStream()

	if reused := finishedStream(mux, debateID, clientIP); reused.Code != http.StatusOK {
		t.Fatalf("slot was not released after the stream ended: status = %d, want 200", reused.Code)
	}
}

// TestStreamLimitBindsAuthenticatedTrafficToItsAddressToo: an agent-only cap
// would let one host add a full allowance per registered agent and still reach
// the fronting proxy's connection limit, so an authenticated long-poll is
// charged to the address as well.
func TestStreamLimitBindsAuthenticatedTrafficToItsAddressToo(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{StreamsPerClient: 1})
	debateID := createDebateDirectly(t, server.svc)
	key := registerAgentKey(t, server.svc, "poller")

	opened, closeStream := holdStream(t, mux, debateID, "203.0.113.9")
	defer closeStream()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("held stream never opened")
	}

	if code := pollTurn(mux, debateID, key, "203.0.113.9"); code != http.StatusTooManyRequests {
		t.Fatalf("long-poll behind an address whose stream budget is spent: status = %d, want 429", code)
	}
	// The agent itself still has budget — only the shared address is exhausted.
	if code := pollTurn(mux, debateID, key, "198.51.100.4"); code == http.StatusTooManyRequests {
		t.Fatalf("the agent's own stream budget was consumed by an unrelated address: %d", code)
	}
}

func pollTurn(mux *http.ServeMux, debateID, key, clientIP string) int {
	request := httptest.NewRequest(http.MethodGet, "/api/debates/"+debateID+"/turn?wait_sec=0", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	request.RemoteAddr = clientIP + ":4444"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Code
}

// --- Хелперы ---

func newLimitedServer(t *testing.T, cfg ratelimit.Config) (*Server, *http.ServeMux) {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := core.NewService(database, core.NewHub(), unusedModerator{}, logger)
	server := New(service, logger, ratelimit.New(cfg))
	mux := http.NewServeMux()
	server.Routes(mux)
	return server, mux
}

func register(mux *http.ServeMux, clientIP string) (int, map[string]string, http.Header) {
	request := httptest.NewRequest(http.MethodPost, "/api/agents", strings.NewReader(`{"name":"agent"}`))
	request.Header.Set("Fly-Client-IP", clientIP)
	request.RemoteAddr = "10.0.0.1:1111"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	var body map[string]string
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body, recorder.Header()
}

func registerAgentKey(t *testing.T, service *core.Service, name string) string {
	t.Helper()
	_, key, err := service.RegisterAgent(name, "")
	if err != nil {
		t.Fatalf("RegisterAgent(%q): %v", name, err)
	}
	return key
}

func createDebate(mux *http.ServeMux, key string) (int, map[string]any) {
	request := httptest.NewRequest(http.MethodPost, "/api/debates",
		strings.NewReader(`{"question":"Стоит ли ограничивать частоту запросов?"}`))
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body
}

func createDebateDirectly(t *testing.T, service *core.Service) string {
	t.Helper()
	agent, _, err := service.RegisterAgent("organiser", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	view, err := service.CreateDebate(agent, core.CreateDebateParams{Question: "Нужны ли лимиты?"})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	return view.ID
}

// finishedStream runs one SSE request whose context is already cancelled, so the
// handler opens and closes the stream immediately. It answers whether a slot was
// available, without leaving one held.
func finishedStream(mux *http.ServeMux, debateID, clientIP string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/debates/"+debateID+"/events", nil)
	request.RemoteAddr = clientIP + ":3333"
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request.WithContext(ctx))
	return recorder
}

// holdStream keeps one SSE connection open until the returned close runs. The
// channel is closed once the handler has flushed its headers, which is the point
// at which the slot is certainly held.
func holdStream(t *testing.T, mux *http.ServeMux, debateID, clientIP string) (<-chan struct{}, func()) {
	t.Helper()
	opened := make(chan struct{})
	request := httptest.NewRequest(http.MethodGet, "/api/debates/"+debateID+"/events", nil)
	request.RemoteAddr = clientIP + ":2222"
	ctx, cancel := context.WithCancel(request.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(newStreamRecorder(opened), request.WithContext(ctx))
	}()
	return opened, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("held stream did not terminate after cancel")
		}
	}
}

// streamRecorder reports the moment the SSE handler flushes its headers.
type streamRecorder struct {
	header http.Header
	opened chan struct{}
	once   sync.Once
}

func newStreamRecorder(opened chan struct{}) *streamRecorder {
	return &streamRecorder{header: make(http.Header), opened: opened}
}

func (w *streamRecorder) Header() http.Header       { return w.header }
func (*streamRecorder) WriteHeader(int)             {}
func (*streamRecorder) Write(p []byte) (int, error) { return len(p), nil }
func (w *streamRecorder) Flush()                    { w.once.Do(func() { close(w.opened) }) }

// TestInvalidRequestsDoNotConsumeBudget: LLM agents routinely send malformed
// arguments, and a charge for work never done would let a buggy client lock
// itself out of a working operation for an hour.
func TestInvalidRequestsDoNotConsumeBudget(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{
		RegistrationsPerHourPerIP: 1,
		DebatesPerHourPerAgent:    1,
	})

	for range 5 {
		if status, _, _ := registerNamed(mux, "203.0.113.7", ""); status != http.StatusBadRequest {
			t.Fatalf("empty name: status = %d, want 400", status)
		}
	}
	if status, _, _ := register(mux, "203.0.113.7"); status != http.StatusCreated {
		t.Fatalf("valid registration after rejected ones: status = %d, want 201", status)
	}
	if status, _, _ := register(mux, "203.0.113.7"); status != http.StatusTooManyRequests {
		t.Fatalf("refunds inflated the registration allowance: status = %d, want 429", status)
	}

	key := registerAgentKey(t, server.svc, "organiser")
	for range 5 {
		if status := createDebateWithBody(mux, key, `{"question":""}`); status != http.StatusBadRequest {
			t.Fatalf("empty question: status = %d, want 400", status)
		}
	}
	if status, _ := createDebate(mux, key); status != http.StatusCreated {
		t.Fatalf("valid debate after rejected ones: status = %d, want 201", status)
	}
	if status, _ := createDebate(mux, key); status != http.StatusTooManyRequests {
		t.Fatalf("refunds inflated the debate allowance: status = %d, want 429", status)
	}
}

func registerNamed(mux *http.ServeMux, clientIP, name string) (int, map[string]string, http.Header) {
	request := httptest.NewRequest(http.MethodPost, "/api/agents",
		strings.NewReader(`{"name":"`+name+`"}`))
	request.Header.Set("Fly-Client-IP", clientIP)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	var body map[string]string
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body, recorder.Header()
}

func createDebateWithBody(mux *http.ServeMux, key, body string) int {
	request := httptest.NewRequest(http.MethodPost, "/api/debates", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Code
}

// TestUndecodableBodiesStayCharged: the refund covers domain validation, not a
// body the server could not read. Refunding those would make a flood of garbage
// against the only unauthenticated write free, which is the outcome the refund
// was explicitly not meant to buy.
func TestUndecodableBodiesStayCharged(t *testing.T) {
	const allowance = 3
	_, mux := newLimitedServer(t, ratelimit.Config{
		RegistrationsPerHourPerIP: allowance,
		ClientIPHeader:            "Fly-Client-IP",
	})

	for i := range allowance {
		if status := postRaw(mux, "/api/agents", "203.0.113.7", "", "not json at all"); status != http.StatusBadRequest {
			t.Fatalf("garbage body %d: status = %d, want 400", i+1, status)
		}
	}
	if status := postRaw(mux, "/api/agents", "203.0.113.7", "", "not json at all"); status != http.StatusTooManyRequests {
		t.Fatalf("a flood of unreadable bodies was free: status = %d, want 429", status)
	}
	if status, _, _ := register(mux, "203.0.113.7"); status != http.StatusTooManyRequests {
		t.Fatalf("valid registration after the budget was spent on garbage: status = %d, want 429", status)
	}
}

func postRaw(mux *http.ServeMux, path, clientIP, key, body string) int {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Fly-Client-IP", clientIP)
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Code
}
