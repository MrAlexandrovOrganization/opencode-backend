// Package store описывает хранилище шлюза: пользователи, токены, сессии
// и история сообщений. Интерфейс отделён от реализации, чтобы позже
// подставить SQLite без изменения движка и API.
package store

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound возвращается, когда запись отсутствует.
var ErrNotFound = errors.New("not found")

// User — пользователь шлюза.
type User struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	DefaultSessionID string    `json:"defaultSessionID"`
	CreatedAt        time.Time `json:"createdAt"`
}

// APIKey — токен пользователя (хранится только SHA-256 хэш).
type APIKey struct {
	TokenHash string    `json:"-"`
	UserID    string    `json:"userID"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"createdAt"`
	Revoked   bool      `json:"revoked"`
}

// Session — сессия opencode в терминах шлюза.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userID"`
	Title     string    `json:"title"`
	Directory string    `json:"directory"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// MessageStatus — статус сообщения в истории шлюза.
type MessageStatus string

const (
	StatusPending   MessageStatus = "pending"
	StatusCompleted MessageStatus = "completed"
	StatusError     MessageStatus = "error"
)

// Message — сообщение в истории (пользовательское или ассистента).
type Message struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"sessionID"`
	Role        string          `json:"role"` // user | assistant
	Status      MessageStatus   `json:"status"`
	Info        json.RawMessage `json:"info,omitempty"`
	Parts       json.RawMessage `json:"parts,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	CompletedAt *time.Time      `json:"completedAt,omitempty"`
}

// Store — хранилище шлюза.
type Store interface {
	// Пользователи и токены.
	EnsureUser(u *User) error
	GetUser(id string) (*User, error)
	SaveUser(u *User) error
	EnsureAPIKey(k *APIKey) error
	UserByTokenHash(hash string) (*APIKey, error)

	// Сессии.
	SaveSession(s *Session) error
	GetSession(id string) (*Session, error)
	ListSessions(userID string) ([]*Session, error)
	DeleteSession(id string) error

	// Сообщения.
	SaveMessage(m *Message) error
	GetMessage(id string) (*Message, error)
	ListMessages(sessionID string) ([]*Message, error)
}
