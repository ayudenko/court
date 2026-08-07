package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"court/internal/core"
)

func TestCredentialStoreKeepsStableAgentAcrossRotationAndRevocation(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	agent := core.Agent{ID: "agt_stable", Name: "Stable", Persona: "test", CreatedAt: now}
	first := core.Credential{ID: "crd_first", AgentID: agent.ID, CreatedAt: now}
	if err := st.CreateAgent(agent, first, "hash-first"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	second := core.Credential{ID: "crd_second", AgentID: agent.ID, CreatedAt: now.Add(time.Minute)}
	if err := st.CreateCredential(second, "hash-second", core.MaxActiveCredentials); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	for _, hash := range []string{"hash-first", "hash-second"} {
		got, err := st.AgentByCredentialHash(hash)
		if err != nil {
			t.Fatalf("AgentByCredentialHash(%q): %v", hash, err)
		}
		if got.ID != agent.ID {
			t.Fatalf("credential %q resolved agent %q, want %q", hash, got.ID, agent.ID)
		}
	}

	if err := st.RevokeCredential(agent.ID, first.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, err := st.AgentByCredentialHash("hash-first"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked credential error = %v, want ErrNotFound", err)
	}
	if got, err := st.AgentByCredentialHash("hash-second"); err != nil || got.ID != agent.ID {
		t.Fatalf("active credential resolved agent = %+v, err = %v", got, err)
	}
}

// TestRevocationTombstonesTheRollbackShadow — ADR 0002 оставил
// agents.api_key_hash как тень для отката на предыдущий бинарь и записал
// условие: пока отзыв не опубликован, тень безопасна. Отзыв опубликован
// (ADR 0005), поэтому тень обязана перестать быть вторым путём аутентификации:
// иначе откат молча воскресил бы ключ, который владелец только что убил.
func TestRevocationTombstonesTheRollbackShadow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tombstone.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	agent := core.Agent{ID: "agt_rotated", Name: "Rotated", CreatedAt: now}
	first := core.Credential{ID: "crd_first", AgentID: agent.ID, CreatedAt: now}
	if err := st.CreateAgent(agent, first, "hash-first"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	second := core.Credential{ID: "crd_second", AgentID: agent.ID, CreatedAt: now.Add(time.Minute)}
	if err := st.CreateCredential(second, "hash-second", core.MaxActiveCredentials); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	// Выпуск дополнительного ключа тень не трогает: она хранит один хэш, и
	// зеркалирование произвольного ключа сделало бы её несогласованной с
	// таблицей credentials.
	if shadow := shadowHash(t, st, agent.ID); shadow != "hash-first" {
		t.Fatalf("issuing a credential changed the shadow to %q, want the registration hash", shadow)
	}

	if err := st.RevokeCredential(agent.ID, first.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	shadow := shadowHash(t, st, agent.ID)
	if shadow != revokedShadowPrefix+agent.ID {
		t.Fatalf("shadow after revocation = %q, want %q", shadow, revokedShadowPrefix+agent.ID)
	}

	// Предыдущий бинарь аутентифицируется запросом по тени. После отзыва он не
	// принимает ни отозванный ключ, ни выпущенный позже: fail-closed по
	// доступности вместо воскрешённого секрета.
	for _, hash := range []string{"hash-first", "hash-second"} {
		var id string
		err := st.db.QueryRow(`SELECT id FROM agents WHERE api_key_hash = ?`, hash).Scan(&id)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("previous-binary shadow query for %q returned %q, err = %v; want no rows", hash, id, err)
		}
	}

	// Новый бинарь по-прежнему принимает действующий ключ.
	if got, err := st.AgentByCredentialHash("hash-second"); err != nil || got.ID != agent.ID {
		t.Fatalf("active credential after revocation = %+v, err = %v", got, err)
	}

	// Рестарт не воскрешает надгробие как новый credential: догоняющая
	// миграция копирует тень обратно, и без исключения каждый запуск создавал
	// бы фантомный действующий ключ.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	list, err := reopened.Credentials(agent.ID)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("credentials after restart = %d (%+v), want the original two", len(list), list)
	}
	active := make([]string, 0, len(list))
	for _, credential := range list {
		if credential.RevokedAt == nil {
			active = append(active, credential.ID)
		}
	}
	if len(active) != 1 || active[0] != second.ID {
		t.Fatalf("active credentials after restart = %v, want [%s]", active, second.ID)
	}
}

// TestTombstonedShadowSurvivesThePreviousBinaryMigration — предыдущий бинарь не
// только читает тень, он копирует её обратно в agent_credentials при каждом
// старте. Надгробие обязано пережить этот запрос: на legacy-базе иначе
// возникает конфликт первичного ключа (бинарь не стартует), а на свежей —
// фантомный «действующий» ключ, который никто не выпускал и который обходит
// правило последнего ключа. См. docs/adr/0005.
func TestTombstonedShadowSurvivesThePreviousBinaryMigration(t *testing.T) {
	// Обе формы идентификатора credential: legacy-строка, созданная миграцией
	// ADR 0002 как 'crd_legacy_' || agent_id, и обычная случайная.
	for _, testCase := range []struct {
		name         string
		agentID      string
		credentialID string
	}{
		{"legacy migrated agent", "agt_legacy", "crd_legacy_agt_legacy"},
		{"agent registered under v1", "agt_fresh", "crd_2f6c1d9ab3e4"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollback.db")
			st, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
			agent := core.Agent{ID: testCase.agentID, Name: "Rotated", CreatedAt: now}
			first := core.Credential{ID: testCase.credentialID, AgentID: agent.ID, CreatedAt: now}
			if err := st.CreateAgent(agent, first, "hash-first"); err != nil {
				t.Fatalf("CreateAgent: %v", err)
			}
			second := core.Credential{ID: "crd_second", AgentID: agent.ID, CreatedAt: now.Add(time.Minute)}
			if err := st.CreateCredential(second, "hash-second", core.MaxActiveCredentials); err != nil {
				t.Fatalf("CreateCredential: %v", err)
			}
			if err := st.RevokeCredential(agent.ID, first.ID, now.Add(2*time.Minute)); err != nil {
				t.Fatalf("RevokeCredential: %v", err)
			}
			if err := st.Close(); err != nil {
				t.Fatalf("Close before the rollback: %v", err)
			}

			// Дословно statements предыдущего бинаря: миграция без исключения
			// надгробий и следующая за ней проверка целостности.
			previous, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
			if err != nil {
				t.Fatalf("previous binary open: %v", err)
			}
			if _, err := previous.Exec(`
				INSERT INTO agent_credentials (id, agent_id, key_hash, created_at)
				SELECT 'crd_legacy_' || id, id, api_key_hash, created_at
				FROM agents a
				WHERE api_key_hash <> ''
				  AND NOT EXISTS (
					SELECT 1 FROM agent_credentials c WHERE c.key_hash = a.api_key_hash
				  )
			`); err != nil {
				t.Fatalf("previous binary startup migration refused to run: %v", err)
			}
			var missing int
			if err := previous.QueryRow(`
				SELECT COUNT(*)
				FROM agents a
				LEFT JOIN agent_credentials c
				  ON c.key_hash = a.api_key_hash AND c.agent_id = a.id
				WHERE a.api_key_hash <> '' AND c.id IS NULL
			`).Scan(&missing); err != nil {
				t.Fatalf("previous binary consistency query: %v", err)
			}
			if missing != 0 {
				t.Fatalf("previous binary reported %d unlinked keys, want 0", missing)
			}
			// Предыдущий бинарь аутентифицируется через agent_credentials, и
			// отозванный ключ обязан остаться отозванным и для него.
			var revived string
			err = previous.QueryRow(
				`SELECT id FROM agent_credentials WHERE key_hash = ? AND revoked_at IS NULL`, "hash-first",
			).Scan(&revived)
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("previous binary revived credential %q, err = %v; want no rows", revived, err)
			}
			if err := previous.Close(); err != nil {
				t.Fatalf("previous binary close: %v", err)
			}

			// Возврат вперёд: ни одного ключа, которого владелец не выпускал.
			rolledForward, err := Open(path)
			if err != nil {
				t.Fatalf("roll forward: %v", err)
			}
			t.Cleanup(func() { _ = rolledForward.Close() })
			list, err := rolledForward.Credentials(agent.ID)
			if err != nil {
				t.Fatalf("Credentials: %v", err)
			}
			active := make([]string, 0, len(list))
			for _, credential := range list {
				if credential.RevokedAt == nil {
					active = append(active, credential.ID)
				}
			}
			if len(active) != 1 || active[0] != second.ID {
				t.Fatalf("active credentials after the rollback window = %v, want [%s]", active, second.ID)
			}
			// Фантомный ключ обошёл бы правило последнего: с ним отзыв
			// единственного настоящего ключа прошёл бы успешно.
			if err := rolledForward.RevokeCredential(agent.ID, second.ID, now.Add(time.Hour)); !errors.Is(err, ErrLastCredential) {
				t.Fatalf("revoking the last real credential: err = %v, want ErrLastCredential", err)
			}
		})
	}
}

func shadowHash(t *testing.T, st *Store, agentID string) string {
	t.Helper()
	var hash string
	if err := st.db.QueryRow(`SELECT api_key_hash FROM agents WHERE id = ?`, agentID).Scan(&hash); err != nil {
		t.Fatalf("read shadow hash: %v", err)
	}
	return hash
}

func TestFreshCredentialSchemaSupportsPreviousBinaryRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Date(2026, 8, 6, 10, 30, 0, 0, time.UTC)
	agent := core.Agent{ID: "agt_new", Name: "New", CreatedAt: now}
	credential := core.Credential{ID: "crd_new", AgentID: agent.ID, CreatedAt: now}
	if err := st.CreateAgent(agent, credential, "hash-new"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// These are the exact authentication and registration statements used by
	// the previous binary. The shadow makes a binary-only rollback executable.
	var oldBinaryAgent core.Agent
	if err := st.db.QueryRow(
		`SELECT id, name, persona, created_at FROM agents WHERE api_key_hash = ?`, "hash-new",
	).Scan(&oldBinaryAgent.ID, &oldBinaryAgent.Name, &oldBinaryAgent.Persona, &oldBinaryAgent.CreatedAt); err != nil {
		t.Fatalf("previous binary authentication query: %v", err)
	}
	if oldBinaryAgent.ID != agent.ID {
		t.Fatalf("previous binary resolved agent %q, want %q", oldBinaryAgent.ID, agent.ID)
	}
	if _, err := st.db.Exec(
		`INSERT INTO agents (id, name, persona, api_key_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		"agt_from_old", "Old binary", "", "hash-from-old", now.Add(time.Minute),
	); err != nil {
		t.Fatalf("previous binary registration statement: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close before returning to new binary: %v", err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("Open after previous binary registration: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got, err := st.AgentByCredentialHash("hash-from-old"); err != nil || got.ID != "agt_from_old" {
		t.Fatalf("new binary after rollback window resolved agent = %+v, err = %v", got, err)
	}
}

func TestOpenMigratesLegacyCredentialsWithoutLosingIdentityOrTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacyHash := "legacy-key-hash"
	createdAt := time.Date(2026, 8, 5, 9, 30, 0, 0, time.UTC)
	legacyShape := createLegacyDatabase(t, path, legacyHash, createdAt)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy database: %v", err)
	}
	got, err := st.AgentByCredentialHash(legacyHash)
	if err != nil {
		t.Fatalf("migrated credential authentication: %v", err)
	}
	if got.ID != "agt_legacy" || got.Name != "Legacy" || got.Persona != "persona" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("migrated agent = %+v", got)
	}
	assertLegacyState(t, st, createdAt)
	if got := legacySchemaFingerprint(t, st.db); got != legacyShape {
		t.Fatalf("legacy schema shape changed after migration\nbefore: %s\nafter:  %s", legacyShape, got)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close migrated database: %v", err)
	}

	// Reopening proves that the backfill is idempotent rather than duplicating
	// credentials or changing which identity a key resolves to.
	st, err = Open(path)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var credentialCount int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM agent_credentials WHERE agent_id = ?`, "agt_legacy",
	).Scan(&credentialCount); err != nil {
		t.Fatalf("count migrated credentials: %v", err)
	}
	if credentialCount != 1 {
		t.Fatalf("migrated credential count = %d, want 1", credentialCount)
	}
	if got, err := st.AgentByCredentialHash(legacyHash); err != nil || got.ID != "agt_legacy" ||
		got.Name != "Legacy" || got.Persona != "persona" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("authentication after reopen = %+v, err = %v", got, err)
	}
	assertLegacyState(t, st, createdAt)
	if got := legacySchemaFingerprint(t, st.db); got != legacyShape {
		t.Fatalf("legacy schema shape changed after second open\nbefore: %s\nafter:  %s", legacyShape, got)
	}
}

func TestModerationPayloadRoundTripsWithoutChangingRenderedText(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)
	agent := core.Agent{ID: "agt_moderator", Name: "Owner", CreatedAt: now}
	credential := core.Credential{ID: "crd_moderator", AgentID: agent.ID, CreatedAt: now}
	if err := st.CreateAgent(agent, credential, "hash-owner"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.CreateDebate(core.Debate{
		ID: "dbt_structured", Question: "Q", Mode: core.ModeModerator,
		Status: core.StatusModerating, Rounds: 2, CurrentRound: 1,
		TurnTimeout: core.MinTurnTimeout, CreatorID: agent.ID, CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}

	summary := core.RoundSummary{
		Summary:             "Summary",
		Claims:              []core.ModerationClaim{{Text: "Claim", Citations: []int64{1, 2}}},
		UnresolvedQuestions: []string{"Question"},
		Decisions:           []string{"Decision"},
		Consensus:           false,
	}
	verdict := core.ModerationVerdict{
		FinalAnswer:         "Answer",
		Claims:              []core.ModerationClaim{{Text: "Supported", Citations: []int64{2}}},
		UnresolvedQuestions: []string{},
		Decisions:           []string{"Ship"},
		Consensus:           true,
	}
	for _, message := range []core.Message{
		{DebateID: "dbt_structured", Round: 1, SpeakerName: "Moderator", Kind: core.KindSummary,
			Text: summary.Text(), RoundSummary: &summary, CreatedAt: now},
		{DebateID: "dbt_structured", Round: 1, SpeakerName: "Moderator", Kind: core.KindVerdict,
			Text: verdict.Text(), Verdict: &verdict, CreatedAt: now.Add(time.Second)},
	} {
		if _, err := st.AddMessage(message); err != nil {
			t.Fatalf("AddMessage(%s): %v", message.Kind, err)
		}
	}

	messages, err := st.Messages("dbt_structured", 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(messages))
	}
	if messages[0].Text != summary.Text() || !reflect.DeepEqual(messages[0].RoundSummary, &summary) {
		t.Fatalf("round summary did not round-trip: %+v", messages[0])
	}
	if messages[1].Text != verdict.Text() || !reflect.DeepEqual(messages[1].Verdict, &verdict) {
		t.Fatalf("verdict did not round-trip: %+v", messages[1])
	}
	var storedJSON string
	if err := st.db.QueryRow(
		`SELECT moderation_json FROM messages WHERE debate_id = ? AND kind = ?`,
		"dbt_structured", core.KindSummary,
	).Scan(&storedJSON); err != nil {
		t.Fatalf("read stored moderation envelope: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(storedJSON), &envelope); err != nil {
		t.Fatalf("decode stored moderation envelope: %v", err)
	}
	if envelope["schema_version"] != float64(currentModerationSchemaVersion) {
		t.Fatalf("stored moderation schema_version = %v", envelope["schema_version"])
	}

	for name, invalid := range map[string]core.Message{
		"summary without payload": {DebateID: "dbt_structured", Kind: core.KindSummary},
		"verdict without payload": {DebateID: "dbt_structured", Kind: core.KindVerdict},
		"argument with summary":   {DebateID: "dbt_structured", Kind: core.KindArgument, RoundSummary: &summary},
		"summary with verdict":    {DebateID: "dbt_structured", Kind: core.KindSummary, Verdict: &verdict},
		"verdict with summary":    {DebateID: "dbt_structured", Kind: core.KindVerdict, RoundSummary: &summary},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := st.AddMessage(invalid); err == nil {
				t.Fatal("AddMessage accepted an invalid new moderation message")
			}
		})
	}

	envelope["schema_version"] = currentModerationSchemaVersion + 1
	unsupported, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode unsupported moderation fixture: %v", err)
	}
	if _, err := st.db.Exec(
		`UPDATE messages SET moderation_json = ? WHERE debate_id = ? AND kind = ?`,
		string(unsupported), "dbt_structured", core.KindSummary,
	); err != nil {
		t.Fatalf("install unsupported moderation fixture: %v", err)
	}
	if _, err := st.Messages("dbt_structured", 0); err == nil {
		t.Fatal("Messages accepted an unsupported durable moderation schema version")
	}
}

func TestDurableModerationV1IsIndependentFromFuturePublicProtocolVersion(t *testing.T) {
	simulatedPublicVersion := core.CurrentProtocolSchemaVersion + 1
	// These complete fixtures are deliberately literal: they are neither
	// marshaled from core types nor built with either current-version constant.
	// Public JSON-tag edits and public version bumps therefore cannot silently
	// rewrite the contract for rows already stored as durable v1.
	tests := []struct {
		name    string
		kind    string
		payload string
		want    core.Message
	}{
		{
			name: "round summary",
			kind: core.KindSummary,
			payload: `{"schema_version":1,"round_summary":{"summary":"durable summary","claims":[{"text":"durable claim","citations":[7,11]}],` +
				`"unresolved_questions":["open question"],"decisions":["record decision"],"consensus":false}}`,
			want: core.Message{RoundSummary: &core.RoundSummary{
				Summary:             "durable summary",
				Claims:              []core.ModerationClaim{{Text: "durable claim", Citations: []int64{7, 11}}},
				UnresolvedQuestions: []string{"open question"},
				Decisions:           []string{"record decision"},
				Consensus:           false,
			}},
		},
		{
			name: "verdict",
			kind: core.KindVerdict,
			payload: `{"schema_version":1,"verdict":{"final_answer":"durable answer","claims":[{"text":"supported claim","citations":[13]}],` +
				`"unresolved_questions":[],"decisions":["ship it"],"consensus":true}}`,
			want: core.Message{Verdict: &core.ModerationVerdict{
				FinalAnswer:         "durable answer",
				Claims:              []core.ModerationClaim{{Text: "supported claim", Citations: []int64{13}}},
				UnresolvedQuestions: []string{},
				Decisions:           []string{"ship it"},
				Consensus:           true,
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := core.Message{Kind: tt.kind}
			if err := decodeModeration(&message, tt.payload); err != nil {
				t.Fatalf("decode durable v1 while public protocol is simulated as v%d: %v", simulatedPublicVersion, err)
			}
			if !reflect.DeepEqual(message.RoundSummary, tt.want.RoundSummary) || !reflect.DeepEqual(message.Verdict, tt.want.Verdict) {
				t.Fatalf("decoded durable payload = summary:%+v verdict:%+v, want summary:%+v verdict:%+v",
					message.RoundSummary, message.Verdict, tt.want.RoundSummary, tt.want.Verdict)
			}
		})
	}
}

func TestRollForwardInventoriesModerationWrittenByPreviousBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "court.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1 database: %v", err)
	}
	now := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	agent := core.Agent{ID: "agt_rollback", Name: "Rollback", CreatedAt: now}
	credential := core.Credential{ID: "crd_rollback", AgentID: agent.ID, CreatedAt: now}
	if err := st.CreateAgent(agent, credential, "rollback-key-hash"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.CreateDebate(core.Debate{
		ID: "dbt_rollback", Question: "Rollback?", Mode: core.ModeModerator,
		Status: core.StatusModerating, Rounds: 1, CurrentRound: 1,
		TurnTimeout: core.MinTurnTimeout, CreatorID: agent.ID, CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	// Exact previous-binary insert shape: the old process does not know about
	// moderation_json, so SQLite leaves it NULL during a binary rollback.
	if _, err := st.db.Exec(
		`INSERT INTO messages (debate_id, round, speaker_id, speaker_name, kind, text,
		 support_id, support_name, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"dbt_rollback", 1, "", "Moderator", core.KindVerdict, "previous-binary verdict", "", "", now.Add(time.Minute),
	); err != nil {
		t.Fatalf("previous-binary message insert: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close before roll-forward: %v", err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("Open after roll-forward: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	messages, err := st.Messages("dbt_rollback", 0)
	if err != nil {
		t.Fatalf("Messages after roll-forward: %v", err)
	}
	if len(messages) != 1 || messages[0].Kind != core.KindVerdict ||
		!messages[0].LegacyUnstructured || messages[0].Verdict != nil {
		t.Fatalf("previous-binary moderation was not surfaced as unstructured evidence: %+v", messages)
	}
	var unstructured int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE kind IN (?, ?) AND moderation_json IS NULL`,
		core.KindSummary, core.KindVerdict,
	).Scan(&unstructured); err != nil {
		t.Fatalf("inventory unstructured moderation: %v", err)
	}
	if unstructured != 1 {
		t.Fatalf("unstructured moderation inventory = %d, want 1", unstructured)
	}
}

func assertLegacyState(t *testing.T, st *Store, createdAt time.Time) {
	t.Helper()
	messages, err := st.Messages("dbt_legacy", 0)
	if err != nil {
		t.Fatalf("Messages after migration: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("migrated messages = %+v", messages)
	}
	message := messages[0]
	if message.Seq != 7 || message.Round != 1 || message.SpeakerID != "agt_legacy" ||
		message.SpeakerName != "Legacy" || message.Kind != core.KindArgument ||
		message.Text != "legacy transcript" || message.SupportID != "agt_legacy" ||
		message.SupportName != "Legacy" || !message.CreatedAt.Equal(createdAt.Add(2*time.Minute)) {
		t.Fatalf("migrated message = %+v", message)
	}
	if message.RoundSummary != nil || message.Verdict != nil || message.LegacyUnstructured {
		t.Fatal("legacy argument was incorrectly marked as structured moderation")
	}
	legacySummary := messages[1]
	if legacySummary.Seq != 8 || legacySummary.Kind != core.KindSummary ||
		legacySummary.Text != "legacy summary" || legacySummary.RoundSummary != nil ||
		legacySummary.Verdict != nil || !legacySummary.LegacyUnstructured {
		t.Fatalf("legacy summary = %+v", legacySummary)
	}
	if !legacySummary.CreatedAt.Equal(createdAt.Add(3 * time.Minute)) {
		t.Fatal("legacy prose-only message was not marked for canonical export")
	}

	debate, err := st.GetDebate("dbt_legacy")
	deadline := createdAt.Add(time.Hour)
	if err != nil || debate.Question != "Legacy question" || debate.Description != "Legacy description" ||
		debate.Mode != core.ModeHybrid || debate.Status != core.StatusRunning || debate.Rounds != 2 ||
		debate.CurrentRound != 1 || debate.TurnTimeout != 45 || debate.PrepTime != 15 ||
		debate.CreatorID != "agt_legacy" || debate.TurnAgentID != "agt_legacy" ||
		!debate.TurnDeadline.Equal(deadline) || !debate.Consensus || !debate.CreatedAt.Equal(createdAt) {
		t.Fatalf("migrated debate = %+v, err = %v", debate, err)
	}
	participants, err := st.Participants("dbt_legacy")
	if err != nil || len(participants) != 1 || participants[0].AgentID != "agt_legacy" ||
		participants[0].Name != "Legacy" || participants[0].Stance != "legacy stance" ||
		!participants[0].JoinedAt.Equal(createdAt.Add(time.Minute)) {
		t.Fatalf("migrated participants = %+v, err = %v", participants, err)
	}

}

// TestModeratorTokensAccumulateAndSurviveDebateWrites охраняет носитель потолка
// расхода: счётчик увеличивается только инкрементом, и UpdateDebate,
// записывающий состояние из возможно устаревшей копии Debate, не имеет права его
// затирать — иначе очередной ход дебатов возвращал бы им уже потраченный бюджет
// (docs/adr/0004-moderator-spend-ceiling.md).
func TestModeratorTokensAccumulateAndSurviveDebateWrites(t *testing.T) {
	st := openTestStore(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	agent := core.Agent{ID: "agt_owner", Name: "Owner", CreatedAt: now}
	if err := st.CreateAgent(agent, core.Credential{ID: "crd_owner", AgentID: agent.ID, CreatedAt: now}, "hash-owner"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	debate := core.Debate{
		ID: "dbt_spend", Question: "Нужен ли потолок?", Mode: core.ModeModerator,
		Status: core.StatusRunning, Rounds: 3, CurrentRound: 1, TurnTimeout: 60,
		CreatorID: agent.ID, CreatedAt: now,
	}
	if err := st.CreateDebate(debate); err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if stored, err := st.GetDebate(debate.ID); err != nil || stored.ModeratorTokens != 0 {
		t.Fatalf("новые дебаты стартуют с расходом %d, err = %v", stored.ModeratorTokens, err)
	}

	for _, tokens := range []int{4_000, 6_500} {
		if err := st.AddModeratorTokens(debate.ID, tokens); err != nil {
			t.Fatalf("AddModeratorTokens(%d): %v", tokens, err)
		}
	}
	if stored, err := st.GetDebate(debate.ID); err != nil || stored.ModeratorTokens != 10_500 {
		t.Fatalf("накопленный расход = %d, ожидалось 10500, err = %v", stored.ModeratorTokens, err)
	}

	// Устаревшая копия: прочитана до списаний и ничего о них не знает.
	stale := debate
	stale.CurrentRound = 2
	if err := st.UpdateDebate(stale); err != nil {
		t.Fatalf("UpdateDebate: %v", err)
	}
	stored, err := st.GetDebate(debate.ID)
	if err != nil {
		t.Fatalf("GetDebate после UpdateDebate: %v", err)
	}
	if stored.ModeratorTokens != 10_500 {
		t.Fatalf("расход после записи состояния = %d, ожидалось 10500 — UpdateDebate затирает счётчик",
			stored.ModeratorTokens)
	}
	if stored.CurrentRound != 2 {
		t.Fatalf("UpdateDebate не записал раунд: %d", stored.CurrentRound)
	}

	if err := st.AddModeratorTokens("dbt_missing", 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("списание на несуществующие дебаты: err = %v, ожидалась ErrNotFound", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "court.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func createLegacyDatabase(t *testing.T, path, keyHash string, createdAt time.Time) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open legacy: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`
		CREATE TABLE agents (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			persona TEXT NOT NULL DEFAULT '',
			api_key_hash TEXT NOT NULL UNIQUE,
			created_at TIMESTAMP NOT NULL
		);
		CREATE TABLE debates (
			id TEXT PRIMARY KEY,
			question TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'moderator',
			status TEXT NOT NULL,
			rounds INTEGER NOT NULL,
			current_round INTEGER NOT NULL DEFAULT 0,
			turn_timeout INTEGER NOT NULL,
			prep_time INTEGER NOT NULL DEFAULT 0,
			creator_id TEXT NOT NULL REFERENCES agents(id),
			turn_agent_id TEXT NOT NULL DEFAULT '',
			turn_deadline TIMESTAMP,
			consensus INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL
		);
		CREATE TABLE participants (
			debate_id TEXT NOT NULL REFERENCES debates(id),
			agent_id TEXT NOT NULL REFERENCES agents(id),
			stance TEXT NOT NULL DEFAULT '',
			joined_at TIMESTAMP NOT NULL,
			PRIMARY KEY (debate_id, agent_id)
		);
		CREATE TABLE messages (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			debate_id TEXT NOT NULL REFERENCES debates(id),
			round INTEGER NOT NULL,
			speaker_id TEXT NOT NULL DEFAULT '',
			speaker_name TEXT NOT NULL,
			kind TEXT NOT NULL,
			text TEXT NOT NULL,
			support_id TEXT NOT NULL DEFAULT '',
			support_name TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL
		);
		CREATE INDEX idx_messages_debate ON messages(debate_id, seq);
		CREATE INDEX idx_debates_status ON debates(status);
	`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO agents (id, name, persona, api_key_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		"agt_legacy", "Legacy", "persona", keyHash, createdAt,
	); err != nil {
		t.Fatalf("insert legacy agent: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO debates (id, question, description, mode, status, rounds, current_round, turn_timeout,
		 prep_time, creator_id, turn_agent_id, turn_deadline, consensus, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"dbt_legacy", "Legacy question", "Legacy description", core.ModeHybrid, core.StatusRunning, 2, 1,
		45, 15, "agt_legacy", "agt_legacy", createdAt.Add(time.Hour), 1, createdAt,
	); err != nil {
		t.Fatalf("insert legacy debate: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO participants (debate_id, agent_id, stance, joined_at) VALUES (?, ?, ?, ?)`,
		"dbt_legacy", "agt_legacy", "legacy stance", createdAt.Add(time.Minute),
	); err != nil {
		t.Fatalf("insert legacy participant: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages (seq, debate_id, round, speaker_id, speaker_name, kind, text,
		 support_id, support_name, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		7, "dbt_legacy", 1, "agt_legacy", "Legacy", core.KindArgument, "legacy transcript",
		"agt_legacy", "Legacy", createdAt.Add(2*time.Minute),
	); err != nil {
		t.Fatalf("insert legacy message: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO messages (seq, debate_id, round, speaker_name, kind, text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		8, "dbt_legacy", 1, "Moderator", core.KindSummary, "legacy summary", createdAt.Add(3*time.Minute),
	); err != nil {
		t.Fatalf("insert legacy summary: %v", err)
	}
	return legacySchemaFingerprint(t, db)
}

type legacyColumnShape struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	NotNull    int    `json:"not_null"`
	Default    string `json:"default,omitempty"`
	HasDefault bool   `json:"has_default"`
	PrimaryKey int    `json:"primary_key"`
}

type legacyForeignKeyShape struct {
	Table    string `json:"table"`
	From     string `json:"from"`
	To       string `json:"to"`
	OnUpdate string `json:"on_update"`
	OnDelete string `json:"on_delete"`
	Match    string `json:"match"`
}

type legacyIndexShape struct {
	Name    string   `json:"name"`
	Unique  int      `json:"unique"`
	Origin  string   `json:"origin"`
	Partial int      `json:"partial"`
	Columns []string `json:"columns"`
}

type legacyTableShape struct {
	Columns       []legacyColumnShape     `json:"columns"`
	ForeignKeys   []legacyForeignKeyShape `json:"foreign_keys"`
	Indexes       []legacyIndexShape      `json:"indexes"`
	Autoincrement bool                    `json:"autoincrement"`
}

func legacySchemaFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	shape := make(map[string]legacyTableShape)
	for _, table := range []string{"agents", "debates", "participants", "messages"} {
		var tableShape legacyTableShape
		rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatalf("table_info(%s): %v", table, err)
		}
		for rows.Next() {
			var cid int
			var column legacyColumnShape
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &column.Name, &column.Type, &column.NotNull, &defaultValue, &column.PrimaryKey); err != nil {
				t.Fatalf("scan table_info(%s): %v", table, err)
			}
			// Умышленные additive-миграции: колонка добавлена с DEFAULT, поэтому
			// прежний бинарь продолжает писать в таблицу и откат остаётся
			// исполнимым. Отпечаток охраняет обратное — что миграция не меняет и
			// не удаляет то, что было в legacy-схеме.
			if column.Name == "moderation_json" || column.Name == "moderator_tokens" {
				continue
			}
			column.Default, column.HasDefault = defaultValue.String, defaultValue.Valid
			tableShape.Columns = append(tableShape.Columns, column)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close table_info(%s): %v", table, err)
		}

		rows, err = db.Query(`PRAGMA foreign_key_list(` + table + `)`)
		if err != nil {
			t.Fatalf("foreign_key_list(%s): %v", table, err)
		}
		for rows.Next() {
			var id, seq int
			var foreignKey legacyForeignKeyShape
			if err := rows.Scan(&id, &seq, &foreignKey.Table, &foreignKey.From, &foreignKey.To,
				&foreignKey.OnUpdate, &foreignKey.OnDelete, &foreignKey.Match); err != nil {
				t.Fatalf("scan foreign_key_list(%s): %v", table, err)
			}
			tableShape.ForeignKeys = append(tableShape.ForeignKeys, foreignKey)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close foreign_key_list(%s): %v", table, err)
		}
		sort.Slice(tableShape.ForeignKeys, func(i, j int) bool {
			return tableShape.ForeignKeys[i].From < tableShape.ForeignKeys[j].From
		})

		rows, err = db.Query(`PRAGMA index_list(` + table + `)`)
		if err != nil {
			t.Fatalf("index_list(%s): %v", table, err)
		}
		for rows.Next() {
			var seq int
			var index legacyIndexShape
			if err := rows.Scan(&seq, &index.Name, &index.Unique, &index.Origin, &index.Partial); err != nil {
				t.Fatalf("scan index_list(%s): %v", table, err)
			}
			tableShape.Indexes = append(tableShape.Indexes, index)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close index_list(%s): %v", table, err)
		}
		for i := range tableShape.Indexes {
			indexRows, err := db.Query(`PRAGMA index_info(` + tableShape.Indexes[i].Name + `)`)
			if err != nil {
				t.Fatalf("index_info(%s): %v", tableShape.Indexes[i].Name, err)
			}
			for indexRows.Next() {
				var indexSeq, cid int
				var name string
				if err := indexRows.Scan(&indexSeq, &cid, &name); err != nil {
					t.Fatalf("scan index_info(%s): %v", tableShape.Indexes[i].Name, err)
				}
				tableShape.Indexes[i].Columns = append(tableShape.Indexes[i].Columns, name)
			}
			if err := indexRows.Close(); err != nil {
				t.Fatalf("close index_info(%s): %v", tableShape.Indexes[i].Name, err)
			}
		}
		sort.Slice(tableShape.Indexes, func(i, j int) bool {
			return tableShape.Indexes[i].Name < tableShape.Indexes[j].Name
		})

		var createSQL string
		if err := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&createSQL); err != nil {
			t.Fatalf("table SQL(%s): %v", table, err)
		}
		tableShape.Autoincrement = strings.Contains(strings.ToUpper(createSQL), "AUTOINCREMENT")
		shape[table] = tableShape
	}
	data, err := json.Marshal(shape)
	if err != nil {
		t.Fatalf("marshal legacy schema fingerprint: %v", err)
	}
	return string(data)
}
