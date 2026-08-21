// Package api — REST-интерфейс шлюза: маршруты /api/v1, middleware
// (auth, логирование, recovery) и хендлеры.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"opencode-backend/internal/config"
	"opencode-backend/internal/engine"
	"opencode-backend/internal/opencode"
	"opencode-backend/internal/store"
	"opencode-backend/internal/token"
	"opencode-backend/internal/ws"
)

// Server связывает движок, WebSocket-хаб и хранилище в HTTP-интерфейс.
type Server struct {
	eng   *engine.Engine
	hub   *ws.Hub
	store store.Store
	cfg   *config.Config
	log   *slog.Logger
}

// New создаёт HTTP-сервер шлюза.
func New(eng *engine.Engine, hub *ws.Hub, st store.Store, cfg *config.Config, log *slog.Logger) *Server {
	return &Server{eng: eng, hub: hub, store: st, cfg: cfg, log: log}
}

// Handler собирает маршрутизатор с middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /api/v1/me", s.me)
	protected.HandleFunc("POST /api/v1/auth/tokens", s.createToken)
	protected.HandleFunc("GET /api/v1/sessions", s.listSessions)
	protected.HandleFunc("POST /api/v1/sessions", s.createSession)
	protected.HandleFunc("GET /api/v1/sessions/{id}", s.getSession)
	protected.HandleFunc("PATCH /api/v1/sessions/{id}", s.updateSession)
	protected.HandleFunc("DELETE /api/v1/sessions/{id}", s.deleteSession)
	protected.HandleFunc("GET /api/v1/sessions/{id}/empty", s.sessionEmpty)
	protected.HandleFunc("GET /api/v1/sessions/{id}/activity", s.sessionActivity)
	protected.HandleFunc("POST /api/v1/sessions/{id}/resume", s.resumeSession)
	protected.HandleFunc("POST /api/v1/sessions/{id}/fork", s.forkSession)
	protected.HandleFunc("POST /api/v1/sessions/{id}/messages", s.sendMessage)
	protected.HandleFunc("GET /api/v1/sessions/{id}/messages", s.listMessages)
	protected.HandleFunc("GET /api/v1/sessions/{id}/messages/{mid}", s.getMessage)
	protected.HandleFunc("POST /api/v1/sessions/{id}/abort", s.abort)
	protected.HandleFunc("POST /api/v1/sessions/{id}/permissions/{pid}", s.replyPermission)
	protected.HandleFunc("POST /api/v1/questions/{qid}", s.replyQuestion)
	protected.HandleFunc("POST /api/v1/files", s.uploadFile)
	protected.HandleFunc("GET /api/v1/agents", s.agents)
	protected.HandleFunc("GET /api/v1/providers", s.providers)
	protected.HandleFunc("GET /api/v1/ws", s.ws)

	mux.Handle("/api/v1/", s.recover(s.logging(s.auth(protected))))
	return mux
}

// ── Middleware ───────────────────────────────────────────────────────────────

type ctxKey int

const userIDKey ctxKey = iota

func userIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(userIDKey).(string)
	return id
}

// bearerToken извлекает токен из Authorization-заголовка или query-параметра
// (для WebSocket, где браузер не может задать заголовок).
func bearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimPrefix(v, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "требуется токен")
			return
		}
		key, err := s.store.UserByTokenHash(token.Hash(raw))
		if err != nil || key.Revoked {
			writeError(w, http.StatusUnauthorized, "недействительный токен")
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, key.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start).Round(time.Microsecond),
		)
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "error", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "внутренняя ошибка")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Hijack позволяет WebSocket-апгрейду (websocket.Accept) перехватить
// соединение сквозь logging-миддлвару.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hj.Hijack()
}

// ── Хендлеры ────────────────────────────────────────────────────────────────

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"healthy": true})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.eng.Health(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "opencode недоступен: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"healthy": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetUser(userIDFrom(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw := token.New()
	key := &store.APIKey{
		TokenHash: token.Hash(raw),
		UserID:    userIDFrom(r.Context()),
		Label:     body.Label,
		CreatedAt: time.Now(),
	}
	if err := s.store.EnsureAPIKey(key); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"token": raw})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.eng.ListSessions(userIDFrom(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    string `json:"title"`
		ParentID string `json:"parentID"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := s.eng.CreateSession(r.Context(), userIDFrom(r.Context()), body.Title)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.eng.GetSession(userIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := s.eng.RenameSession(r.Context(), userIDFrom(r.Context()), r.PathValue("id"), body.Title)
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) resumeSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.eng.ResumeSession(userIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) sessionActivity(w http.ResponseWriter, r *http.Request) {
	act, err := s.eng.SessionActivity(r.Context(), userIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, act)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.DeleteSession(r.Context(), userIDFrom(r.Context()), r.PathValue("id")); err != nil {
		writeEngineErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessionEmpty сообщает, пуста ли сессия (нет сообщений). Используется
// фронтендами, чтобы не создавать дублирующие пустые сессии.
func (s *Server) sessionEmpty(w http.ResponseWriter, r *http.Request) {
	empty, err := s.eng.SessionEmpty(r.Context(), userIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"empty": empty})
}

func (s *Server) forkSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MessageID string `json:"messageID"`
	}
	_ = decodeJSON(r, &body)
	sess, err := s.eng.ForkSession(r.Context(), userIDFrom(r.Context()), r.PathValue("id"), body.MessageID)
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var req opencode.MessageRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Parts) == 0 {
		writeError(w, http.StatusBadRequest, "parts обязателен")
		return
	}

	msgID, err := s.eng.SendMessage(r.Context(), userIDFrom(r.Context()), r.PathValue("id"), req)
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"messageID": msgID})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.eng.ListMessages(userIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	msg, err := s.eng.GetMessage(userIDFrom(r.Context()), r.PathValue("id"), r.PathValue("mid"))
	if err != nil {
		writeEngineErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.AbortSession(r.Context(), userIDFrom(r.Context()), r.PathValue("id")); err != nil {
		writeEngineErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) replyPermission(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Response string `json:"response"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch body.Response {
	case "once", "always", "reject":
	default:
		writeError(w, http.StatusBadRequest, "response: once|always|reject")
		return
	}
	if err := s.eng.ReplyPermission(r.Context(), userIDFrom(r.Context()),
		r.PathValue("id"), r.PathValue("pid"), body.Response); err != nil {
		writeEngineErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) replyQuestion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Answers [][]string `json:"answers"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.eng.ReplyQuestion(r.Context(), userIDFrom(r.Context()), r.PathValue("qid"), body.Answers); err != nil {
		writeEngineErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.eng.Agents(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	data, err := s.eng.Providers(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	userID := userIDFrom(r.Context())
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "ожидается multipart-форма с полем file")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "поле file обязательно")
		return
	}
	defer file.Close()

	name := sanitizeFilename(header.Filename)
	dir := filepath.Join(s.cfg.WorkspaceDir, ".opencode-backend", "uploads", userID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(dir, name)
	dst, err := os.Create(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"url": "file://" + path})
}

// sanitizeFilename приводит имя файла к безопасному виду.
func sanitizeFilename(name string) string {
	base := filepath.Base(name)
	var sb strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	out := strings.Trim(sb.String(), "._")
	if out == "" {
		out = "file"
	}
	return out
}

func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	s.hub.ServeWS(w, r, userIDFrom(r.Context()))
}

// ── Утилиты ─────────────────────────────────────────────────────────────────

func decodeJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeEngineErr(w http.ResponseWriter, err error) {
	switch {
	case err == engine.ErrSessionNotFound:
		writeError(w, http.StatusNotFound, err.Error())
	case err == engine.ErrBusy:
		writeError(w, http.StatusConflict, err.Error())
	case err == store.ErrNotFound:
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}
