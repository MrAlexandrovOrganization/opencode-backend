// Package ws — WebSocket-канал шлюза: хаб с подписками по пользователю и
// сессии, fan-out нормализованных событий и хендлер апгрейда соединения.
package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Event — нормализованное событие, уходящее клиентам через WebSocket.
type Event struct {
	V       int             `json:"v"` // версия протокола
	Type    string          `json:"type"`
	Session string          `json:"session,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Conn — одно клиентское подключение, подписанное на события пользователя.
// sessionID == "" или "*" означает «все сессии пользователя».
type Conn struct {
	hub       *Hub
	conn      *websocket.Conn
	userID    string
	sessionID string
	send      chan Event
	closed    atomic.Bool
}

func (c *Conn) matches(userID, sessionID string) bool {
	if c.userID != userID {
		return false
	}
	return c.sessionID == "" || c.sessionID == "*" || c.sessionID == sessionID
}

// Hub управляет подключениями и рассылает события.
type Hub struct {
	mu    sync.RWMutex
	conns map[*Conn]struct{}
}

// NewHub создаёт пустой хаб.
func NewHub() *Hub {
	return &Hub{conns: make(map[*Conn]struct{})}
}

func (h *Hub) add(c *Conn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// Publish рассылает событие всем подключениям пользователя userID, которые
// подписаны на sessionID (или на все сессии). Медленный потребитель
// отключается.
func (h *Hub) Publish(userID, sessionID string, ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		if c.closed.Load() || !c.matches(userID, sessionID) {
			continue
		}
		select {
		case c.send <- ev:
		default:
			// Клиент не успевает читать — принудительно закрываем.
			c.closed.Store(true)
			if c.conn != nil {
				_ = c.conn.CloseNow()
			}
		}
	}
}

// Subscribe регистрирует программного подписчика (для тестов и будущих
// внутренних клиентов). Возвращает канал событий и функцию отписки.
func (h *Hub) Subscribe(userID, sessionID string) (<-chan Event, func()) {
	c := &Conn{
		hub:       h,
		userID:    userID,
		sessionID: sessionID,
		send:      make(chan Event, 64),
	}
	h.add(c)
	return c.send, func() {
		h.remove(c)
		c.closed.Store(true)
	}
}

const writeTimeout = 10 * time.Second

func (c *Conn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c.send:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err = c.conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (c *Conn) readLoop(ctx context.Context) {
	for {
		if _, _, err := c.conn.Read(ctx); err != nil {
			return
		}
	}
}

// ServeWS апгрейдит HTTP-соединение в WebSocket и обслуживает его до
// закрытия. userID берётся из авторизации, подписка — из query-параметра
// session (или "*" для всех сессий пользователя).
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, userID string) {
	sessionID := r.URL.Query().Get("session")

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // TLS терминация на реверс-прокси
	})
	if err != nil {
		return
	}

	c := &Conn{
		hub:       h,
		conn:      conn,
		userID:    userID,
		sessionID: sessionID,
		send:      make(chan Event, 64),
	}
	h.add(c)
	defer h.remove(c)
	defer conn.CloseNow()

	// Первое событие — server.connected (сверка соединения).
	h.Publish(userID, sessionID, Event{V: 1, Type: "server.connected",
		Payload: mustMarshal(map[string]string{"userId": userID})})

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go c.writeLoop(ctx)
	c.readLoop(ctx)
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
