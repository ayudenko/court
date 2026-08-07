package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"court/internal/core"
	"court/internal/ratelimit"
)

// TestCredentialRotationOverHTTP проходит опубликованный путь ротации целиком:
// выпустить новый ключ, убедиться что он работает, отозвать старый. Ядро
// проверяет тот же инвариант на своих типах; здесь проверяется, что он
// действительно доступен клиенту и отражён в статусах.
func TestCredentialRotationOverHTTP(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	agent, firstKey, err := server.svc.RegisterAgent("rotator", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	// Пока ключ один, отзывать нечего: 409, а не потеря идентичности.
	first := onlyCredential(t, mux, firstKey)
	if status, body := revokeCredential(mux, firstKey, first); status != http.StatusConflict {
		t.Fatalf("revoking the last credential: status = %d body = %v, want 409", status, body)
	}

	status, issued := issueCredential(mux, firstKey)
	if status != http.StatusCreated {
		t.Fatalf("issue: status = %d body = %v, want 201", status, issued)
	}
	secondKey, _ := issued["api_key"].(string)
	if secondKey == "" || secondKey == firstKey {
		t.Fatalf("issued api_key = %q, want a new secret", secondKey)
	}
	if me := whoami(t, mux, secondKey); me != agent.ID {
		t.Fatalf("new key authenticated as %q, want the same agent %q", me, agent.ID)
	}

	if status, body := revokeCredential(mux, secondKey, first); status != http.StatusNoContent {
		t.Fatalf("revoke: status = %d body = %v, want 204", status, body)
	}
	if status := whoamiStatus(mux, firstKey); status != http.StatusUnauthorized {
		t.Fatalf("revoked key: status = %d, want 401", status)
	}
	if me := whoami(t, mux, secondKey); me != agent.ID {
		t.Fatalf("surviving key stopped working, got %q", me)
	}

	// Отзыв уже отозванного ключа неотличим от отзыва несуществующего.
	revokedStatus, revokedBody := revokeCredential(mux, secondKey, first)
	unknownStatus, unknownBody := revokeCredential(mux, secondKey, "crd_does_not_exist")
	if revokedStatus != http.StatusNotFound || unknownStatus != http.StatusNotFound {
		t.Fatalf("statuses = %d and %d, want 404 for both", revokedStatus, unknownStatus)
	}
	if fmt.Sprint(revokedBody) != fmt.Sprint(unknownBody) {
		t.Fatalf("a revoked id is distinguishable from an unknown one: %v vs %v", revokedBody, unknownBody)
	}
}

// TestCredentialEndpointsAreScopedToTheAuthenticatedAgent — ключи чужого агента
// нельзя ни увидеть, ни отозвать, а ответ не подтверждает их существование.
func TestCredentialEndpointsAreScopedToTheAuthenticatedAgent(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	owner, ownerKey, err := server.svc.RegisterAgent("owner", "")
	if err != nil {
		t.Fatalf("RegisterAgent(owner): %v", err)
	}
	ownerCredential := onlyCredential(t, mux, ownerKey)
	if status, body := issueCredential(mux, ownerKey); status != http.StatusCreated {
		t.Fatalf("issue for owner: status = %d body = %v", status, body)
	}

	_, intruderKey, err := server.svc.RegisterAgent("intruder", "")
	if err != nil {
		t.Fatalf("RegisterAgent(intruder): %v", err)
	}

	foreignStatus, foreignBody := revokeCredential(mux, intruderKey, ownerCredential)
	unknownStatus, unknownBody := revokeCredential(mux, intruderKey, "crd_does_not_exist")
	if foreignStatus != http.StatusNotFound {
		t.Fatalf("revoking another agent's credential: status = %d body = %v, want 404", foreignStatus, foreignBody)
	}
	if fmt.Sprint(foreignBody) != fmt.Sprint(unknownBody) || unknownStatus != foreignStatus {
		t.Fatalf("endpoint is an existence oracle: %d %v vs %d %v",
			foreignStatus, foreignBody, unknownStatus, unknownBody)
	}
	if me := whoami(t, mux, ownerKey); me != owner.ID {
		t.Fatalf("owner's key was revoked by another agent, whoami = %q", me)
	}
	for _, credential := range listCredentials(t, mux, intruderKey) {
		if credential["agent_id"] != nil && credential["id"] == ownerCredential {
			t.Fatalf("intruder sees the owner's credential: %v", credential)
		}
	}

	// Без ключа операции недоступны вовсе.
	request := httptest.NewRequest(http.MethodPost, "/api/agents/me/credentials", nil)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated issue: status = %d, want 401", recorder.Code)
	}
}

// TestCredentialResponsesCarryNoStoredSecret — выпуск отдаёт ключ ровно один
// раз; ни список, ни повторное чтение его не возвращают, и хэш наружу не
// выходит ни в каком виде.
func TestCredentialResponsesCarryNoStoredSecret(t *testing.T) {
	server, mux := newLimitedServer(t, ratelimit.Config{})
	_, firstKey, err := server.svc.RegisterAgent("inspected", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	status, issued := issueCredential(mux, firstKey)
	if status != http.StatusCreated {
		t.Fatalf("issue: status = %d body = %v", status, issued)
	}
	secondKey, _ := issued["api_key"].(string)

	credential, ok := issued["credential"].(map[string]any)
	if !ok {
		t.Fatalf("issue response has no credential object: %v", issued)
	}
	for _, field := range []string{"key_hash", "keyHash", "hash", "api_key", "key"} {
		if _, present := credential[field]; present {
			t.Fatalf("credential object exposes %q: %v", field, credential)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/agents/me/credentials", nil)
	request.Header.Set("Authorization", "Bearer "+firstKey)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for name, secret := range map[string]string{"registration key": firstKey, "issued key": secondKey} {
		if strings.Contains(body, secret) {
			t.Fatalf("credential listing contains the %s: %s", name, body)
		}
	}
	if !strings.Contains(body, credential["id"].(string)) {
		t.Fatalf("credential listing is missing the issued credential: %s", body)
	}
}

// TestCredentialIssueRateLimitIsPerAgentKey — выпуск ключей это
// аутентифицированная запись, создающая долговечные строки, поэтому он живёт
// внутри модели лимитов ADR 0003 и считается по стабильному agent_id.
func TestCredentialIssueRateLimitIsPerAgentKey(t *testing.T) {
	const allowance = 2
	server, mux := newLimitedServer(t, ratelimit.Config{CredentialsPerHourPerAgent: allowance})
	_, firstKey, err := server.svc.RegisterAgent("first", "")
	if err != nil {
		t.Fatalf("RegisterAgent(first): %v", err)
	}
	_, secondKey, err := server.svc.RegisterAgent("second", "")
	if err != nil {
		t.Fatalf("RegisterAgent(second): %v", err)
	}

	extraKeys := make([]string, 0, allowance)
	for i := range allowance {
		status, body := issueCredential(mux, firstKey)
		if status != http.StatusCreated {
			t.Fatalf("issue %d: status = %d body = %v, want 201", i+1, status, body)
		}
		key, _ := body["api_key"].(string)
		extraKeys = append(extraKeys, key)
	}
	if status, body := issueCredential(mux, firstKey); status != http.StatusTooManyRequests {
		t.Fatalf("issue over the allowance: status = %d body = %v, want 429", status, body)
	}
	// Второй ключ того же агента не даёт второго бюджета: счёт идёт по
	// стабильному agent_id, а не по предъявленному credential.
	if status, body := issueCredential(mux, extraKeys[0]); status != http.StatusTooManyRequests {
		t.Fatalf("issue under a sibling credential: status = %d body = %v, want 429", status, body)
	}
	if status, body := issueCredential(mux, secondKey); status != http.StatusCreated {
		t.Fatalf("unrelated agent: status = %d body = %v, want 201", status, body)
	}
}

// TestCapRejectionsDoNotSpendTheIssuanceBudget — упор в потолок действующих
// ключей ничего не создаёт, поэтому не должен стоить бюджета. Иначе агент,
// упёршийся в потолок, обменивает свой час на 429 и не может выпустить замену
// сразу после того, как освободил слот отзывом утёкшего ключа. Тот же отказ,
// оплаченный из общего адресного бюджета, блокировал бы ротацию и соседям за
// одним адресом.
func TestCapRejectionsDoNotSpendTheIssuanceBudget(t *testing.T) {
	// Арифметика выводится из констант, а не вписана числами: при уменьшении
	// потолка ключей захардкоженный бюджет перестал бы переполняться, и тест
	// проходил бы даже с отменённым возвратом — то есть перестал бы проверять
	// то, что назван проверять.
	// Бюджета ровно столько, чтобы отказы по потолку помещались в него без
	// возврата: тогда без возврата не проходит следующий за ними выпуск замены,
	// и падает именно то утверждение, ради которого тест написан.
	const successes = core.MaxActiveCredentials - 1
	const rejections = 3
	const budget = successes + rejections
	server, mux := newLimitedServer(t, ratelimit.Config{
		CredentialsPerHourPerAgent: budget,
		CredentialsPerHourPerIP:    budget,
		ClientIPHeader:             "Fly-Client-IP",
	})
	agent, key, err := server.svc.RegisterAgent("capped", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	spare := ""
	for i := range successes {
		status, body := issueCredential(mux, key)
		if status != http.StatusCreated {
			t.Fatalf("issue %d: status = %d body = %v, want 201", i+1, status, body)
		}
		if credential, ok := body["credential"].(map[string]any); ok && spare == "" {
			spare, _ = credential["id"].(string)
		}
	}

	// Отказы по потолку: их ровно столько, что без возврата бюджет был бы съеден.
	for i := range rejections {
		if status, body := issueCredential(mux, key); status != http.StatusConflict {
			t.Fatalf("issue %d past the cap: status = %d body = %v, want 409", i+1, status, body)
		}
	}

	// Освободили слот — замена обязана выпуститься, а не упереться в 429.
	// Отзывается не тот ключ, которым идёт аутентификация: иначе следующий
	// запрос упал бы в 401 и тест проверял бы не то.
	if status, body := revokeCredential(mux, key, spare); status != http.StatusNoContent {
		t.Fatalf("revoke to free a slot: status = %d body = %v, want 204", status, body)
	}
	if status, body := issueCredential(mux, key); status != http.StatusCreated {
		t.Fatalf("replacement issue after freeing a slot: status = %d body = %v, want 201", status, body)
	}

	// Общий адресный бюджет тоже не потрачен: сосед за тем же адресом работает.
	_, neighbourKey, err := server.svc.RegisterAgent("neighbour", "")
	if err != nil {
		t.Fatalf("RegisterAgent(neighbour): %v", err)
	}
	if status, body := issueCredential(mux, neighbourKey); status != http.StatusCreated {
		t.Fatalf("neighbour at the same address: status = %d body = %v, want 201", status, body)
	}
	if agent.ID == "" {
		t.Fatal("registered agent has no id")
	}
}

// TestCredentialEventsAreObservableWithTheClientAddress — критерий отката
// ADR 0005 разрешим только по этим строкам. Сами «выпущен» и «отозван» у
// ротации владельцем и у угона украденным ключом совпадают дословно: различает
// их адрес, поэтому он обязан быть в строке.
func TestCredentialEventsAreObservableWithTheClientAddress(t *testing.T) {
	var logged bytes.Buffer
	server, mux := newLimitedServer(t, ratelimit.Config{ClientIPHeader: "Fly-Client-IP"})
	server.log = slog.New(slog.NewTextHandler(&logged, nil))

	_, key, err := server.svc.RegisterAgent("audited", "")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	first := onlyCredential(t, mux, key)
	if status, body := issueCredentialFrom(mux, key, "203.0.113.7"); status != http.StatusCreated {
		t.Fatalf("issue: status = %d body = %v", status, body)
	}
	// Отзыв «с другого адреса» — ровно та форма, по которой правило и читается.
	if status, body := revokeCredentialFrom(mux, key, first, "198.51.100.4"); status != http.StatusNoContent {
		t.Fatalf("revoke: status = %d body = %v", status, body)
	}

	out := logged.String()
	for _, want := range []string{
		"выпущен ключ агента", "отозван ключ агента", "203.0.113.7", "198.51.100.4", first,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("credential audit log is missing %q: %s", want, out)
		}
	}
	// Секрет в лог не попадает.
	if strings.Contains(out, key) {
		t.Fatalf("credential log leaked the api key: %s", out)
	}
}

// --- Хелперы ---

func issueCredential(mux *http.ServeMux, key string) (int, map[string]any) {
	return issueCredentialFrom(mux, key, "203.0.113.7")
}

func issueCredentialFrom(mux *http.ServeMux, key, clientIP string) (int, map[string]any) {
	request := httptest.NewRequest(http.MethodPost, "/api/agents/me/credentials", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Fly-Client-IP", clientIP)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body
}

func listCredentials(t *testing.T, mux *http.ServeMux, key string) []map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/agents/me/credentials", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("list credentials: status = %d body = %s", recorder.Code, recorder.Body)
	}
	var body struct {
		Credentials []map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	return body.Credentials
}

// onlyCredential возвращает id единственного ключа агента. Вызывать до выпуска
// следующих: соответствие «ключ → credential» живёт только в хранилище, и
// опора на порядок сортировки сделала бы тест зависимым от разрешения часов.
func onlyCredential(t *testing.T, mux *http.ServeMux, key string) string {
	t.Helper()
	list := listCredentials(t, mux, key)
	if len(list) != 1 {
		t.Fatalf("agent has %d credentials; call this before issuing more", len(list))
	}
	id, _ := list[0]["id"].(string)
	if id == "" {
		t.Fatalf("credential has no id: %v", list[0])
	}
	return id
}

func revokeCredential(mux *http.ServeMux, key, credentialID string) (int, map[string]any) {
	return revokeCredentialFrom(mux, key, credentialID, "203.0.113.7")
}

func revokeCredentialFrom(mux *http.ServeMux, key, credentialID, clientIP string) (int, map[string]any) {
	request := httptest.NewRequest(http.MethodDelete, "/api/agents/me/credentials/"+credentialID, nil)
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Fly-Client-IP", clientIP)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder.Code, body
}

func whoamiStatus(mux *http.ServeMux, key string) int {
	request := httptest.NewRequest(http.MethodGet, "/api/agents/me", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder.Code
}

func whoami(t *testing.T, mux *http.ServeMux, key string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/agents/me", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("whoami: status = %d body = %s", recorder.Code, recorder.Body)
	}
	var agent core.Agent
	if err := json.Unmarshal(recorder.Body.Bytes(), &agent); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	return agent.ID
}
