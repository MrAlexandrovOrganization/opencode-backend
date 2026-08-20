package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}

	now := time.Now().UTC()
	if err := s.EnsureUser(&User{ID: "u1", Name: "админ", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Повторный EnsureUser не перезаписывает.
	if err := s.EnsureUser(&User{ID: "u1", Name: "другой", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUser("u1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "админ" {
		t.Fatalf("name = %q", u.Name)
	}

	if err := s.EnsureAPIKey(&APIKey{TokenHash: "h1", UserID: "u1", Label: "cli", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	k, err := s.UserByTokenHash("h1")
	if err != nil {
		t.Fatal(err)
	}
	if k.UserID != "u1" || k.Revoked {
		t.Fatalf("key = %+v", k)
	}

	sess := &Session{ID: "s1", UserID: "u1", Title: "тест", Directory: "/workspace",
		CreatedAt: now, UpdatedAt: now}
	if err := s.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "тест" || !got.CreatedAt.Equal(now) {
		t.Fatalf("session = %+v", got)
	}

	completed := now.Add(time.Minute)
	msg := &Message{
		ID: "m1", SessionID: "s1", Role: "assistant", Status: StatusCompleted,
		Info: json.RawMessage(`{"cost":0.5}`), Parts: json.RawMessage(`[{"type":"text"}]`),
		CreatedAt: now, CompletedAt: &completed,
	}
	if err := s.SaveMessage(msg); err != nil {
		t.Fatal(err)
	}
	gotMsg, err := s.GetMessage("m1")
	if err != nil {
		t.Fatal(err)
	}
	if gotMsg.Role != "assistant" || gotMsg.Status != StatusCompleted {
		t.Fatalf("message = %+v", gotMsg)
	}
	if gotMsg.CompletedAt == nil || !gotMsg.CompletedAt.Equal(completed) {
		t.Fatalf("completedAt = %v", gotMsg.CompletedAt)
	}
	if string(gotMsg.Info) != `{"cost":0.5}` {
		t.Fatalf("info = %s", gotMsg.Info)
	}

	sesss, err := s.ListSessions("u1")
	if err != nil || len(sesss) != 1 {
		t.Fatalf("list sessions: %d, %v", len(sesss), err)
	}
	msgs, err := s.ListMessages("s1")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list messages: %d, %v", len(msgs), err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLitePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")

	s1, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_ = s1.EnsureUser(&User{ID: "u1", Name: "n", CreatedAt: now})
	_ = s1.EnsureAPIKey(&APIKey{TokenHash: "h", UserID: "u1", CreatedAt: now})
	_ = s1.SaveSession(&Session{ID: "s1", UserID: "u1", Title: "t", Directory: "/workspace",
		CreatedAt: now, UpdatedAt: now})
	_ = s1.SaveMessage(&Message{ID: "m1", SessionID: "s1", Role: "user", Status: StatusPending,
		Parts: json.RawMessage(`[{"type":"text","text":"hi"}]`), CreatedAt: now})
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// Симулируем рестарт сервиса: переоткрываем ту же базу.
	s2, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if u, err := s2.GetUser("u1"); err != nil || u.Name != "n" {
		t.Fatalf("user после рестарта: %+v, %v", u, err)
	}
	if _, err := s2.UserByTokenHash("h"); err != nil {
		t.Fatalf("key после рестарта: %v", err)
	}
	sess, err := s2.GetSession("s1")
	if err != nil || sess.Title != "t" {
		t.Fatalf("session после рестарта: %+v, %v", sess, err)
	}
	msgs, err := s2.ListMessages("s1")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages после рестарта: %d, %v", len(msgs), err)
	}
}

func TestSQLiteErrNotFound(t *testing.T) {
	s, err := NewSQLite(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.GetUser("nope"); err != ErrNotFound {
		t.Fatalf("GetUser: %v, want ErrNotFound", err)
	}
	if _, err := s.GetSession("nope"); err != ErrNotFound {
		t.Fatalf("GetSession: %v, want ErrNotFound", err)
	}
	if _, err := s.GetMessage("nope"); err != ErrNotFound {
		t.Fatalf("GetMessage: %v, want ErrNotFound", err)
	}
	if _, err := s.UserByTokenHash("nope"); err != ErrNotFound {
		t.Fatalf("UserByTokenHash: %v, want ErrNotFound", err)
	}
}

// ensure в памяти и на диске ведут себя одинаково для общих сценариев.
func TestMemoryConformance(t *testing.T) {
	m := NewMemory()
	_ = m.EnsureUser(&User{ID: "u1", Name: "n", CreatedAt: time.Now()})
	_ = m.EnsureAPIKey(&APIKey{TokenHash: "h", UserID: "u1", CreatedAt: time.Now()})
	if _, err := m.GetUser("nope"); err != ErrNotFound {
		t.Fatalf("memory GetUser: %v", err)
	}
}
