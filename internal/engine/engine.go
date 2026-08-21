// Package engine — движок шлюза: пер-пользовательское состояние сессий,
// оркестрация запросов к opencode и нормализация событий SSE в единый
// протокол для всех фронтендов.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"opencode-backend/internal/config"
	"opencode-backend/internal/opencode"
	"opencode-backend/internal/store"
	"opencode-backend/internal/ws"
)

// Ошибки движка, которые API транслирует в HTTP-статусы.
var (
	ErrBusy            = errors.New("сессия занята другим запросом")
	ErrSessionNotFound = errors.New("сессия не найдена")
)

// Engine владеет состоянием всех пользователей и сессий.
type Engine struct {
	oc    *opencode.Client
	store store.Store
	hub   *ws.Hub
	cfg   *config.Config
	log   *slog.Logger

	mu    sync.RWMutex
	users map[string]*UserState // userID -> состояние
	owner map[string]string     // ocSessionID -> userID
}

// UserState — состояние пользователя.
type UserState struct {
	ID               string
	mu               sync.Mutex
	defaultSessionID string
	sessions         map[string]*SessionState
}

// New создаёт движок.
func New(oc *opencode.Client, st store.Store, hub *ws.Hub, cfg *config.Config, log *slog.Logger) *Engine {
	return &Engine{
		oc:    oc,
		store: st,
		hub:   hub,
		cfg:   cfg,
		log:   log,
		users: make(map[string]*UserState),
		owner: make(map[string]string),
	}
}

// EnsureUser регистрирует пользователя в памяти и хранилище.
func (e *Engine) EnsureUser(userID, name string) {
	e.mu.Lock()
	u, ok := e.users[userID]
	if !ok {
		u = &UserState{ID: userID, sessions: make(map[string]*SessionState)}
		e.users[userID] = u
	}
	e.mu.Unlock()
	if !ok {
		_ = e.store.EnsureUser(&store.User{ID: userID, Name: name, CreatedAt: time.Now()})
	}
}

// Start запускает SSE-цикл в фоне.
func (e *Engine) Start(ctx context.Context) {
	go e.runSSELoop(ctx)
}

func (e *Engine) runSSELoop(ctx context.Context) {
	for {
		err := e.oc.Events(ctx, e.handleEvent)
		if ctx.Err() != nil {
			return
		}
		e.log.Warn("event stream dropped", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// ── Состояние ───────────────────────────────────────────────────────────────

func (e *Engine) ownerFor(sessionID string) (string, bool) {
	e.mu.RLock()
	id, ok := e.owner[sessionID]
	e.mu.RUnlock()
	return id, ok
}

func (e *Engine) sessionState(userID, sessionID string) *SessionState {
	e.mu.RLock()
	u := e.users[userID]
	e.mu.RUnlock()
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sessions[sessionID]
}

func (e *Engine) addSession(userID, sessionID string) {
	e.EnsureUser(userID, userID)
	e.mu.Lock()
	e.owner[sessionID] = userID
	e.mu.Unlock()
	u := e.users[userID]
	u.mu.Lock()
	if u.sessions[sessionID] == nil {
		u.sessions[sessionID] = newSessionState(userID, sessionID)
	}
	u.mu.Unlock()
}

// ensureSession возвращает активное состояние сессии. После рестарта шлюза
// in-memory состояние теряется, но сессия и история переживают рестарт в
// хранилище (SQLite) — ensureSession заново активирует сохранённую сессию
// пользователя (регистрирует владельца и состояние), чтобы по ней можно было
// продолжить работу и переключаться между сессиями.
func (e *Engine) ensureSession(userID, sessionID string) (*SessionState, error) {
	if st := e.sessionState(userID, sessionID); st != nil {
		return st, nil
	}
	stored, err := e.store.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	if stored.UserID != userID {
		return nil, ErrSessionNotFound
	}
	e.addSession(userID, sessionID)
	e.updateDefaultSession(userID, sessionID)
	e.log.Info("session resumed from store", "session_id", sessionID)
	return e.sessionState(userID, sessionID), nil
}

func (e *Engine) removeOwner(sessionID string) {
	e.mu.Lock()
	userID, ok := e.owner[sessionID]
	if ok {
		delete(e.owner, sessionID)
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	u := e.users[userID]
	if u != nil {
		u.mu.Lock()
		delete(u.sessions, sessionID)
		u.mu.Unlock()
	}
}

func (e *Engine) updateDefaultSession(userID, sessionID string) {
	u, err := e.store.GetUser(userID)
	if err != nil {
		return
	}
	if u.DefaultSessionID != "" {
		return
	}
	u.DefaultSessionID = sessionID
	_ = e.store.SaveUser(u)
}

// publish рассылает нормализованное событие подключениям пользователя.
func (e *Engine) publish(userID, sessionID, evType string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	e.hub.Publish(userID, sessionID, ws.Event{
		V:       1,
		Type:    evType,
		Session: sessionID,
		Payload: data,
	})
}

// ── Сессии ──────────────────────────────────────────────────────────────────

// CreateSession создаёт сессию opencode для пользователя.
func (e *Engine) CreateSession(ctx context.Context, userID, title string) (*store.Session, error) {
	s, err := e.oc.CreateSession(ctx, title)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	stored := &store.Session{
		ID:        s.ID,
		UserID:    userID,
		Title:     s.Title,
		Directory: s.Directory,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if stored.Title == "" {
		stored.Title = title
	}
	if err := e.store.SaveSession(stored); err != nil {
		return nil, err
	}
	e.addSession(userID, s.ID)
	e.updateDefaultSession(userID, s.ID)
	e.publish(userID, s.ID, "session.created", stored)
	return stored, nil
}

// ForkSession создаёт форк сессии.
func (e *Engine) ForkSession(ctx context.Context, userID, sessionID, messageID string) (*store.Session, error) {
	if _, ok := e.ownerFor(sessionID); !ok {
		return nil, ErrSessionNotFound
	}
	fs, err := e.oc.ForkSession(ctx, sessionID, messageID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	stored := &store.Session{
		ID:        fs.ID,
		UserID:    userID,
		Title:     fs.Title,
		Directory: fs.Directory,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := e.store.SaveSession(stored); err != nil {
		return nil, err
	}
	e.addSession(userID, fs.ID)
	e.publish(userID, fs.ID, "session.created", stored)
	return stored, nil
}

// ListSessions возвращает сессии пользователя.
func (e *Engine) ListSessions(userID string) ([]*store.Session, error) {
	return e.store.ListSessions(userID)
}

// GetSession возвращает сессию пользователя.
func (e *Engine) GetSession(userID, sessionID string) (*store.Session, error) {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return nil, err
	}
	return e.store.GetSession(sessionID)
}

// SessionEmpty возвращает true, если в сессии нет ни одного сообщения.
// Позволяет фронтендам (например, /reset в боте) не плодить новые пустые
// сессии, а переиспользовать уже существующую пустую.
func (e *Engine) SessionEmpty(ctx context.Context, userID, sessionID string) (bool, error) {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return false, err
	}
	msgs, err := e.store.ListMessages(sessionID)
	if err != nil {
		return false, err
	}
	return len(msgs) == 0, nil
}

// ResumeSession активирует существующую сессию пользователя (для переключения
// фронтенда на другую сессию) и возвращает её.
func (e *Engine) ResumeSession(userID, sessionID string) (*store.Session, error) {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return nil, err
	}
	return e.store.GetSession(sessionID)
}

// RenameSession переименовывает сессию.
func (e *Engine) RenameSession(ctx context.Context, userID, sessionID, title string) (*store.Session, error) {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return nil, err
	}
	s, err := e.oc.UpdateSession(ctx, sessionID, title)
	if err != nil {
		return nil, err
	}
	stored, err := e.store.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	stored.Title = s.Title
	stored.UpdatedAt = time.Now()
	_ = e.store.SaveSession(stored)
	e.publish(userID, sessionID, "session.updated", stored)
	return stored, nil
}

// DeleteSession удаляет сессию.
func (e *Engine) DeleteSession(ctx context.Context, userID, sessionID string) error {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return err
	}
	if err := e.oc.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	_ = e.store.DeleteSession(sessionID)
	e.removeOwner(sessionID)
	e.publish(userID, sessionID, "session.deleted", map[string]string{"id": sessionID})
	return nil
}

// ── Сообщения ───────────────────────────────────────────────────────────────

// SendMessage принимает запрос и запускает его выполнение в фоне сессии.
// Возвращает локальный messageID (для 202 Accepted). Прогресс — через события.
func (e *Engine) SendMessage(ctx context.Context, userID, sessionID string, req opencode.MessageRequest) (string, error) {
	if len(req.Parts) == 0 {
		return "", errors.New("нет частей сообщения")
	}
	sess, err := e.ensureSession(userID, sessionID)
	if err != nil {
		return "", err
	}
	if !sess.tryAcquire() {
		return "", ErrBusy
	}

	msgID := newID()
	parts, _ := json.Marshal(req.Parts)
	_ = e.store.SaveMessage(&store.Message{
		ID:        msgID,
		SessionID: sessionID,
		Role:      "user",
		Parts:     parts,
		Status:    store.StatusPending,
		CreatedAt: time.Now(),
	})

	e.publish(userID, sessionID, "message.started", map[string]string{"messageID": msgID})
	go e.runMessage(userID, sessionID, msgID, req)
	return msgID, nil
}

// runMessage выполняет запрос в фоне сессии и сохраняет ответ ассистента.
// При недоступности выбранной модели последовательно перебирает запасные
// (OPENCODE_MODEL_FALLBACK) и в конце — дефолтную модель opencode (nil в запросе).
func (e *Engine) runMessage(userID, sessionID, msgID string, req opencode.MessageRequest) {
	defer func() {
		if sess := e.sessionState(userID, sessionID); sess != nil {
			sess.release()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.RequestTimeout)
	defer cancel()

	now := time.Now()
	msg := &store.Message{
		ID:          msgID,
		SessionID:   sessionID,
		Role:        "assistant",
		Status:      store.StatusCompleted,
		CreatedAt:   now,
		CompletedAt: &now,
	}

	candidates := e.modelCandidates(req.Model)
	var (
		resp    *opencode.MessageResponse
		lastErr error
		usedIdx = -1
	)
	for i, m := range candidates {
		attempt := req
		if m.ModelID != "" {
			attempt.Model = &m
		} else {
			attempt.Model = nil // дефолтная модель opencode
		}
		resp, lastErr = e.oc.SendMessage(ctx, sessionID, attempt)
		if lastErr == nil && resp.MessageError() == "" {
			usedIdx = i
			break
		}
		if lastErr != nil {
			e.log.Warn("model unavailable, trying next", "model", m.String(), "error", lastErr)
		} else {
			e.log.Warn("model returned error, trying next", "model", m.String(), "error", resp.MessageError())
			lastErr = fmt.Errorf("%s", resp.MessageError())
		}
	}

	if usedIdx > 0 {
		e.log.Info("model fallback used", "model", candidates[usedIdx].String())
	}

	if lastErr != nil {
		msg.Status = store.StatusError
		msg.Info = mustJSON(map[string]string{"error": lastErr.Error()})
		e.publish(userID, sessionID, "message.updated", map[string]any{
			"sessionID": sessionID,
			"info": opencode.Message{
				ID:        msgID,
				SessionID: sessionID,
				Role:      "assistant",
				Error: &struct {
					Name string `json:"name"`
					Data struct {
						Message string `json:"message"`
					} `json:"data"`
				}{Name: "Error", Data: struct {
					Message string `json:"message"`
				}{Message: lastErr.Error()}},
			},
		})
	} else {
		if resp.Info.ID != "" {
			msg.ID = resp.Info.ID
		}
		msg.Info = mustJSON(resp.Info)
		msg.Parts = mustJSON(resp.Parts)
		if resp.MessageError() != "" {
			msg.Status = store.StatusError
		}
		// Сохраняем ДО публикации: фронтенд, получив message.updated,
		// сразу дозагружает полный текст из истории.
		_ = e.store.SaveMessage(msg)
		e.publish(userID, sessionID, "message.updated", map[string]any{
			"sessionID": sessionID,
			"info":      assistantMessage(sessionID, resp.Info),
		})
		return
	}
	_ = e.store.SaveMessage(msg)
}

// modelCandidates строит упорядоченный список моделей для одного запроса:
//  1. модель, явно заданная клиентом (если есть);
//  2. OPENCODE_MODEL (если задана);
//  3. OPENCODE_MODEL_FALLBACK по порядку;
//  4. дефолтная модель opencode (пустая ModelRef → поле model не шлётся).
//
// Повторы исключаются. Финальный пустой кандидат гарантирует, что при
// недоступности всех явных моделей запрос уходит без модели — opencode сам
// выбирает свою дефолтную, и сообщение не падает.
func (e *Engine) modelCandidates(requested *opencode.ModelRef) []opencode.ModelRef {
	seen := make(map[string]bool)
	var out []opencode.ModelRef
	add := func(m opencode.ModelRef) {
		key := m.String() // для пустой (дефолт opencode) ключ == ""
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, m)
	}
	if requested != nil {
		add(*requested)
	}
	if e.cfg.DefaultModel != "" {
		add(opencode.ParseModelRef(e.cfg.DefaultModel))
	}
	for _, m := range e.cfg.FallbackModels {
		add(m)
	}
	add(opencode.ModelRef{}) // дефолт opencode — всегда последний запасной
	return out
}

// assistantMessage превращает AssistantInfo в единый тип Message для событий.
func assistantMessage(sessionID string, info opencode.AssistantInfo) opencode.Message {
	m := opencode.Message{
		ID:        info.ID,
		SessionID: sessionID,
		Role:      "assistant",
		Cost:      info.Cost,
		Finish:    info.Finish,
		Error:     info.Error,
	}
	m.Time.Created = info.Time.Created
	if info.Time.Completed > 0 {
		t := info.Time.Completed
		m.Time.Completed = &t
	}
	m.Tokens.Input = info.Tokens.Input
	m.Tokens.Output = info.Tokens.Output
	return m
}

// ListMessages возвращает историю сообщений сессии.
func (e *Engine) ListMessages(userID, sessionID string) ([]*store.Message, error) {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return nil, err
	}
	return e.store.ListMessages(sessionID)
}

// GetMessage возвращает сообщение из истории.
func (e *Engine) GetMessage(userID, sessionID, messageID string) (*store.Message, error) {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return nil, err
	}
	return e.store.GetMessage(messageID)
}

// ── Управление запросом ─────────────────────────────────────────────────────

// AbortSession прерывает выполняющийся запрос сессии.
func (e *Engine) AbortSession(ctx context.Context, userID, sessionID string) error {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return err
	}
	return e.oc.AbortSession(ctx, sessionID)
}

// ReplyPermission отвечает на запрос разрешения.
func (e *Engine) ReplyPermission(ctx context.Context, userID, sessionID, permissionID, response string) error {
	if _, err := e.ensureSession(userID, sessionID); err != nil {
		return err
	}
	if err := e.oc.ReplyPermission(ctx, sessionID, permissionID, response); err != nil {
		return err
	}
	if sess := e.sessionState(userID, sessionID); sess != nil {
		sess.mu.Lock()
		delete(sess.perms, permissionID)
		sess.mu.Unlock()
	}
	e.publish(userID, sessionID, "permission.replied", map[string]string{
		"sessionID": sessionID, "permissionID": permissionID, "response": response,
	})
	return nil
}

// ReplyQuestion отвечает на вопрос агента.
func (e *Engine) ReplyQuestion(ctx context.Context, userID, requestID string, answers [][]string) error {
	if err := e.oc.ReplyQuestion(ctx, requestID, answers); err != nil {
		return err
	}
	sessionID := e.sessionForQuestion(userID, requestID)
	e.publish(userID, sessionID, "question.answered", map[string]string{"questionID": requestID})
	return nil
}

func (e *Engine) sessionForQuestion(userID, requestID string) string {
	e.mu.RLock()
	u := e.users[userID]
	e.mu.RUnlock()
	if u == nil {
		return ""
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for sid, s := range u.sessions {
		if s != nil && s.pending != nil && s.pending.requestID == requestID {
			return sid
		}
	}
	return ""
}

// ── Справочные данные ───────────────────────────────────────────────────────

// Health проверяет доступность opencode-сервера.
func (e *Engine) Health(ctx context.Context) error {
	return e.oc.Health(ctx)
}

// Agents возвращает список агентов opencode.
func (e *Engine) Agents(ctx context.Context) ([]opencode.Agent, error) {
	return e.oc.Agents(ctx)
}

// Providers возвращает провайдеров и модели (сырой JSON сервера).
func (e *Engine) Providers(ctx context.Context) (json.RawMessage, error) {
	return e.oc.Providers(ctx)
}

// ── Утилиты ─────────────────────────────────────────────────────────────────

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
