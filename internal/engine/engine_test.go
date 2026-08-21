package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-backend/internal/config"
	"opencode-backend/internal/opencode"
	"opencode-backend/internal/store"
	"opencode-backend/internal/ws"
)

// fakeServer имитирует opencode-сервер для тестов движка.
type fakeServer struct {
	mu        sync.Mutex
	srv       *httptest.Server
	lastID    string // последняя созданная сессия
	msgHits   chan struct{}
	replies   []string
	delay     time.Duration // задержка ответа POST /session/{id}/message
	permReply []string      // полученные response на permission
	qReply    [][]string    // полученные answers на question
}

func newFakeServer() *fakeServer {
	f := &fakeServer{msgHits: make(chan struct{}, 10)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/global/health":
		writeJSON(w, map[string]bool{"healthy": true})

	case r.Method == http.MethodPost && r.URL.Path == "/session":
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastID = "sess" + time.Now().Format("150405.000000")
		id := f.lastID
		f.mu.Unlock()
		writeJSON(w, map[string]any{
			"id": id, "title": body.Title, "directory": "/workspace",
			"time": map[string]int64{"created": time.Now().UnixMilli()},
		})

	case r.Method == http.MethodPost &&
		strings.HasPrefix(r.URL.Path, "/session/") && strings.HasSuffix(r.URL.Path, "/message"):
		f.mu.Lock()
		f.msgHits <- struct{}{}
		delay := f.delay
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if len(f.replies) > 0 {
			writeJSON(w, json.RawMessage(f.replies[0]))
		} else {
			writeJSON(w, map[string]any{
				"info": map[string]any{"id": "m1", "finish": "end", "cost": 0.5,
					"tokens": map[string]int{"input": 10, "output": 20}},
				"parts": []map[string]string{{"type": "text", "text": "привет"}},
			})
		}

	case r.URL.Path == "/event":
		// SSE: ждём вызова message, шлём живой текст, держим поток открытым.
		select {
		case <-f.msgHits:
		case <-r.Context().Done():
			return
		}
		f.mu.Lock()
		id := f.lastID
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		part, _ := json.Marshal(map[string]any{
			"type": "message.part.updated",
			"properties": map[string]any{
				"part": map[string]any{
					"sessionID": id, "messageID": "m1", "type": "text", "text": "при",
				},
				"delta": "при",
			},
		})
		_, _ = w.Write([]byte("data: " + string(part) + "\n\n"))
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()

	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/permissions/"):
		var body struct {
			Response string `json:"response"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.permReply = append(f.permReply, body.Response)
		f.mu.Unlock()
		writeJSON(w, true)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reply"):
		var body struct {
			Answers [][]string `json:"answers"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.qReply = append(f.qReply, body.Answers...)
		f.mu.Unlock()
		writeJSON(w, true)

	default:
		writeJSON(w, map[string]string{"error": "not found"})
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustMarshal: %v", err)
	}
	return data
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testEngine(t *testing.T, f *fakeServer) (*Engine, *ws.Hub, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	hub := ws.NewHub()
	oc := opencode.New(f.srv.URL, "opencode", "")
	cfg := &config.Config{RequestTimeout: 5 * time.Second, WorkspaceDir: "/workspace"}
	eng := New(oc, st, hub, cfg, testLogger())
	eng.EnsureUser("u1", "u1")
	ctx, cancel := context.WithCancel(context.Background())
	eng.Start(ctx)
	t.Cleanup(func() {
		cancel() // останавливает SSE-цикл
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return eng, hub, st
}

func TestCreateSession(t *testing.T) {
	f := newFakeServer()
	eng, hub, st := testEngine(t, f)
	ch, unsub := hub.Subscribe("u1", "*")
	defer unsub()

	sess, err := eng.CreateSession(context.Background(), "u1", "тест")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.UserID != "u1" {
		t.Fatalf("owner = %q, want u1", sess.UserID)
	}
	got, err := st.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Title != "тест" {
		t.Fatalf("title = %q", got.Title)
	}
	u, _ := st.GetUser("u1")
	if u.DefaultSessionID != sess.ID {
		t.Fatalf("default session = %q", u.DefaultSessionID)
	}
	assertEvent(t, ch, "session.created")
}

func TestResumeSessionAfterRestart(t *testing.T) {
	f := newFakeServer()
	st := store.NewMemory()
	hub := ws.NewHub()
	oc := opencode.New(f.srv.URL, "opencode", "")
	cfg := &config.Config{RequestTimeout: 5 * time.Second, WorkspaceDir: "/workspace"}

	eng1 := New(oc, st, hub, cfg, testLogger())
	eng1.EnsureUser("u1", "u1")
	ctx1, cancel1 := context.WithCancel(context.Background())
	eng1.Start(ctx1)
	defer cancel1()

	sess, err := eng1.CreateSession(context.Background(), "u1", "рабочая")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// «Рестарт» шлюза: новый движок на том же хранилище, in-memory состояние
	// (owner/sessionState) пусто — сессия живёт только в SQLite/Memory.
	cancel1()
	eng2 := New(oc, st, hub, cfg, testLogger())
	eng2.EnsureUser("u1", "u1")

	// GetSession сам активирует сохранённую сессию.
	if _, err := eng2.GetSession("u1", sess.ID); err != nil {
		t.Fatalf("GetSession после рестарта: %v", err)
	}
	// ResumeSession возвращает сессию и делает её активной для SendMessage.
	resumed, err := eng2.ResumeSession("u1", sess.ID)
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if resumed.ID != sess.ID {
		t.Fatalf("resumed = %q, want %q", resumed.ID, sess.ID)
	}

	var req opencode.MessageRequest
	req.AddText("продолжаем")
	if _, err := eng2.SendMessage(context.Background(), "u1", sess.ID, req); err != nil {
		t.Fatalf("SendMessage после resume: %v", err)
	}

	// Чужая сессия не резюмируется.
	if _, err := eng2.ResumeSession("u2", sess.ID); err != ErrSessionNotFound {
		t.Fatalf("ResumeSession чужой сессии: %v, want ErrSessionNotFound", err)
	}
}

func TestSendMessageAsyncFlow(t *testing.T) {
	f := newFakeServer()
	eng, hub, st := testEngine(t, f)

	ctx := context.Background()
	sess, err := eng.CreateSession(ctx, "u1", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ch, unsub := hub.Subscribe("u1", sess.ID)
	defer unsub()

	var req opencode.MessageRequest
	req.AddText("привет")
	msgID, err := eng.SendMessage(ctx, "u1", sess.ID, req)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if msgID == "" {
		t.Fatal("пустой messageID")
	}
	assertEvent(t, ch, "message.started")

	// Живая часть (из SSE) и завершение.
	waitEvent(t, ch, "message.part.updated", 5*time.Second)
	waitEvent(t, ch, "message.updated", 5*time.Second)

	msgs, err := st.ListMessages(sess.ID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("сообщений = %d, want 2 (user+assistant)", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Status != store.StatusPending {
		t.Fatalf("первое сообщение не user/pending: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Status != store.StatusCompleted {
		t.Fatalf("второе сообщение не assistant/completed: %+v", msgs[1])
	}

	// Сессия снова свободна.
	if _, err := eng.SendMessage(ctx, "u1", sess.ID, req); err != nil {
		t.Fatalf("повторный SendMessage после завершения: %v", err)
	}
}

func TestBusyConflict(t *testing.T) {
	f := newFakeServer()
	f.delay = 500 * time.Millisecond
	eng, hub, _ := testEngine(t, f)

	ctx := context.Background()
	sess, err := eng.CreateSession(ctx, "u1", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	var req opencode.MessageRequest
	req.AddText("а")
	if _, err := eng.SendMessage(ctx, "u1", sess.ID, req); err != nil {
		t.Fatalf("первый SendMessage: %v", err)
	}
	// busy выставляется синхронно — второй запрос обязан получить ErrBusy.
	if _, err := eng.SendMessage(ctx, "u1", sess.ID, req); err != ErrBusy {
		t.Fatalf("второй SendMessage: %v, want ErrBusy", err)
	}

	// Дожидаемся завершения, чтобы фоновый goroutine не висел.
	ch, unsub := hub.Subscribe("u1", sess.ID)
	defer unsub()
	waitEvent(t, ch, "message.updated", 3*time.Second)
}

func TestPermissionAndQuestion(t *testing.T) {
	f := newFakeServer()
	eng, hub, _ := testEngine(t, f)

	ctx := context.Background()
	sess, err := eng.CreateSession(ctx, "u1", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ch, unsub := hub.Subscribe("u1", sess.ID)
	defer unsub()

	// permission.updated → нормализуем в permission.asked и шлём владельцу.
	permProps := json.RawMessage(mustMarshal(t, map[string]any{
		"id": "perm1", "sessionID": sess.ID, "permission": "bash",
		"pattern": "npm install *",
	}))
	eng.handleEvent(opencode.Event{Type: "permission.updated", Properties: permProps})
	assertEvent(t, ch, "permission.asked")

	// Дубликат не должен прийти повторно.
	eng.handleEvent(opencode.Event{Type: "permission.asked", Properties: permProps})
	assertNoEvent(t, ch, "permission.asked")

	// Ответ на permission уходит на сервер.
	if err := eng.ReplyPermission(ctx, "u1", sess.ID, "perm1", "once"); err != nil {
		t.Fatalf("ReplyPermission: %v", err)
	}
	f.mu.Lock()
	replies := append([]string(nil), f.permReply...)
	f.mu.Unlock()
	if len(replies) != 1 || replies[0] != "once" {
		t.Fatalf("permission replies = %v", replies)
	}
	assertEvent(t, ch, "permission.replied")

	// question.asked → пересылаем подписчику.
	qProps := json.RawMessage(mustMarshal(t, map[string]any{
		"id": "q1", "sessionID": sess.ID,
		"questions": []map[string]any{
			{"question": "Что сделать?", "options": []map[string]string{{"label": "да"}}},
		},
	}))
	eng.handleEvent(opencode.Event{Type: "question.asked", Properties: qProps})
	assertEvent(t, ch, "question.asked")

	// Ответ на вопрос.
	if err := eng.ReplyQuestion(ctx, "u1", "q1", [][]string{{"да"}}); err != nil {
		t.Fatalf("ReplyQuestion: %v", err)
	}
	assertEvent(t, ch, "question.answered")
	f.mu.Lock()
	qReplies := append([][]string(nil), f.qReply...)
	f.mu.Unlock()
	if len(qReplies) != 1 {
		t.Fatalf("question replies = %d, want 1", len(qReplies))
	}
}

func TestSessionNotFound(t *testing.T) {
	f := newFakeServer()
	eng, _, _ := testEngine(t, f)

	var req opencode.MessageRequest
	req.AddText("х")
	if _, err := eng.SendMessage(context.Background(), "u1", "no-such-session", req); err != ErrSessionNotFound {
		t.Fatalf("SendMessage: %v, want ErrSessionNotFound", err)
	}
}

// ── Хелперы ─────────────────────────────────────────────────────────────────

func assertEvent(t *testing.T, ch <-chan ws.Event, wantType string) {
	t.Helper()
	ev, ok := waitEvent(t, ch, wantType, 3*time.Second)
	if !ok {
		t.Fatalf("не пришло событие %s", wantType)
	}
	t.Logf("событие %s: %s", wantType, ev.Payload)
}

func waitEvent(t *testing.T, ch <-chan ws.Event, wantType string, timeout time.Duration) (ws.Event, bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			if ev.Type == wantType {
				return ev, true
			}
		case <-deadline:
			return ws.Event{}, false
		}
	}
}

func assertNoEvent(t *testing.T, ch <-chan ws.Event, notType string) {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Type == notType {
			t.Fatalf("неожиданное событие %s", notType)
		}
	case <-time.After(200 * time.Millisecond):
	}
}
