package mcp

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"court/internal/api"
	"court/internal/core"
	"court/internal/ratelimit"
	"court/internal/store"
)

// TestLimitsAreSharedBetweenRestAndMcp: REST and MCP expose the same
// operations, so a per-transport budget would be no budget at all — a caller
// would simply alternate.
func TestLimitsAreSharedBetweenRestAndMcp(t *testing.T) {
	mux, service := newSharedServer(t, ratelimit.Config{
		RegistrationsPerHourPerIP: 1,
		DebatesPerHourPerAgent:    1,
		ClientIPHeader:            "Fly-Client-IP",
	})

	// Registration: spend the address budget over REST, then try MCP.
	if status := restRegister(mux, "203.0.113.7"); status != http.StatusCreated {
		t.Fatalf("REST registration: status = %d, want 201", status)
	}
	if body := callTool(mux, "", "203.0.113.7", `{"name":"mcp"}`, "register_agent"); !isLimited(body) {
		t.Fatalf("MCP registration was not charged to the address budget spent over REST: %s", body)
	}
	// The limit is per address, not global.
	if body := callTool(mux, "", "198.51.100.4", `{"name":"mcp"}`, "register_agent"); isLimited(body) {
		t.Fatalf("unrelated address inherited an exhausted budget: %s", body)
	}

	// Debate creation: spend the agent budget over MCP, then try REST.
	_, key, err := service.RegisterAgent("organiser", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	created := callTool(mux, key, "203.0.113.7", `{"question":"Нужны ли лимиты?"}`, "create_debate")
	if isLimited(created) {
		t.Fatalf("first MCP debate was rejected: %s", created)
	}
	if status := restCreateDebate(mux, key); status != http.StatusTooManyRequests {
		t.Fatalf("REST debate after the MCP budget was spent: status = %d, want 429", status)
	}
}

func newSharedServer(t *testing.T, cfg ratelimit.Config) (*http.ServeMux, *core.Service) {
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
	service := core.NewService(database, core.NewHub(), nil, logger)
	limiter := ratelimit.New(cfg)
	mux := http.NewServeMux()
	api.New(service, logger, limiter).Routes(mux)
	mux.Handle("/mcp", Handler(service, "test", limiter))
	return mux, service
}

func callTool(mux *http.ServeMux, key, clientIP, arguments, tool string) string {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool +
		`","arguments":` + arguments + `}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Fly-Client-IP", clientIP)
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Body.String()
}

func isLimited(mcpResponse string) bool {
	return strings.Contains(mcpResponse, ratelimit.ErrLimited.Error())
}

func restRegister(mux *http.ServeMux, clientIP string) int {
	request := httptest.NewRequest(http.MethodPost, "/api/agents", strings.NewReader(`{"name":"rest"}`))
	request.Header.Set("Fly-Client-IP", clientIP)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Code
}

func restCreateDebate(mux *http.ServeMux, key string) int {
	request := httptest.NewRequest(http.MethodPost, "/api/debates",
		strings.NewReader(`{"question":"Нужны ли лимиты?"}`))
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Code
}

// TestAbandonedStreamReleasesItsSlotWithinTheBound is the canary for the leak
// the transport-level slot introduces: the SDK holds a stream until its context
// is cancelled and sends no keepalive, so a connection dropped without a close
// (laptop sleep, NAT eviction, a killed client) would otherwise hold the slot
// until the OS gives up on the socket — locking an agent out of /mcp entirely.
func TestAbandonedStreamReleasesItsSlotWithinTheBound(t *testing.T) {
	const bound = 300 * time.Millisecond
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = database.Close() }()
	service := core.NewService(database, core.NewHub(), nil, logger)
	limiter := ratelimit.New(ratelimit.Config{StreamsPerClient: 1})
	server := httptest.NewServer(Handler(service, "test", limiter, WithMaxRequestDuration(bound)))
	defer server.Close()

	// Open a stream and abandon it without closing the body.
	request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(subscribeFrame))
	if err != nil {
		t.Fatalf("subscription request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "subscriptions/listen")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open subscription: %v", err)
	}
	buf := make([]byte, 128)
	if _, err := response.Body.Read(buf); err != nil {
		t.Fatalf("subscription never acknowledged: %v", err)
	}

	if status := probeToolsList(t, server.URL); status != http.StatusTooManyRequests {
		t.Fatalf("while the stream is open: status = %d, want 429", status)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if probeToolsList(t, server.URL) != http.StatusTooManyRequests {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot of an abandoned stream was still held 5s after a %v bound", bound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// probeToolsList issues a well-formed short MCP call and reports its status;
// 429 means the client currently holds no free stream slot.
func probeToolsList(t *testing.T, baseURL string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

const subscribeFrame = `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":` +
	`{"notifications":{"toolsListChanged":true},"_meta":` +
	`{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientCapabilities":{}}}}`
