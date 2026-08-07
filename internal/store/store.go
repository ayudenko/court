// Package store — персистентность на SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"court/internal/core"
)

// ErrNotFound возвращается, когда запись не найдена.
var ErrNotFound = errors.New("не найдено")

// currentModerationSchemaVersion evolves independently from the public SSE and
// JSONL protocol version so persisted v1 evidence remains readable after a
// future public protocol bump.
const currentModerationSchemaVersion = 1

// Store — обёртка над SQLite.
type Store struct {
	db                    *sql.DB
	hasAgentKeyHashShadow bool
}

const schema = `
CREATE TABLE IF NOT EXISTS agents (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	persona      TEXT NOT NULL DEFAULT '',
	api_key_hash TEXT NOT NULL UNIQUE,
	created_at   TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_credentials (
	id         TEXT PRIMARY KEY,
	agent_id   TEXT NOT NULL REFERENCES agents(id),
	key_hash   TEXT NOT NULL UNIQUE,
	created_at TIMESTAMP NOT NULL,
	revoked_at TIMESTAMP
);
CREATE TABLE IF NOT EXISTS debates (
	id            TEXT PRIMARY KEY,
	question      TEXT NOT NULL,
	description   TEXT NOT NULL DEFAULT '',
	mode          TEXT NOT NULL DEFAULT 'moderator',
	status        TEXT NOT NULL,
	rounds        INTEGER NOT NULL,
	current_round INTEGER NOT NULL DEFAULT 0,
	turn_timeout  INTEGER NOT NULL,
	prep_time     INTEGER NOT NULL DEFAULT 0,
	creator_id    TEXT NOT NULL REFERENCES agents(id),
	turn_agent_id TEXT NOT NULL DEFAULT '',
	turn_deadline TIMESTAMP,
	consensus     INTEGER NOT NULL DEFAULT 0,
	created_at    TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS participants (
	debate_id TEXT NOT NULL REFERENCES debates(id),
	agent_id  TEXT NOT NULL REFERENCES agents(id),
	stance    TEXT NOT NULL DEFAULT '',
	joined_at TIMESTAMP NOT NULL,
	PRIMARY KEY (debate_id, agent_id)
);
CREATE TABLE IF NOT EXISTS messages (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	debate_id    TEXT NOT NULL REFERENCES debates(id),
	round        INTEGER NOT NULL,
	speaker_id   TEXT NOT NULL DEFAULT '',
	speaker_name TEXT NOT NULL,
	kind         TEXT NOT NULL,
	text         TEXT NOT NULL,
	support_id   TEXT NOT NULL DEFAULT '',
	support_name TEXT NOT NULL DEFAULT '',
	moderation_json TEXT,
	created_at   TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_debate ON messages(debate_id, seq);
CREATE INDEX IF NOT EXISTS idx_debates_status ON debates(status);
CREATE INDEX IF NOT EXISTS idx_agent_credentials_agent ON agent_credentials(agent_id);
`

// Open открывает базу и применяет схему.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("открытие БД: %w", err)
	}
	// SQLite допускает только одного писателя; сериализуем доступ на уровне пула.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("миграция схемы: %w", err)
	}
	// Догоняющие миграции для баз, созданных до появления колонок.
	for _, stmt := range []string{
		`ALTER TABLE debates ADD COLUMN mode TEXT NOT NULL DEFAULT 'moderator'`,
		`ALTER TABLE debates ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE debates ADD COLUMN prep_time INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN support_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN support_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN moderation_json TEXT`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("миграция схемы: %w", err)
		}
	}
	hasAgentKeyHashShadow, err := columnExists(db, "agents", "api_key_hash")
	if err != nil {
		return nil, fmt.Errorf("проверка legacy-схемы credentials: %w", err)
	}
	if hasAgentKeyHashShadow {
		if _, err := db.Exec(`
			INSERT INTO agent_credentials (id, agent_id, key_hash, created_at)
			SELECT 'crd_legacy_' || id, id, api_key_hash, created_at
			FROM agents a
			WHERE api_key_hash <> ''
			  AND NOT EXISTS (
				SELECT 1 FROM agent_credentials c WHERE c.key_hash = a.api_key_hash
			  )
		`); err != nil {
			return nil, fmt.Errorf("миграция legacy credentials: %w", err)
		}
		var missing int
		if err := db.QueryRow(`
			SELECT COUNT(*)
			FROM agents a
			LEFT JOIN agent_credentials c
			  ON c.key_hash = a.api_key_hash AND c.agent_id = a.id
			WHERE a.api_key_hash <> '' AND c.id IS NULL
		`).Scan(&missing); err != nil {
			return nil, fmt.Errorf("проверка legacy credentials: %w", err)
		}
		if missing != 0 {
			return nil, fmt.Errorf("миграция legacy credentials: %d ключей не связаны со своими агентами", missing)
		}
	}
	return &Store{db: db, hasAgentKeyHashShadow: hasAgentKeyHashShadow}, nil
}

// Close закрывает базу.
func (s *Store) Close() error { return s.db.Close() }

// --- Агенты ---

// CreateAgent атомарно сохраняет стабильного агента и его первый credential.
func (s *Store) CreateAgent(a core.Agent, credential core.Credential, keyHash string) error {
	if credential.AgentID != a.ID || credential.ID == "" || keyHash == "" {
		return errors.New("credential не соответствует агенту")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if s.hasAgentKeyHashShadow {
		_, err = tx.Exec(
			`INSERT INTO agents (id, name, persona, api_key_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
			a.ID, a.Name, a.Persona, keyHash, a.CreatedAt,
		)
	} else {
		_, err = tx.Exec(
			`INSERT INTO agents (id, name, persona, created_at) VALUES (?, ?, ?, ?)`,
			a.ID, a.Name, a.Persona, a.CreatedAt,
		)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO agent_credentials (id, agent_id, key_hash, created_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?)`,
		credential.ID, credential.AgentID, keyHash, credential.CreatedAt, nullTimePtr(credential.RevokedAt),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateCredential добавляет ещё один независимо отзываемый ключ агента.
func (s *Store) CreateCredential(credential core.Credential, keyHash string) error {
	if credential.ID == "" || credential.AgentID == "" || keyHash == "" {
		return errors.New("неполный credential")
	}
	_, err := s.db.Exec(
		`INSERT INTO agent_credentials (id, agent_id, key_hash, created_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?)`,
		credential.ID, credential.AgentID, keyHash, credential.CreatedAt, nullTimePtr(credential.RevokedAt),
	)
	return err
}

// RevokeCredential запрещает дальнейшую аутентификацию по credential.
func (s *Store) RevokeCredential(id string, at time.Time) error {
	res, err := s.db.Exec(
		`UPDATE agent_credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, at, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AgentByCredentialHash ищет агента по активному credential.
func (s *Store) AgentByCredentialHash(hash string) (core.Agent, error) {
	return s.scanAgent(s.db.QueryRow(`
		SELECT a.id, a.name, a.persona, a.created_at
		FROM agent_credentials c
		JOIN agents a ON a.id = c.agent_id
		WHERE c.key_hash = ? AND c.revoked_at IS NULL`, hash))
}

// AgentByID ищет агента по идентификатору.
func (s *Store) AgentByID(id string) (core.Agent, error) {
	return s.scanAgent(s.db.QueryRow(
		`SELECT id, name, persona, created_at FROM agents WHERE id = ?`, id))
}

func (s *Store) scanAgent(row *sql.Row) (core.Agent, error) {
	var a core.Agent
	err := row.Scan(&a.ID, &a.Name, &a.Persona, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// --- Дебаты ---

// CreateDebate сохраняет новую дискуссию.
func (s *Store) CreateDebate(d core.Debate) error {
	_, err := s.db.Exec(
		`INSERT INTO debates (id, question, description, mode, status, rounds, current_round, turn_timeout,
		                      prep_time, creator_id, turn_agent_id, turn_deadline, consensus, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Question, d.Description, d.Mode, d.Status, d.Rounds, d.CurrentRound, d.TurnTimeout,
		d.PrepTime,
		d.CreatorID, d.TurnAgentID, nullTime(d.TurnDeadline), boolInt(d.Consensus), d.CreatedAt,
	)
	return err
}

// UpdateDebate обновляет изменяемые поля дискуссии.
func (s *Store) UpdateDebate(d core.Debate) error {
	_, err := s.db.Exec(
		`UPDATE debates SET status = ?, current_round = ?, turn_agent_id = ?,
		                    turn_deadline = ?, consensus = ? WHERE id = ?`,
		d.Status, d.CurrentRound, d.TurnAgentID, nullTime(d.TurnDeadline), boolInt(d.Consensus), d.ID,
	)
	return err
}

// DeleteDebate удаляет дискуссию вместе с участниками и протоколом.
func (s *Store) DeleteDebate(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM messages WHERE debate_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM participants WHERE debate_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM debates WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// GetDebate возвращает дискуссию по id.
func (s *Store) GetDebate(id string) (core.Debate, error) {
	rows, err := s.queryDebates(`WHERE id = ?`, id)
	if err != nil {
		return core.Debate{}, err
	}
	if len(rows) == 0 {
		return core.Debate{}, ErrNotFound
	}
	return rows[0], nil
}

// ListDebates возвращает дискуссии, опционально фильтруя по статусу.
func (s *Store) ListDebates(status string, limit int) ([]core.Debate, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if status != "" {
		return s.queryDebates(`WHERE status = ? ORDER BY created_at DESC LIMIT ?`, status, limit)
	}
	return s.queryDebates(`ORDER BY created_at DESC LIMIT ?`, limit)
}

// ActiveDebates возвращает дискуссии в статусах preparing/running/moderating —
// для тикера дедлайнов и восстановления после рестарта.
func (s *Store) ActiveDebates() ([]core.Debate, error) {
	return s.queryDebates(`WHERE status IN (?, ?, ?)`,
		core.StatusPreparing, core.StatusRunning, core.StatusModerating)
}

func (s *Store) queryDebates(tail string, args ...any) (_ []core.Debate, err error) {
	rows, err := s.db.Query(
		`SELECT id, question, description, mode, status, rounds, current_round, turn_timeout,
		        prep_time, creator_id, turn_agent_id, turn_deadline, consensus, created_at
		 FROM debates `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, rows.Close())
	}()
	var out []core.Debate
	for rows.Next() {
		var d core.Debate
		var deadline sql.NullTime
		var consensus int
		if err := rows.Scan(&d.ID, &d.Question, &d.Description, &d.Mode, &d.Status, &d.Rounds, &d.CurrentRound,
			&d.TurnTimeout, &d.PrepTime, &d.CreatorID, &d.TurnAgentID, &deadline, &consensus, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.TurnDeadline = deadline.Time
		d.Consensus = consensus != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- Участники ---

// AddParticipant добавляет агента в дискуссию.
func (s *Store) AddParticipant(debateID, agentID, stance string, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO participants (debate_id, agent_id, stance, joined_at) VALUES (?, ?, ?, ?)`,
		debateID, agentID, stance, at,
	)
	return err
}

// Participants возвращает участников в порядке присоединения — это порядок ходов.
func (s *Store) Participants(debateID string) (_ []core.Participant, err error) {
	rows, err := s.db.Query(
		`SELECT p.agent_id, a.name, p.stance, p.joined_at
		 FROM participants p JOIN agents a ON a.id = p.agent_id
		 WHERE p.debate_id = ? ORDER BY p.joined_at, p.agent_id`, debateID)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, rows.Close())
	}()
	var out []core.Participant
	for rows.Next() {
		var p core.Participant
		if err := rows.Scan(&p.AgentID, &p.Name, &p.Stance, &p.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- Сообщения ---

// AddMessage сохраняет запись протокола и возвращает её порядковый номер.
func (s *Store) AddMessage(m core.Message) (int64, error) {
	moderationJSON, err := encodeModeration(m)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(
		`INSERT INTO messages (debate_id, round, speaker_id, speaker_name, kind, text,
		                       support_id, support_name, moderation_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.DebateID, m.Round, m.SpeakerID, m.SpeakerName, m.Kind, m.Text,
		m.SupportID, m.SupportName, nullString(moderationJSON), m.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Messages возвращает протокол дискуссии после указанного seq.
func (s *Store) Messages(debateID string, afterSeq int64) (_ []core.Message, err error) {
	rows, err := s.db.Query(
		`SELECT seq, debate_id, round, speaker_id, speaker_name, kind, text,
		        support_id, support_name, moderation_json, created_at
		 FROM messages WHERE debate_id = ? AND seq > ? ORDER BY seq`, debateID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, rows.Close())
	}()
	var out []core.Message
	for rows.Next() {
		var m core.Message
		var moderationJSON sql.NullString
		if err := rows.Scan(&m.Seq, &m.DebateID, &m.Round, &m.SpeakerID,
			&m.SpeakerName, &m.Kind, &m.Text, &m.SupportID, &m.SupportName, &moderationJSON, &m.CreatedAt); err != nil {
			return nil, err
		}
		if err := decodeModeration(&m, moderationJSON.String); err != nil {
			return nil, fmt.Errorf("message %d moderation payload: %w", m.Seq, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func encodeModeration(m core.Message) (string, error) {
	if m.RoundSummary != nil && m.Verdict != nil {
		return "", errors.New("message cannot contain both round_summary and verdict")
	}
	payload := storedModerationV1{SchemaVersion: currentModerationSchemaVersion}
	switch m.Kind {
	case core.KindSummary:
		if m.RoundSummary == nil || m.Verdict != nil {
			return "", errors.New("new summary message requires exactly one round_summary payload")
		}
		roundSummary := storedRoundSummaryV1FromCore(*m.RoundSummary)
		payload.RoundSummary = &roundSummary
	case core.KindVerdict:
		if m.Verdict == nil || m.RoundSummary != nil {
			return "", errors.New("new verdict message requires exactly one verdict payload")
		}
		verdict := storedVerdictV1FromCore(*m.Verdict)
		payload.Verdict = &verdict
	case core.KindArgument, core.KindSystem:
		if m.RoundSummary != nil || m.Verdict != nil {
			return "", fmt.Errorf("message kind %q cannot contain moderation payload", m.Kind)
		}
		return "", nil
	default:
		return "", fmt.Errorf("unsupported message kind %q", m.Kind)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode moderation payload: %w", err)
	}
	return string(data), nil
}

func decodeModeration(m *core.Message, payload string) error {
	if payload == "" {
		m.LegacyUnstructured = m.Kind == core.KindSummary || m.Kind == core.KindVerdict
		return nil
	}
	var stored storedModerationV1
	if err := json.Unmarshal([]byte(payload), &stored); err != nil {
		return err
	}
	if stored.SchemaVersion != currentModerationSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", stored.SchemaVersion)
	}
	if stored.RoundSummary != nil && stored.Verdict != nil {
		return errors.New("moderation payload contains both round_summary and verdict")
	}
	switch {
	case m.Kind == core.KindSummary && stored.RoundSummary != nil:
		roundSummary := stored.RoundSummary.toCore()
		m.RoundSummary = &roundSummary
	case m.Kind == core.KindVerdict && stored.Verdict != nil:
		verdict := stored.Verdict.toCore()
		m.Verdict = &verdict
	default:
		return fmt.Errorf("kind %q does not match moderation payload", m.Kind)
	}
	return nil
}

// The v1 durable DTOs intentionally do not embed core/public protocol types.
// Their JSON shape is immutable for the lifetime of durable schema v1; changes
// to core JSON tags therefore cannot silently reinterpret already stored rows.
type storedModerationV1 struct {
	SchemaVersion int                   `json:"schema_version"`
	RoundSummary  *storedRoundSummaryV1 `json:"round_summary,omitempty"`
	Verdict       *storedVerdictV1      `json:"verdict,omitempty"`
}

type storedClaimV1 struct {
	Text      string  `json:"text"`
	Citations []int64 `json:"citations"`
}

type storedRoundSummaryV1 struct {
	Summary             string          `json:"summary"`
	Claims              []storedClaimV1 `json:"claims"`
	UnresolvedQuestions []string        `json:"unresolved_questions"`
	Decisions           []string        `json:"decisions"`
	Consensus           bool            `json:"consensus"`
}

type storedVerdictV1 struct {
	FinalAnswer         string          `json:"final_answer"`
	Claims              []storedClaimV1 `json:"claims"`
	UnresolvedQuestions []string        `json:"unresolved_questions"`
	Decisions           []string        `json:"decisions"`
	Consensus           bool            `json:"consensus"`
}

func storedClaimsV1FromCore(claims []core.ModerationClaim) []storedClaimV1 {
	result := make([]storedClaimV1, len(claims))
	for i, claim := range claims {
		result[i] = storedClaimV1{Text: claim.Text, Citations: claim.Citations}
	}
	return result
}

func storedClaimsV1ToCore(claims []storedClaimV1) []core.ModerationClaim {
	result := make([]core.ModerationClaim, len(claims))
	for i, claim := range claims {
		result[i] = core.ModerationClaim{Text: claim.Text, Citations: claim.Citations}
	}
	return result
}

func storedRoundSummaryV1FromCore(summary core.RoundSummary) storedRoundSummaryV1 {
	return storedRoundSummaryV1{
		Summary:             summary.Summary,
		Claims:              storedClaimsV1FromCore(summary.Claims),
		UnresolvedQuestions: summary.UnresolvedQuestions,
		Decisions:           summary.Decisions,
		Consensus:           summary.Consensus,
	}
}

func (summary storedRoundSummaryV1) toCore() core.RoundSummary {
	return core.RoundSummary{
		Summary:             summary.Summary,
		Claims:              storedClaimsV1ToCore(summary.Claims),
		UnresolvedQuestions: summary.UnresolvedQuestions,
		Decisions:           summary.Decisions,
		Consensus:           summary.Consensus,
	}
}

func storedVerdictV1FromCore(verdict core.ModerationVerdict) storedVerdictV1 {
	return storedVerdictV1{
		FinalAnswer:         verdict.FinalAnswer,
		Claims:              storedClaimsV1FromCore(verdict.Claims),
		UnresolvedQuestions: verdict.UnresolvedQuestions,
		Decisions:           verdict.Decisions,
		Consensus:           verdict.Consensus,
	}
}

func (verdict storedVerdictV1) toCore() core.ModerationVerdict {
	return core.ModerationVerdict{
		FinalAnswer:         verdict.FinalAnswer,
		Claims:              storedClaimsV1ToCore(verdict.Claims),
		UnresolvedQuestions: verdict.UnresolvedQuestions,
		Decisions:           verdict.Decisions,
		Consensus:           verdict.Consensus,
	}
}

func columnExists(db *sql.DB, table, column string) (_ bool, err error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, rows.Close())
	}()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
