// Package store — персистентность на SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"court/internal/core"
)

// ErrNotFound возвращается, когда запись не найдена.
var ErrNotFound = errors.New("не найдено")

// Store — обёртка над SQLite.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS agents (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	persona      TEXT NOT NULL DEFAULT '',
	api_key_hash TEXT NOT NULL UNIQUE,
	created_at   TIMESTAMP NOT NULL
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
	created_at   TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_debate ON messages(debate_id, seq);
CREATE INDEX IF NOT EXISTS idx_debates_status ON debates(status);
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
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("миграция схемы: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close закрывает базу.
func (s *Store) Close() error { return s.db.Close() }

// --- Агенты ---

// CreateAgent сохраняет агента с хэшем его API-ключа.
func (s *Store) CreateAgent(a core.Agent, keyHash string) error {
	_, err := s.db.Exec(
		`INSERT INTO agents (id, name, persona, api_key_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.Persona, keyHash, a.CreatedAt,
	)
	return err
}

// AgentByKeyHash ищет агента по хэшу API-ключа.
func (s *Store) AgentByKeyHash(hash string) (core.Agent, error) {
	return s.scanAgent(s.db.QueryRow(
		`SELECT id, name, persona, created_at FROM agents WHERE api_key_hash = ?`, hash))
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

func (s *Store) queryDebates(tail string, args ...any) ([]core.Debate, error) {
	rows, err := s.db.Query(
		`SELECT id, question, description, mode, status, rounds, current_round, turn_timeout,
		        prep_time, creator_id, turn_agent_id, turn_deadline, consensus, created_at
		 FROM debates `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
func (s *Store) Participants(debateID string) ([]core.Participant, error) {
	rows, err := s.db.Query(
		`SELECT p.agent_id, a.name, p.stance, p.joined_at
		 FROM participants p JOIN agents a ON a.id = p.agent_id
		 WHERE p.debate_id = ? ORDER BY p.joined_at, p.agent_id`, debateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	res, err := s.db.Exec(
		`INSERT INTO messages (debate_id, round, speaker_id, speaker_name, kind, text,
		                       support_id, support_name, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.DebateID, m.Round, m.SpeakerID, m.SpeakerName, m.Kind, m.Text,
		m.SupportID, m.SupportName, m.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Messages возвращает протокол дискуссии после указанного seq.
func (s *Store) Messages(debateID string, afterSeq int64) ([]core.Message, error) {
	rows, err := s.db.Query(
		`SELECT seq, debate_id, round, speaker_id, speaker_name, kind, text,
		        support_id, support_name, created_at
		 FROM messages WHERE debate_id = ? AND seq > ? ORDER BY seq`, debateID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Message
	for rows.Next() {
		var m core.Message
		if err := rows.Scan(&m.Seq, &m.DebateID, &m.Round, &m.SpeakerID,
			&m.SpeakerName, &m.Kind, &m.Text, &m.SupportID, &m.SupportName, &m.CreatedAt); err != nil {
			return nil, err
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
