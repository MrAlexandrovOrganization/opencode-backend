package engine

import (
	"sync"
	"time"

	"opencode-backend/internal/opencode"
)

// Stream отслеживает выполняющийся запрос сессии: частичный текст и текущий
// статус агента. Наполняется из SSE, рендеринг — на стороне клиента.
type Stream struct {
	mu      sync.Mutex
	partial string // накопленный текст текущего ответа
	status  string // краткая строка «что агент делает сейчас»
}

// permAsk — ожидающий запрос разрешения (для дедупликации повторных событий).
type permAsk struct {
	created time.Time
}

// pendingQuestions — серия вопросов агента (инструмент question), ожидающая
// ответа пользователя. Вопросы задаются по одному, ответы накапливаются и
// отправляются разом через /question/{id}/reply.
type pendingQuestions struct {
	requestID string
	questions []opencode.Question
	answers   [][]string
	idx       int
}

// SessionState — состояние одной сессии пользователя. busy гарантирует
// серийность запросов в рамках сессии.
type SessionState struct {
	ocSessionID string
	userID      string

	mu      sync.Mutex
	busy    bool
	stream  *Stream
	perms   map[string]*permAsk // permissionID -> время запроса
	pending *pendingQuestions
	model   *opencode.ModelRef
	agent   string
}

func newSessionState(userID, sessionID string) *SessionState {
	return &SessionState{
		ocSessionID: sessionID,
		userID:      userID,
		perms:       make(map[string]*permAsk),
	}
}

// tryAcquire занимает сессию под новый запрос.
func (s *SessionState) tryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	s.stream = &Stream{}
	return true
}

// release освобождает сессию после завершения запроса.
func (s *SessionState) release() {
	s.mu.Lock()
	s.busy = false
	s.stream = nil
	s.mu.Unlock()
}
