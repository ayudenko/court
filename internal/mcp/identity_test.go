package mcp

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"court/internal/ratelimit"
)

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

func decodeMcp(t *testing.T, response string) mcpEnvelope {
	t.Helper()
	_, payload, ok := strings.Cut(response, "data: ")
	if !ok {
		t.Fatalf("MCP response has no data frame: %s", response)
	}
	var envelope mcpEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &envelope); err != nil {
		t.Fatalf("decode MCP envelope %s: %v", response, err)
	}
	return envelope
}

func isError(t *testing.T, response string) bool {
	t.Helper()
	envelope := decodeMcp(t, response)
	return envelope.Error != nil || envelope.Result.IsError
}

func toolPayload(t *testing.T, response string) map[string]any {
	t.Helper()
	envelope := decodeMcp(t, response)
	if envelope.Error != nil {
		t.Fatalf("MCP call failed: %s", envelope.Error.Message)
	}
	if len(envelope.Result.Content) == 0 {
		t.Fatalf("MCP result has no content: %s", response)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode tool payload %q: %v", envelope.Result.Content[0].Text, err)
	}
	return payload
}

// TestMCPWhoAmIReturnsStableIdentity proves that a configured credential can
// verify the public identity facts needed to avoid duplicate registration, and
// that REST rotation does not change that identity.
func TestMCPWhoAmIReturnsStableIdentity(t *testing.T) {
	mux, service := newSharedServer(t, ratelimit.Config{})
	agent, key, err := service.RegisterAgent("identity-check", "public persona")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if response := callTool(mux, "", "203.0.113.7", `{}`, "whoami"); !isError(t, response) {
		t.Fatalf("whoami accepted an argument-free call without configured auth: %s", response)
	}

	assertWhoAmI(t, callTool(mux, key, "203.0.113.7", `{}`, "whoami"), restMe(t, mux, key))
	credentials, err := service.Credentials(agent)
	if err != nil || len(credentials) != 1 {
		t.Fatalf("Credentials after registration: credentials=%v err=%v", credentials, err)
	}
	rotated, rotatedKey, err := service.IssueCredential(agent)
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	assertWhoAmI(t, callTool(mux, rotatedKey, "203.0.113.7", `{}`, "whoami"), restMe(t, mux, rotatedKey))
	if err := service.RevokeCredential(agent, credentials[0].ID); err != nil {
		t.Fatalf("RevokeCredential after replacement whoami: %v", err)
	}
	if status, body := callToolHTTPAt(mux, "/mcp", key, "203.0.113.7", `{}`, "whoami"); status != http.StatusUnauthorized {
		t.Fatalf("revoked old Bearer: status=%d, want 401; body=%s", status, body)
	}
	if rotated.AgentID != agent.ID {
		t.Fatalf("replacement credential belongs to %s, want %s", rotated.AgentID, agent.ID)
	}
	assertWhoAmI(t, callTool(mux, rotatedKey, "203.0.113.7", `{}`, "whoami"), restMe(t, mux, rotatedKey))
}

// TestMCPInvalidBearerCannotRegisterReplacement proves that a configured but
// invalid credential fails before tool dispatch, while anonymous requests also
// cannot create a replacement because registration is not an MCP capability.
func TestMCPInvalidBearerCannotRegisterReplacement(t *testing.T) {
	mux, _ := newSharedServer(t, ratelimit.Config{})
	status, body := callToolHTTPAt(mux, "/mcp", "ck_invalid", "203.0.113.7",
		`{"name":"must-not-exist"}`, "register_agent")
	if status != http.StatusUnauthorized || !strings.Contains(body, "неверный API-ключ") {
		t.Fatalf("invalid Bearer was not rejected before dispatch: status=%d body=%s", status, body)
	}
	if anonymous := callTool(mux, "", "203.0.113.7",
		`{"name":"must-not-exist"}`, "register_agent"); !isError(t, anonymous) {
		t.Fatalf("anonymous MCP registration unexpectedly exists: %s", anonymous)
	}
	for name, values := range map[string][]string{
		"empty":                {""},
		"whitespace":           {" \t "},
		"wrong scheme":         {"Basic eDp5"},
		"bearer without token": {"Bearer"},
		"coalesced bearers":    {"Bearer ck_invalid, Bearer ck_other"},
		"empty then invalid":   {"", "Bearer ck_invalid"},
		"two bearers":          {"Bearer ck_invalid", "Bearer ck_other"},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_debates","arguments":{}}}`))
			request.Header["Authorization"] = values
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized ||
				!strings.Contains(recorder.Body.String(), "неверный API-ключ") {
				t.Fatalf("ambiguous Authorization dispatched: status=%d body=%s",
					recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestMCPToolSchemasKeepCredentialsOutOfArguments guards both sides of the
// prompt-injection boundary: exact closed schemas and no credential-management
// capability anywhere on the MCP surface.
func TestMCPToolSchemasKeepCredentialsOutOfArguments(t *testing.T) {
	mux, _ := newSharedServer(t, ratelimit.Config{})
	wantProperties := map[string][]string{
		"whoami":        {},
		"list_debates":  {"limit", "status"},
		"create_debate": {"description", "mode", "observer", "prep_time_sec", "question", "rounds", "stance", "turn_timeout_sec"},
		"join_debate":   {"debate_id", "stance"},
		"start_debate":  {"debate_id"},
		"get_debate":    {"debate_id"},
		"wait_for_turn": {"debate_id", "wait_sec"},
		"post_argument": {"debate_id", "support_agent_id", "text"},
	}
	assertToolProfile(t, mux, wantProperties)

	for _, arguments := range []string{
		`{"api_key":"ck_secret"}`,
		`{"credential":"ck_secret"}`,
		`{"bearer_token":"ck_secret"}`,
	} {
		response := callTool(mux, "", "203.0.113.8", arguments, "whoami")
		if !isError(t, response) || !strings.Contains(response, "additional properties") {
			t.Errorf("whoami accepted credential-shaped extra arguments %s: %s", arguments, response)
		}
	}

	for _, tool := range []string{
		"register_agent", "issue_credential", "list_credentials", "revoke_credential", "delete_debate",
	} {
		response := callTool(mux, "", "203.0.113.9", `{}`, tool)
		if !isError(t, response) {
			t.Errorf("MCP unexpectedly exposed credential tool %s: %s", tool, response)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp/credentials",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("removed /mcp/credentials endpoint returned %d, want 404", recorder.Code)
	}
}

func assertToolProfile(t *testing.T, mux *http.ServeMux, wantProperties map[string][]string) {
	t.Helper()
	body := callMCPMethod(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_, frame, ok := strings.Cut(body, "data: ")
	if !ok {
		t.Fatalf("tools/list response has no data frame: %s", body)
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(frame)), &envelope); err != nil {
		t.Fatalf("decode tools/list: %v\n%s", err, body)
	}
	for _, tool := range envelope.Result.Tools {
		want, known := wantProperties[tool.Name]
		if !known {
			t.Errorf("unexpected MCP tool %q", tool.Name)
			continue
		}
		properties, _ := tool.InputSchema["properties"].(map[string]any)
		got := make([]string, 0, len(properties))
		for name := range properties {
			got = append(got, name)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("tool %s input properties = %v, want reviewed allowlist %v", tool.Name, got, want)
		}
		if additional, present := tool.InputSchema["additionalProperties"]; !present || additional != false {
			t.Errorf("tool %s schema additionalProperties = %v (present=%v), want false", tool.Name, additional, present)
		}
		delete(wantProperties, tool.Name)
	}
	if len(wantProperties) != 0 {
		missing := slices.Collect(maps.Keys(wantProperties))
		slices.Sort(missing)
		t.Fatalf("reviewed MCP tools are absent: %v", missing)
	}
}

func assertWhoAmI(t *testing.T, response string, rest map[string]any) {
	t.Helper()
	who := toolPayload(t, response)
	for mcpField, restField := range map[string]string{
		"agent_id": "id", "name": "name", "persona": "persona", "created_at": "created_at",
	} {
		if who[mcpField] != rest[restField] {
			t.Fatalf("whoami %s = %v, REST %s = %v; whoami=%v REST=%v",
				mcpField, who[mcpField], restField, rest[restField], who, rest)
		}
	}
}

func restMe(t *testing.T, mux *http.ServeMux, key string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/agents/me", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/agents/me: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var agent map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &agent); err != nil {
		t.Fatalf("decode GET /api/agents/me: %v", err)
	}
	return agent
}

func callMCPMethod(t *testing.T, mux *http.ServeMux, body string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Fly-Client-IP", "203.0.113.7")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Body.String()
}
