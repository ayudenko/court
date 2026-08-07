package core_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"court/internal/core"
	"court/internal/store"
)

// Ротация ключей существует ради одного свойства: идентичность участника
// переживает компрометацию секрета. Протокол, голоса и вердикт ссылаются на
// стабильный agent_id, поэтому утёкший ключ не должен означать потерю автора.
// См. docs/adr/0005-credential-rotation-and-revocation.md.

// TestRotationKeepsAgentIdentityStable — выпуск нового ключа не создаёт нового
// агента: и старый, и новый ключ ведут к тому же agent_id, а уже написанное
// под старым ключом остаётся за ним.
func TestRotationKeepsAgentIdentityStable(t *testing.T) {
	service := newTestService(t)
	agent, firstKey, err := service.RegisterAgent("rotator", "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	opponent := registerAgent(t, service, "opponent")
	created, err := service.CreateDebate(agent, core.CreateDebateParams{
		Question:       "Does identity outlive a key?",
		TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(opponent, created.ID, "challenge"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}

	second, secondKey, err := service.IssueCredential(agent)
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if second.AgentID != agent.ID {
		t.Fatalf("issued credential belongs to %q, want %q", second.AgentID, agent.ID)
	}
	if secondKey == firstKey {
		t.Fatal("issued credential reused the registration key")
	}

	for name, key := range map[string]string{"registration key": firstKey, "issued key": secondKey} {
		authenticated, err := service.Authenticate(key)
		if err != nil {
			t.Fatalf("Authenticate with %s: %v", name, err)
		}
		if authenticated.ID != agent.ID {
			t.Fatalf("%s authenticated as %q, want %q", name, authenticated.ID, agent.ID)
		}
	}

	// Авторство, записанное до ротации, продолжает указывать на того же агента.
	debate, err := service.GetDebate(created.ID)
	if err != nil {
		t.Fatalf("GetDebate: %v", err)
	}
	if debate.CreatorID != agent.ID {
		t.Fatalf("debate creator = %q, want %q", debate.CreatorID, agent.ID)
	}
	rotated, err := service.Authenticate(secondKey)
	if err != nil {
		t.Fatalf("Authenticate after rotation: %v", err)
	}
	if _, err := service.StartDebate(rotated, created.ID); err != nil {
		t.Fatalf("StartDebate under the rotated key: %v", err)
	}
}

// TestRevokedCredentialStopsAuthenticating — отозванный ключ перестаёт
// работать, а остальные ключи агента продолжают.
func TestRevokedCredentialStopsAuthenticating(t *testing.T) {
	service := newTestService(t)
	agent, leakedKey, err := service.RegisterAgent("leaky", "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	leaked := registrationCredential(t, service, agent, leakedKey)
	_, freshKey, err := service.IssueCredential(agent)
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	if err := service.RevokeCredential(agent, leaked.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}

	if _, err := service.Authenticate(leakedKey); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked key authenticated: err = %v, want ErrUnauthorized", err)
	}
	if authenticated, err := service.Authenticate(freshKey); err != nil || authenticated.ID != agent.ID {
		t.Fatalf("surviving key stopped working: %+v, %v", authenticated, err)
	}

	list, err := service.Credentials(agent)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	var revoked *core.Credential
	for i := range list {
		if list[i].ID == leaked.ID {
			revoked = &list[i]
		}
	}
	if revoked == nil {
		t.Fatalf("revoked credential disappeared from the listing: %+v", list)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("revoked credential is still listed as active")
	}

	// Повторный отзыв не находит действующего ключа — тот же ответ, что и на
	// несуществующий идентификатор.
	if err := service.RevokeCredential(agent, leaked.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second revocation error = %v, want ErrNotFound", err)
	}
}

// TestLastActiveCredentialCannotBeRevoked — единственный действующий ключ
// отозвать нельзя: у идентичности агента нет канала восстановления, и такой
// запрос уничтожил бы её навсегда.
func TestLastActiveCredentialCannotBeRevoked(t *testing.T) {
	service := newTestService(t)
	agent, key, err := service.RegisterAgent("solo", "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	only := registrationCredential(t, service, agent, key)

	if err := service.RevokeCredential(agent, only.ID); !errors.Is(err, store.ErrLastCredential) {
		t.Fatalf("revoking the last credential: err = %v, want ErrLastCredential", err)
	}
	if authenticated, err := service.Authenticate(key); err != nil || authenticated.ID != agent.ID {
		t.Fatalf("refused revocation still disabled the key: %+v, %v", authenticated, err)
	}

	// Порядок ротации: выпустить новый, затем отозвать старый.
	if _, _, err := service.IssueCredential(agent); err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if err := service.RevokeCredential(agent, only.ID); err != nil {
		t.Fatalf("RevokeCredential after issuing a replacement: %v", err)
	}
}

// TestCredentialOperationsRejectAnotherAgentsCredential — операции с ключами
// ограничены собственным агентом, и чужой идентификатор неотличим от
// несуществующего: иначе эндпоинт работал бы оракулом существования.
func TestCredentialOperationsRejectAnotherAgentsCredential(t *testing.T) {
	service := newTestService(t)
	owner, ownerKey, err := service.RegisterAgent("owner", "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent(owner): %v", err)
	}
	ownerCredential := registrationCredential(t, service, owner, ownerKey)
	if _, _, err := service.IssueCredential(owner); err != nil {
		t.Fatalf("IssueCredential(owner): %v", err)
	}

	intruder := registerAgent(t, service, "intruder")

	unknownErr := service.RevokeCredential(intruder, "crd_does_not_exist")
	foreignErr := service.RevokeCredential(intruder, ownerCredential.ID)
	if !errors.Is(foreignErr, store.ErrNotFound) {
		t.Fatalf("revoking another agent's credential: err = %v, want ErrNotFound", foreignErr)
	}
	if foreignErr.Error() != unknownErr.Error() {
		t.Fatalf("foreign credential is distinguishable from an unknown one: %q vs %q",
			foreignErr, unknownErr)
	}
	if authenticated, err := service.Authenticate(ownerKey); err != nil || authenticated.ID != owner.ID {
		t.Fatalf("owner's credential was revoked by another agent: %+v, %v", authenticated, err)
	}

	// Список ключей тоже ограничен собственным агентом.
	list, err := service.Credentials(intruder)
	if err != nil {
		t.Fatalf("Credentials(intruder): %v", err)
	}
	for _, credential := range list {
		if credential.AgentID != intruder.ID {
			t.Fatalf("listing leaked credential %q of agent %q", credential.ID, credential.AgentID)
		}
	}
}

// TestActiveCredentialCapIsEnforced — набор действующих ключей конечен.
// Лимит частоты ограничивает скорость их появления, но не количество.
func TestActiveCredentialCapIsEnforced(t *testing.T) {
	service := newTestService(t)
	agent, firstKey, err := service.RegisterAgent("collector", "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	first := registrationCredential(t, service, agent, firstKey)
	// Регистрация уже выдала один ключ.
	for i := 1; i < core.MaxActiveCredentials; i++ {
		if _, _, err := service.IssueCredential(agent); err != nil {
			t.Fatalf("IssueCredential %d: %v", i+1, err)
		}
	}
	if _, _, err := service.IssueCredential(agent); !errors.Is(err, store.ErrTooManyCredentials) {
		t.Fatalf("issuing past the cap: err = %v, want ErrTooManyCredentials", err)
	}

	// Отзыв освобождает место: потолок считает действующие ключи, а не все.
	if err := service.RevokeCredential(agent, first.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, _, err := service.IssueCredential(agent); err != nil {
		t.Fatalf("IssueCredential after freeing a slot: %v", err)
	}
}

// TestCredentialListingCarriesNoKeyMaterial — наружу выходят только
// метаданные. Хэш ключа не покидает границу хранилища (ADR 0002).
func TestCredentialListingCarriesNoKeyMaterial(t *testing.T) {
	service := newTestService(t)
	agent, key, err := service.RegisterAgent("inspected", "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	issued, issuedKey, err := service.IssueCredential(agent)
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	list, err := service.Credentials(agent)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("Credentials returned %d entries, want 2", len(list))
	}
	for _, credential := range list {
		rendered := renderCredential(credential)
		for name, secret := range map[string]string{
			"registration key":      key,
			"issued key":            issuedKey,
			"registration key hash": sha256Hex(key),
			"issued key hash":       sha256Hex(issuedKey),
		} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("credential %q exposes the %s: %s", credential.ID, name, rendered)
			}
		}
	}
	if issued.CreatedAt.IsZero() {
		t.Fatal("issued credential has no creation time")
	}
}

// TestCredentialListingStaysBoundedAndKeepsActiveKeysVisible — отозванные
// строки накапливаются и не удаляются, поэтому список ограничен. Обрезаться
// обязана история, а не действующие ключи: иначе владелец перестал бы видеть
// ключ, который ему нужно отозвать.
func TestCredentialListingStaysBoundedAndKeepsActiveKeysVisible(t *testing.T) {
	service := newTestService(t)
	agent, firstKey, err := service.RegisterAgent("busy", "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	oldest := registrationCredential(t, service, agent, firstKey)

	// Набор действующих ключей у самого потолка: их не должно вытеснить ничем.
	// Проверять на одном ключе мало — так тест не заметил бы, что потолок
	// выдачи стал меньше потолка действующих ключей. Один слот остаётся
	// свободным: через него дальше прокручивается история.
	wantActive := map[string]bool{oldest.ID: true}
	for range core.MaxActiveCredentials - 2 {
		issued, _, err := service.IssueCredential(agent)
		if err != nil {
			t.Fatalf("IssueCredential: %v", err)
		}
		wantActive[issued.ID] = true
	}

	// История: каждая ротация оставляет отозванную строку. Их заведомо больше,
	// чем помещается в выдачу.
	for range store.MaxListedCredentials + 10 {
		issued, _, err := service.IssueCredential(agent)
		if err != nil {
			t.Fatalf("IssueCredential for history: %v", err)
		}
		if err := service.RevokeCredential(agent, issued.ID); err != nil {
			t.Fatalf("RevokeCredential for history: %v", err)
		}
	}

	list, err := service.Credentials(agent)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(list) > store.MaxListedCredentials {
		t.Fatalf("listing returned %d credentials, want at most %d", len(list), store.MaxListedCredentials)
	}
	seen := map[string]bool{}
	for _, credential := range list {
		if credential.RevokedAt == nil {
			seen[credential.ID] = true
		}
	}
	if len(seen) != len(wantActive) {
		t.Fatalf("listing shows %d active credentials, want all %d", len(seen), len(wantActive))
	}
	for id := range wantActive {
		if !seen[id] {
			t.Fatalf("active credential %q was truncated out of the listing", id)
		}
	}
	// Виден не декоративно: самый старый действующий ключ остаётся отзываемым.
	if err := service.RevokeCredential(agent, oldest.ID); err != nil {
		t.Fatalf("RevokeCredential on the oldest active credential: %v", err)
	}
}

// registrationCredential возвращает запись ключа, выданного при регистрации.
// Соответствие «ключ → credential» живёт только внутри хранилища, поэтому
// вызывать хелпер можно лишь пока у агента ровно один ключ — тогда это не
// предположение о порядке выдачи, а единственный элемент списка. Порядок
// сортировки зависит от разрешения часов, и тест, опирающийся на него, начал бы
// врать под инъецированным временем.
func registrationCredential(t *testing.T, service *core.Service, agent core.Agent, key string) core.Credential {
	t.Helper()
	authenticated, err := service.Authenticate(key)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authenticated.ID != agent.ID {
		t.Fatalf("key belongs to %q, want %q", authenticated.ID, agent.ID)
	}
	list, err := service.Credentials(agent)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("agent %q has %d credentials; call this before issuing more", agent.ID, len(list))
	}
	if list[0].RevokedAt != nil {
		t.Fatalf("registration credential of %q is already revoked", agent.ID)
	}
	return list[0]
}

// renderCredential склеивает всё, что Credential выносит наружу.
func renderCredential(c core.Credential) string {
	revoked := "active"
	if c.RevokedAt != nil {
		revoked = c.RevokedAt.String()
	}
	return strings.Join([]string{c.ID, c.AgentID, c.CreatedAt.String(), revoked}, " ")
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
