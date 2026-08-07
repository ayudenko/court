package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"court/internal/ratelimit"
)

// TestCredentialRotationWorksOverMcp — большинство участников подключается по
// MCP, поэтому путь ротации, доступный только в REST, не был бы путём ротации.
// Проверяется полный цикл: выпустить, аутентифицироваться новым ключом,
// отозвать старый, убедиться что он больше не работает.
func TestCredentialRotationWorksOverMcp(t *testing.T) {
	mux, service := newSharedServer(t, ratelimit.Config{})
	agent, firstKey, err := service.RegisterAgent("mcp-rotator", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	listed := callTool(mux, firstKey, "203.0.113.7", `{}`, "list_credentials")
	firstID := onlyCredentialID(t, listed)

	issued := toolPayload(t, callTool(mux, firstKey, "203.0.113.7", `{}`, "issue_credential"))
	secondKey, _ := issued["api_key"].(string)
	if secondKey == "" || secondKey == firstKey {
		t.Fatalf("issue_credential returned no new key: %v", issued)
	}
	credential, ok := issued["credential"].(map[string]any)
	if !ok || credential["agent_id"] != agent.ID {
		t.Fatalf("issued credential does not belong to %q: %v", agent.ID, issued)
	}

	// Новый ключ аутентифицируется как тот же агент.
	me := toolPayload(t, callTool(mux, secondKey, "203.0.113.7", `{}`, "list_credentials"))
	if !strings.Contains(fmt.Sprint(me), agent.ID) {
		t.Fatalf("new key does not resolve to %q: %v", agent.ID, me)
	}

	revoked := callTool(mux, secondKey, "203.0.113.7", `{"credential_id":"`+firstID+`"}`, "revoke_credential")
	if isError(t, revoked) || toolPayload(t, revoked)["revoked"] != firstID {
		t.Fatalf("revoke_credential: %s", revoked)
	}
	if after := callTool(mux, firstKey, "203.0.113.7", `{}`, "list_credentials"); !isError(t, after) {
		t.Fatalf("revoked key still works over MCP: %s", after)
	}
	if _, err := service.Authenticate(firstKey); err == nil {
		t.Fatal("revoked key still authenticates")
	}
}

// TestMcpRefusesRevokingTheLastCredential — тот же инвариант, что и в REST:
// путь к необратимой потере идентичности не должен открываться сменой
// транспорта.
func TestMcpRefusesRevokingTheLastCredential(t *testing.T) {
	mux, service := newSharedServer(t, ratelimit.Config{})
	_, key, err := service.RegisterAgent("mcp-solo", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	only := onlyCredentialID(t, callTool(mux, key, "203.0.113.7", `{}`, "list_credentials"))

	response := callTool(mux, key, "203.0.113.7", `{"credential_id":"`+only+`"}`, "revoke_credential")
	if !isError(t, response) {
		t.Fatalf("MCP allowed revoking the last credential: %s", response)
	}
	if _, err := service.Authenticate(key); err != nil {
		t.Fatalf("refused revocation still disabled the key: %v", err)
	}
}

// TestMcpCredentialIssueSharesTheRestBudget — смена транспорта не должна давать
// второй бюджет на ту же операцию.
func TestMcpCredentialIssueSharesTheRestBudget(t *testing.T) {
	mux, service := newSharedServer(t, ratelimit.Config{
		CredentialsPerHourPerAgent: 1,
		ClientIPHeader:             "Fly-Client-IP",
	})
	_, key, err := service.RegisterAgent("mcp-budget", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if body := callTool(mux, key, "203.0.113.7", `{}`, "issue_credential"); isLimited(body) {
		t.Fatalf("first issue over MCP was rejected: %s", body)
	}
	if status := restIssueCredential(mux, key); status != 429 {
		t.Fatalf("REST issue after the MCP budget was spent: status = %d, want 429", status)
	}
}

// mcpEnvelope — минимальная форма ответа JSON-RPC, которую разбирают тесты.
type mcpEnvelope struct {
	Result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// decodeMcp разбирает SSE-кадр вокруг ответа JSON-RPC.
func decodeMcp(t *testing.T, mcpResponse string) mcpEnvelope {
	t.Helper()
	_, payload, ok := strings.Cut(mcpResponse, "data: ")
	if !ok {
		t.Fatalf("MCP response has no data frame: %s", mcpResponse)
	}
	var envelope mcpEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &envelope); err != nil {
		t.Fatalf("decode MCP envelope %s: %v", mcpResponse, err)
	}
	return envelope
}

// isError сообщает, что MCP-вызов вернул ошибку, а не результат.
func isError(t *testing.T, mcpResponse string) bool {
	t.Helper()
	envelope := decodeMcp(t, mcpResponse)
	return envelope.Error != nil || envelope.Result.IsError
}

// toolPayload разбирает JSON, который инструмент отдаёт текстовым контентом.
func toolPayload(t *testing.T, mcpResponse string) map[string]any {
	t.Helper()
	envelope := decodeMcp(t, mcpResponse)
	if envelope.Error != nil {
		t.Fatalf("MCP call failed: %s", envelope.Error.Message)
	}
	if len(envelope.Result.Content) == 0 {
		t.Fatalf("MCP result has no content: %s", mcpResponse)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode tool payload %q: %v", envelope.Result.Content[0].Text, err)
	}
	return payload
}

// onlyCredentialID достаёт идентификатор единственного ключа агента. Вызывать
// до выпуска следующих: соответствие «ключ → credential» живёт только в
// хранилище, и опора на порядок сортировки сделала бы тест зависимым от
// разрешения часов.
func onlyCredentialID(t *testing.T, mcpResponse string) string {
	t.Helper()
	list, ok := toolPayload(t, mcpResponse)["credentials"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("agent does not have exactly one credential: %s", mcpResponse)
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected credential shape: %v", list[0])
	}
	id, _ := first["id"].(string)
	if id == "" {
		t.Fatalf("credential has no id: %v", first)
	}
	return id
}

func restIssueCredential(mux *http.ServeMux, key string) int {
	request := httptest.NewRequest(http.MethodPost, "/api/agents/me/credentials", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Fly-Client-IP", "203.0.113.7")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Code
}
