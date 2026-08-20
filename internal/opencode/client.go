// Package opencode — тонкий HTTP-клиент к opencode-серверу
// (https://opencode.ai/docs/server). Единственная точка, через которую шлюз
// взаимодействует с сервером: сессии, сообщения, permissions, вопросы и SSE.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxEventBytes ограничивает размер одного SSE-события.
const maxEventBytes = 8 << 20

// ModelRef ссылается на модель по провайдеру и ID модели.
type ModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// PartInput — часть контента, отправляемая на сервер.
// Это либо текст, либо файл (см. AddText/AddFile).
type PartInput struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Filename string `json:"filename,omitempty"`
	URL      string `json:"url,omitempty"`
}

// MessageRequest — тело POST /session/{id}/message.
type MessageRequest struct {
	Model *ModelRef   `json:"model,omitempty"`
	Agent string      `json:"agent,omitempty"`
	Parts []PartInput `json:"parts"`
}

// AddText добавляет текстовую часть.
func (m *MessageRequest) AddText(text string) {
	m.Parts = append(m.Parts, PartInput{Type: "text", Text: text})
}

// AddFile добавляет файловую часть. url — file:// URL.
func (m *MessageRequest) AddFile(mime, filename, url string) {
	m.Parts = append(m.Parts, PartInput{Type: "file", Mime: mime, Filename: filename, URL: url})
}

// Session зеркалит тип Session opencode-сервера.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	ProjectID string `json:"projectID"`
	Time      struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

// MessagePart — часть ответа ассистента. Текстовые части несут контент.
type MessagePart struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Part — универсальная часть сообщения (для истории сообщений).
type Part struct {
	ID        string          `json:"id,omitempty"`
	SessionID string          `json:"sessionID,omitempty"`
	MessageID string          `json:"messageID,omitempty"`
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	CallID    string          `json:"callID,omitempty"`
	State     json.RawMessage `json:"state,omitempty"`
	URL       string          `json:"url,omitempty"`
	Mime      string          `json:"mime,omitempty"`
	Filename  string          `json:"filename,omitempty"`
}

// Message — зеркало типа Message из SDK opencode (событие message.updated).
type Message struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	Error     *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
	Time struct {
		Created   int64  `json:"created"`
		Completed *int64 `json:"completed,omitempty"`
	} `json:"time"`
	Cost   float64 `json:"cost"`
	Tokens struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
	} `json:"tokens"`
	Finish     string `json:"finish"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
}

// MessageError возвращает пользовательское сообщение об ошибке, если ответ не удался.
func (m *Message) MessageError() string {
	if m == nil || m.Error == nil {
		return ""
	}
	if m.Error.Name == "MessageAbortedError" {
		return "Отменено."
	}
	if msg := m.Error.Data.Message; msg != "" {
		return msg
	}
	return m.Error.Name
}

// MessageResponse — тело ответа POST /session/{id}/message.
type MessageResponse struct {
	Info  AssistantInfo `json:"info"`
	Parts []MessagePart `json:"parts"`
}

// AssistantInfo несёт метаданные сообщения, включая ошибки.
type AssistantInfo struct {
	ID     string  `json:"id"`
	Finish string  `json:"finish"`
	Cost   float64 `json:"cost"`
	Time   struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed,omitempty"`
	} `json:"time"`
	Tokens struct {
		Input  int `json:"input"`
		Output int `json:"output"`
	} `json:"tokens"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// MessageError возвращает пользовательское сообщение об ошибке.
func (r *MessageResponse) MessageError() string {
	if r.Info.Error == nil {
		return ""
	}
	if r.Info.Error.Name == "MessageAbortedError" {
		return "Отменено."
	}
	if msg := r.Info.Error.Data.Message; msg != "" {
		return msg
	}
	return r.Info.Error.Name
}

// Text конкатенирует все текстовые части ответа.
func (r *MessageResponse) Text() string {
	var sb strings.Builder
	for _, p := range r.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// SessionStatus — статус сессии (из GET /session/status).
type SessionStatus struct {
	Type    string `json:"type"` // idle | busy | retry
	Attempt int    `json:"attempt,omitempty"`
	Message string `json:"message,omitempty"`
	Next    int    `json:"next,omitempty"`
}

// Agent — минимальное зеркало типа Agent из SDK opencode.
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Mode        string `json:"mode"`
	BuiltIn     bool   `json:"builtIn"`
}

// MessageRecord — сообщение из истории сессии (GET /session/{id}/message).
type MessageRecord struct {
	Info  Message `json:"info"`
	Parts []Part  `json:"parts"`
}

// PermissionAsked — событие permission (permission.updated / permission.asked).
type PermissionAsked struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Permission string         `json:"permission"`
	Pattern    string         `json:"pattern"`
	Patterns   []string       `json:"patterns"`
	Title      string         `json:"title"`
	Metadata   map[string]any `json:"metadata"`
	Tool       struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool"`
}

// QuestionOption — опция вопроса ассистента.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Question — вопрос с вариантами ответа.
type Question struct {
	Question string           `json:"question"`
	Header   string           `json:"header"`
	Options  []QuestionOption `json:"options"`
	Custom   *bool            `json:"custom"`   // false = без «своего ответа»
	Multiple *bool            `json:"multiple"` // true = можно выбрать несколько
}

// QuestionAsked — событие question.asked.
type QuestionAsked struct {
	ID        string     `json:"id"`
	SessionID string     `json:"sessionID"`
	Questions []Question `json:"questions"`
}

// Event — событие из SSE-шины (/event). Properties оставлен сырым,
// чтобы вызывающий мог распарсить под конкретный тип.
type Event struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// Client — тонкий HTTP-клиент к opencode-серверу.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// New создаёт клиент для заданного URL сервера.
func New(baseURL, username, password string) *Client {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 30 * time.Second,
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     &http.Client{Transport: transport},
	}
}

func (c *Client) setAuth(req *http.Request) {
	if c.password == "" {
		return
	}
	token := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
	req.Header.Set("Authorization", "Basic "+token)
}

// do выполняет JSON-запрос и декодирует ответ в out (если задан).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode: %s %s: status %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Health проверяет доступность сервера.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/global/health", nil, nil)
}

// CreateSession создаёт сессию на сервере.
func (c *Client) CreateSession(ctx context.Context, title string) (*Session, error) {
	var s Session
	err := c.do(ctx, http.MethodPost, "/session", struct {
		Title string `json:"title"`
	}{title}, &s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSession возвращает сессию по ID.
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	if err := c.do(ctx, http.MethodGet, "/session/"+id, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListSessions возвращает все сессии сервера.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	var out []Session
	if err := c.do(ctx, http.MethodGet, "/session", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateSession переименовывает сессию.
func (c *Client) UpdateSession(ctx context.Context, id, title string) (*Session, error) {
	var s Session
	err := c.do(ctx, http.MethodPatch, "/session/"+id, struct {
		Title string `json:"title"`
	}{title}, &s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteSession удаляет сессию и все её данные.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/session/"+id, nil, nil)
}

// ForkSession создаёт форк сессии.
func (c *Client) ForkSession(ctx context.Context, id, messageID string) (*Session, error) {
	var s Session
	body := struct {
		MessageID string `json:"messageID,omitempty"`
	}{messageID}
	if err := c.do(ctx, http.MethodPost, "/session/"+id+"/fork", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// AbortSession прерывает выполняющийся запрос.
func (c *Client) AbortSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/session/"+id+"/abort", nil, nil)
}

// SendMessage отправляет промпт и блокируется до готовности ответа.
// Вызывающий обязан задать щедрый таймаут через ctx.
func (c *Client) SendMessage(ctx context.Context, id string, req MessageRequest) (*MessageResponse, error) {
	var resp MessageResponse
	if err := c.do(ctx, http.MethodPost, "/session/"+id+"/message", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListMessages возвращает историю сообщений сессии.
func (c *Client) ListMessages(ctx context.Context, id string, limit int) ([]MessageRecord, error) {
	path := "/session/" + id + "/message"
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}
	var out []MessageRecord
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetMessage возвращает сообщение по ID.
func (c *Client) GetMessage(ctx context.Context, sessionID, messageID string) (*MessageRecord, error) {
	var out MessageRecord
	if err := c.do(ctx, http.MethodGet, "/session/"+sessionID+"/message/"+messageID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SessionStatuses возвращает статусы всех сессий.
func (c *Client) SessionStatuses(ctx context.Context) (map[string]SessionStatus, error) {
	var out map[string]SessionStatus
	if err := c.do(ctx, http.MethodGet, "/session/status", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Agents возвращает список доступных агентов.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var out []Agent
	if err := c.do(ctx, http.MethodGet, "/agent", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Providers возвращает провайдеров и модели по умолчанию (сырой JSON).
func (c *Client) Providers(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/config/providers", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReplyPermission отвечает на запрос разрешения.
// response — одно из "once", "always" или "reject".
func (c *Client) ReplyPermission(ctx context.Context, sessionID, permissionID, response string) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/session/%s/permissions/%s", sessionID, permissionID),
		struct {
			Response string `json:"response"`
		}{response}, nil)
}

// ReplyQuestion отвечает на вопрос ассистента.
// answers — по одному элементу на вопрос (в порядке следования), каждый —
// массив выбранных меток.
func (c *Client) ReplyQuestion(ctx context.Context, requestID string, answers [][]string) error {
	return c.do(ctx, http.MethodPost,
		"/question/"+requestID+"/reply",
		struct {
			Answers [][]string `json:"answers"`
		}{answers}, nil)
}

// Events подписывается на SSE-шину и вызывает fn для каждого события.
// Блокируется до отмены ctx или обрыва потока.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode: events: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxEventBytes)
	var data strings.Builder
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				var ev Event
				if err := json.Unmarshal([]byte(data.String()), &ev); err == nil {
					fn(ev)
				}
				data.Reset()
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}
