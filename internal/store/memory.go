package store

import (
	"sort"
	"sync"
)

// Memory — in-memory реализация Store. Используется до появления SQLite.
type Memory struct {
	mu    sync.RWMutex
	users map[string]*User
	keys  map[string]*APIKey // tokenHash -> key
	sess  map[string]*Session
	msg   map[string]*Message
}

// NewMemory создаёт пустое in-memory хранилище.
func NewMemory() *Memory {
	return &Memory{
		users: make(map[string]*User),
		keys:  make(map[string]*APIKey),
		sess:  make(map[string]*Session),
		msg:   make(map[string]*Message),
	}
}

func (m *Memory) EnsureUser(u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.ID]; !ok {
		m.users[u.ID] = u
	}
	return nil
}

func (m *Memory) GetUser(id string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

func (m *Memory) SaveUser(u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[u.ID] = u
	return nil
}

func (m *Memory) EnsureAPIKey(k *APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[k.TokenHash]; !ok {
		m.keys[k.TokenHash] = k
	}
	return nil
}

func (m *Memory) UserByTokenHash(hash string) (*APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[hash]
	if !ok {
		return nil, ErrNotFound
	}
	return k, nil
}

func (m *Memory) SaveSession(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sess[s.ID] = s
	return nil
}

func (m *Memory) GetSession(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sess[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

func (m *Memory) ListSessions(userID string) ([]*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Session
	for _, s := range m.sess {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) DeleteSession(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sess, id)
	return nil
}

func (m *Memory) SaveMessage(msg *Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msg[msg.ID] = msg
	return nil
}

func (m *Memory) GetMessage(id string) (*Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	msg, ok := m.msg[id]
	if !ok {
		return nil, ErrNotFound
	}
	return msg, nil
}

func (m *Memory) ListMessages(sessionID string) ([]*Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Message
	for _, msg := range m.msg {
		if msg.SessionID == sessionID {
			out = append(out, msg)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ensure Memory implements Store.
var _ Store = (*Memory)(nil)
