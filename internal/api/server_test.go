package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"opencode-backend/internal/config"
	"opencode-backend/internal/engine"
	"opencode-backend/internal/opencode"
	"opencode-backend/internal/store"
	"opencode-backend/internal/token"
	"opencode-backend/internal/ws"
)

// fakeOC — минимальный имитатор opencode-сервера для API-тестов.
type fakeOC struct {
	srv *httptest.Server
}

func newFakeOC() *fakeOC {
	f := &fakeOC{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *fakeOC) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/global/health":
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"healthy":true}`)

	case r.Method == http.MethodPost && r.URL.Path == "/session":
		var b struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "oc-sess-1", "title": b.Title, "directory": "/workspace",
		})

	case r.Method == http.MethodPost &&
		strings.HasPrefix(r.URL.Path, "/session/") && strings.HasSuffix(r.URL.Path, "/message"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"info": map[string]any{"id": "oc-msg-1", "finish": "end", "cost": 0.1,
				"tokens": map[string]int{"input": 5, "output": 7}},
			"parts": []map[string]string{{"type": "text", "text": "ответ"}},
		})

	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/permissions/"):
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `true`)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAPIEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   func(t *testing.T) store.Store
	}{
		{"memory", func(t *testing.T) store.Store { return store.NewMemory() }},
		{"sqlite", func(t *testing.T) store.Store {
			s, err := store.NewSQLite(filepath.Join(t.TempDir(), "api.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testAPIEndToEnd(t, tc.st(t))
		})
	}
}

func testAPIEndToEnd(t *testing.T, st store.Store) {
	t.Helper()
	f := newFakeOC()
	t.Cleanup(f.srv.Close)

	hub := ws.NewHub()
	oc := opencode.New(f.srv.URL, "opencode", "")
	cfg := &config.Config{WorkspaceDir: t.TempDir(), RequestTimeout: 5 * time.Second}
	eng := engine.New(oc, st, hub, cfg, discardLogger())
	eng.EnsureUser("u1", "u1")

	const admin = "admin-secret"
	if err := st.EnsureAPIKey(&store.APIKey{TokenHash: token.Hash(admin), UserID: "u1", Label: "admin"}); err != nil {
		t.Fatal(err)
	}

	srv := New(eng, hub, st, cfg, discardLogger())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// /healthz доступен без авторизации.
	if code := rawReq(t, ts.URL+"/healthz", "GET", "", nil); code != http.StatusOK {
		t.Fatalf("/healthz = %d", code)
	}

	// /api/v1/* без токена — 401.
	if code := rawReq(t, ts.URL+"/api/v1/me", "GET", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("/me без токена = %d, want 401", code)
	}
	// С неверным токеном — 401.
	if code := rawReq(t, ts.URL+"/api/v1/me", "GET", "Bearer wrong", nil); code != http.StatusUnauthorized {
		t.Fatalf("/me с плохим токеном = %d, want 401", code)
	}

	do := func(method, path string, body any) (int, []byte) {
		return req(t, ts.URL+path, "Bearer "+admin, method, body)
	}

	// /me.
	code, raw := do("GET", "/api/v1/me", nil)
	if code != http.StatusOK {
		t.Fatalf("/me = %d: %s", code, raw)
	}
	var me store.User
	json.Unmarshal(raw, &me)
	if me.ID != "u1" {
		t.Fatalf("me.ID = %q", me.ID)
	}

	// Создание токена.
	code, raw = do("POST", "/api/v1/auth/tokens", map[string]string{"label": "cli"})
	if code != http.StatusCreated {
		t.Fatalf("tokens = %d: %s", code, raw)
	}
	var tokResp struct{ Token string }
	json.Unmarshal(raw, &tokResp)
	if tokResp.Token == "" {
		t.Fatal("пустой токен")
	}
	// Новым токеном тоже работает.
	if code, _ := req(t, ts.URL+"/api/v1/me", "Bearer "+tokResp.Token, "GET", nil); code != http.StatusOK {
		t.Fatalf("/me новым токеном = %d", code)
	}

	// Создание сессии.
	code, raw = do("POST", "/api/v1/sessions", map[string]string{"title": "тест"})
	if code != http.StatusCreated {
		t.Fatalf("sessions = %d: %s", code, raw)
	}
	var sess store.Session
	json.Unmarshal(raw, &sess)
	if sess.ID == "" {
		t.Fatal("пустой sessionID")
	}

	// Отправка сообщения — 202 + messageID.
	code, raw = do("POST", "/api/v1/sessions/"+sess.ID+"/messages",
		map[string]any{"parts": []map[string]string{{"type": "text", "text": "привет"}}})
	if code != http.StatusAccepted {
		t.Fatalf("messages = %d: %s", code, raw)
	}
	var msgResp struct{ MessageID string }
	json.Unmarshal(raw, &msgResp)
	if msgResp.MessageID == "" {
		t.Fatal("пустой messageID")
	}

	// Дожидаемся ответа ассистента в истории.
	deadline := time.Now().Add(5 * time.Second)
	var msgs []store.Message
	for {
		code, raw = do("GET", "/api/v1/sessions/"+sess.ID+"/messages", nil)
		if code != http.StatusOK {
			t.Fatalf("list messages = %d", code)
		}
		msgs = nil
		json.Unmarshal(raw, &msgs)
		if len(msgs) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ассистент не ответил: %d сообщений", len(msgs))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("роли: %q, %q", msgs[0].Role, msgs[1].Role)
	}
	if msgs[1].Status != store.StatusCompleted {
		t.Fatalf("статус ассистента: %q", msgs[1].Status)
	}

	// Ответ на permission.
	code, _ = do("POST", "/api/v1/sessions/"+sess.ID+"/permissions/p1", map[string]string{"response": "once"})
	if code != http.StatusNoContent {
		t.Fatalf("permission reply = %d", code)
	}
	// Неверный response — 400.
	if code, _ := req(t, ts.URL+"/api/v1/sessions/"+sess.ID+"/permissions/p1",
		"Bearer "+admin, "POST", map[string]string{"response": "nope"}); code != http.StatusBadRequest {
		t.Fatalf("permission reply invalid = %d, want 400", code)
	}

	// Несуществующая сессия — 404.
	if code, _ := req(t, ts.URL+"/api/v1/sessions/nope/messages",
		"Bearer "+admin, "POST", map[string]any{"parts": []map[string]string{{"type": "text", "text": "x"}}}); code != http.StatusNotFound {
		t.Fatalf("чужой сессии = %d, want 404", code)
	}
}

// ── Хелперы ─────────────────────────────────────────────────────────────────

// TestWebSocketUpgrade проверяет, что WS-апгрейд проходит сквозь logging- и
// auth-миддлвары (statusWriter должен поддерживать http.Hijacker), а не
// падает с 501.
func TestWebSocketUpgrade(t *testing.T) {
	st := store.NewMemory()
	hub := ws.NewHub()
	oc := opencode.New("http://127.0.0.1:1", "opencode", "")
	cfg := &config.Config{WorkspaceDir: t.TempDir(), RequestTimeout: 5 * time.Second}
	eng := engine.New(oc, st, hub, cfg, discardLogger())
	eng.EnsureUser("u1", "u1")

	const admin = "admin-secret"
	if err := st.EnsureAPIKey(&store.APIKey{TokenHash: token.Hash(admin), UserID: "u1", Label: "admin"}); err != nil {
		t.Fatal(err)
	}

	srv := New(eng, hub, st, cfg, discardLogger())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/ws?session=*", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + admin}},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var ev struct{ Type string }
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("ws payload: %v", err)
	}
	if ev.Type != "server.connected" {
		t.Fatalf("первое событие = %q, want server.connected", ev.Type)
	}
}

func rawReq(t *testing.T, url, method, auth string, body any) int {
	t.Helper()
	_, code := doRawReq(t, url, method, auth, body)
	return code
}

func req(t *testing.T, url, auth, method string, body any) (int, []byte) {
	t.Helper()
	raw, code := doRawReq(t, url, method, auth, body)
	return code, raw
}

func doRawReq(t *testing.T, url, method, auth string, body any) ([]byte, int) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode
}
