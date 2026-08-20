package engine

import (
	"encoding/json"
	"time"

	"opencode-backend/internal/opencode"
)

// handleEvent диспетчеризует события SSE-шины opencode. События нормализуются
// и пере-рассылаются только владельцу сессии (маршрутизация по owner map).
func (e *Engine) handleEvent(ev opencode.Event) {
	switch ev.Type {
	case "message.part.updated":
		e.onMessagePartUpdated(ev)
	case "message.updated":
		e.onMessageUpdated(ev)
	case "permission.updated", "permission.asked":
		e.onPermission(ev)
	case "permission.replied":
		e.onPermissionReplied(ev)
	case "question.asked":
		e.onQuestion(ev)
	case "session.status":
		e.onSessionStatus(ev)
	case "session.updated":
		e.onSessionUpdated(ev)
	case "session.deleted":
		e.onSessionDeleted(ev)
	case "session.error":
		e.onSessionError(ev)
	}
}

func (e *Engine) onMessagePartUpdated(ev opencode.Event) {
	var props struct {
		Part struct {
			ID        string          `json:"id"`
			SessionID string          `json:"sessionID"`
			MessageID string          `json:"messageID"`
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Tool      string          `json:"tool"`
			State     json.RawMessage `json:"state"`
		} `json:"part"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(ev.Properties, &props); err != nil {
		e.log.Warn("parse message.part.updated", "error", err)
		return
	}
	sessionID := props.Part.SessionID
	userID, ok := e.ownerFor(sessionID)
	if !ok || sessionID == "" {
		return
	}

	if st := e.streamFor(sessionID); st != nil {
		st.mu.Lock()
		switch props.Part.Type {
		case "text":
			st.status = ""
			if props.Delta != "" {
				st.partial += props.Delta
			} else {
				st.partial = props.Part.Text
			}
		case "tool":
			st.status = toolStatus(props.Part.Tool, props.Part.State)
		case "step-start", "reasoning":
			st.status = ""
		}
		st.mu.Unlock()
	}

	e.publish(userID, sessionID, "message.part.updated", map[string]any{
		"sessionID": sessionID,
		"messageID": props.Part.MessageID,
		"part":      props.Part,
		"delta":     props.Delta,
	})
}

// streamFor возвращает активный поток сессии (или nil, если сессия не занята).
func (e *Engine) streamFor(sessionID string) *Stream {
	userID, ok := e.ownerFor(sessionID)
	if !ok {
		return nil
	}
	sess := e.sessionState(userID, sessionID)
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if !sess.busy {
		return nil
	}
	return sess.stream
}

// toolStatus формирует краткую строку «что агент делает сейчас».
func toolStatus(tool string, state json.RawMessage) string {
	var s struct {
		Status string         `json:"status"`
		Title  string         `json:"title"`
		Input  map[string]any `json:"input"`
	}
	if err := json.Unmarshal(state, &s); err != nil {
		return ""
	}
	if s.Status != "pending" && s.Status != "running" {
		return ""
	}
	if s.Title != "" {
		return "⚙️ " + s.Title
	}
	return "⚙️ " + tool
}

func (e *Engine) onMessageUpdated(ev opencode.Event) {
	var props struct {
		Info opencode.Message `json:"info"`
	}
	if err := json.Unmarshal(ev.Properties, &props); err != nil {
		e.log.Warn("parse message.updated", "error", err)
		return
	}
	if props.Info.SessionID == "" {
		return
	}
	userID, ok := e.ownerFor(props.Info.SessionID)
	if !ok {
		return
	}
	e.publish(userID, props.Info.SessionID, "message.updated", props)
}

func (e *Engine) onPermission(ev opencode.Event) {
	var p opencode.PermissionAsked
	if err := json.Unmarshal(ev.Properties, &p); err != nil {
		e.log.Warn("parse permission", "error", err)
		return
	}
	if p.SessionID == "" {
		return
	}
	userID, ok := e.ownerFor(p.SessionID)
	if !ok {
		return
	}
	if p.ID == "" {
		return
	}

	// Дедупликация повторных событий.
	if sess := e.sessionState(userID, p.SessionID); sess != nil {
		sess.mu.Lock()
		if _, dup := sess.perms[p.ID]; dup {
			sess.mu.Unlock()
			return
		}
		sess.perms[p.ID] = &permAsk{created: time.Now()}
		sess.mu.Unlock()
	}

	e.publish(userID, p.SessionID, "permission.asked", p)
}

func (e *Engine) onPermissionReplied(ev opencode.Event) {
	var props struct {
		SessionID    string `json:"sessionID"`
		PermissionID string `json:"permissionID"`
		Response     string `json:"response"`
	}
	if err := json.Unmarshal(ev.Properties, &props); err != nil {
		return
	}
	userID, ok := e.ownerFor(props.SessionID)
	if !ok {
		return
	}
	if sess := e.sessionState(userID, props.SessionID); sess != nil {
		sess.mu.Lock()
		delete(sess.perms, props.PermissionID)
		sess.mu.Unlock()
	}
	e.publish(userID, props.SessionID, "permission.replied", props)
}

func (e *Engine) onQuestion(ev opencode.Event) {
	var q opencode.QuestionAsked
	if err := json.Unmarshal(ev.Properties, &q); err != nil {
		e.log.Warn("parse question", "error", err)
		return
	}
	if q.SessionID == "" || len(q.Questions) == 0 {
		return
	}
	userID, ok := e.ownerFor(q.SessionID)
	if !ok {
		return
	}
	if sess := e.sessionState(userID, q.SessionID); sess != nil {
		sess.mu.Lock()
		sess.pending = &pendingQuestions{
			requestID: q.ID,
			questions: q.Questions,
			answers:   make([][]string, len(q.Questions)),
		}
		sess.mu.Unlock()
	}
	e.publish(userID, q.SessionID, "question.asked", q)
}

func (e *Engine) onSessionStatus(ev opencode.Event) {
	var props struct {
		SessionID string                 `json:"sessionID"`
		Status    opencode.SessionStatus `json:"status"`
	}
	if err := json.Unmarshal(ev.Properties, &props); err != nil {
		return
	}
	userID, ok := e.ownerFor(props.SessionID)
	if !ok {
		return
	}
	e.publish(userID, props.SessionID, "session.status", props)
}

func (e *Engine) onSessionUpdated(ev opencode.Event) {
	var props struct {
		Info opencode.Session `json:"info"`
	}
	if err := json.Unmarshal(ev.Properties, &props); err != nil {
		return
	}
	userID, ok := e.ownerFor(props.Info.ID)
	if !ok {
		return
	}
	e.publish(userID, props.Info.ID, "session.updated", props.Info)
}

func (e *Engine) onSessionDeleted(ev opencode.Event) {
	var props struct {
		Info opencode.Session `json:"info"`
	}
	if err := json.Unmarshal(ev.Properties, &props); err != nil {
		return
	}
	userID, ok := e.ownerFor(props.Info.ID)
	if !ok {
		return
	}
	_ = e.store.DeleteSession(props.Info.ID)
	e.removeOwner(props.Info.ID)
	e.publish(userID, props.Info.ID, "session.deleted", props.Info)
}

func (e *Engine) onSessionError(ev opencode.Event) {
	var props struct {
		SessionID string          `json:"sessionID"`
		Error     json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(ev.Properties, &props); err != nil {
		return
	}
	userID, ok := e.ownerFor(props.SessionID)
	if !ok {
		return
	}
	e.publish(userID, props.SessionID, "session.error", props)
}
