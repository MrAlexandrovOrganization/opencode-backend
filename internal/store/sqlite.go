package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite" // драйвер pure-Go (CGO_ENABLED=0)
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id                TEXT PRIMARY KEY,
	name              TEXT NOT NULL DEFAULT '',
	default_session_id TEXT NOT NULL DEFAULT '',
	created_at        TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL,
	label      TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	revoked    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	directory  TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	id           TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	role         TEXT NOT NULL,
	status       TEXT NOT NULL,
	info         TEXT,
	parts        TEXT,
	created_at   TEXT NOT NULL,
	completed_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
`

// SQLite — реализация Store на modernc.org/sqlite (pure Go).
type SQLite struct {
	db *sql.DB
}

// NewSQLite открывает (и при необходимости создаёт) базу по пути path.
// Пустой path недопустим — используйте NewMemory для in-memory режима.
func NewSQLite(path string) (*SQLite, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Один писатель — избегаем SQLITE_BUSY при конкуренции.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// Close закрывает соединение с базой.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// ── Пользователи и токены ───────────────────────────────────────────────────

func (s *SQLite) EnsureUser(u *User) error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO users (id, name, default_session_id, created_at)
		VALUES (?, ?, ?, ?)`,
		u.ID, u.Name, u.DefaultSessionID, timeToStr(u.CreatedAt))
	return err
}

func (s *SQLite) GetUser(id string) (*User, error) {
	row := s.db.QueryRow(`
		SELECT id, name, default_session_id, created_at FROM users WHERE id = ?`, id)
	var u User
	var created string
	if err := row.Scan(&u.ID, &u.Name, &u.DefaultSessionID, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = strToTime(created)
	return &u, nil
}

func (s *SQLite) SaveUser(u *User) error {
	_, err := s.db.Exec(`
		INSERT INTO users (id, name, default_session_id, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			default_session_id = excluded.default_session_id`,
		u.ID, u.Name, u.DefaultSessionID, timeToStr(u.CreatedAt))
	return err
}

func (s *SQLite) EnsureAPIKey(k *APIKey) error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO api_keys (token_hash, user_id, label, created_at, revoked)
		VALUES (?, ?, ?, ?, ?)`,
		k.TokenHash, k.UserID, k.Label, timeToStr(k.CreatedAt), boolToInt(k.Revoked))
	return err
}

func (s *SQLite) UserByTokenHash(hash string) (*APIKey, error) {
	row := s.db.QueryRow(`
		SELECT token_hash, user_id, label, created_at, revoked FROM api_keys WHERE token_hash = ?`, hash)
	var k APIKey
	var created string
	var revoked int
	if err := row.Scan(&k.TokenHash, &k.UserID, &k.Label, &created, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k.CreatedAt = strToTime(created)
	k.Revoked = revoked != 0
	return &k, nil
}

// ── Сессии ──────────────────────────────────────────────────────────────────

func (s *SQLite) SaveSession(sess *Session) error {
	_, err := s.db.Exec(`
		INSERT INTO sessions (id, user_id, title, directory, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			user_id = excluded.user_id,
			title = excluded.title,
			directory = excluded.directory,
			updated_at = excluded.updated_at`,
		sess.ID, sess.UserID, sess.Title, sess.Directory,
		timeToStr(sess.CreatedAt), timeToStr(sess.UpdatedAt))
	return err
}

func (s *SQLite) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(`
		SELECT id, user_id, title, directory, created_at, updated_at FROM sessions WHERE id = ?`, id)
	var sess Session
	var created, updated string
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.Title, &sess.Directory, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sess.CreatedAt = strToTime(created)
	sess.UpdatedAt = strToTime(updated)
	return &sess, nil
}

func (s *SQLite) ListSessions(userID string) ([]*Session, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, title, directory, created_at, updated_at FROM sessions
		WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		var sess Session
		var created, updated string
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.Title, &sess.Directory, &created, &updated); err != nil {
			return nil, err
		}
		sess.CreatedAt = strToTime(created)
		sess.UpdatedAt = strToTime(updated)
		out = append(out, &sess)
	}
	return out, rows.Err()
}

func (s *SQLite) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ── Сообщения ───────────────────────────────────────────────────────────────

func (s *SQLite) SaveMessage(m *Message) error {
	var info, parts, completed any
	if len(m.Info) > 0 {
		info = string(m.Info)
	}
	if len(m.Parts) > 0 {
		parts = string(m.Parts)
	}
	if m.CompletedAt != nil {
		completed = timeToStr(*m.CompletedAt)
	}
	_, err := s.db.Exec(`
		INSERT INTO messages (id, session_id, role, status, info, parts, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			session_id = excluded.session_id,
			role = excluded.role,
			status = excluded.status,
			info = excluded.info,
			parts = excluded.parts,
			completed_at = excluded.completed_at`,
		m.ID, m.SessionID, m.Role, m.Status, info, parts, timeToStr(m.CreatedAt), completed)
	return err
}

func (s *SQLite) GetMessage(id string) (*Message, error) {
	row := s.db.QueryRow(`
		SELECT id, session_id, role, status, info, parts, created_at, completed_at FROM messages WHERE id = ?`, id)
	msg, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return msg, err
}

func (s *SQLite) ListMessages(sessionID string) ([]*Message, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, role, status, info, parts, created_at, completed_at FROM messages
		WHERE session_id = ? ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMessage(row scanner) (*Message, error) {
	var m Message
	var info, parts, completed, created sql.NullString
	if err := row.Scan(&m.ID, &m.SessionID, &m.Role, &m.Status,
		&info, &parts, &created, &completed); err != nil {
		return nil, err
	}
	m.CreatedAt = strToTime(created.String)
	if info.Valid && info.String != "" {
		m.Info = json.RawMessage(info.String)
	}
	if parts.Valid && parts.String != "" {
		m.Parts = json.RawMessage(parts.String)
	}
	if completed.Valid {
		t := strToTime(completed.String)
		m.CompletedAt = &t
	}
	return &m, nil
}

// ── Утилиты ─────────────────────────────────────────────────────────────────

func timeToStr(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func strToTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ensure SQLite implements Store.
var _ Store = (*SQLite)(nil)
