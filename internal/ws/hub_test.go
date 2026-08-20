package ws

import (
	"testing"
	"time"
)

func TestSubscribePublish(t *testing.T) {
	h := NewHub()
	allCh, allUnsub := h.Subscribe("u1", "*")
	sessCh, sessUnsub := h.Subscribe("u1", "sess1")
	otherCh, _ := h.Subscribe("u2", "*")
	defer allUnsub()
	defer sessUnsub()

	ev := Event{V: 1, Type: "test", Session: "sess1"}
	h.Publish("u1", "sess1", ev)

	assertRecv(t, allCh, "test")
	assertRecv(t, sessCh, "test")
	assertNoRecv(t, otherCh)

	// u1 не видит события чужой сессии пользователя u2.
	h.Publish("u2", "sess2", ev)
	assertNoRecv(t, allCh)

	// Подписчик «все сессии» получает и другую свою сессию.
	h.Publish("u1", "sess2", ev)
	assertRecv(t, allCh, "test")
}

func TestUnsubscribe(t *testing.T) {
	h := NewHub()
	ch, unsub := h.Subscribe("u1", "*")
	h.Publish("u1", "s", Event{Type: "a"})
	assertRecv(t, ch, "a")
	unsub()
	h.Publish("u1", "s", Event{Type: "b"})
	assertNoRecv(t, ch)
}

func TestPublishSlowSubscriberClosed(t *testing.T) {
	h := NewHub()
	c := &Conn{userID: "u1", sessionID: "*", send: make(chan Event, 1)}
	h.add(c)
	c.send <- Event{} // канал полон — подписчик не успевает читать

	h.Publish("u1", "s", Event{Type: "x"}) // не должно паниковать (conn == nil)
	if !c.closed.Load() {
		t.Fatal("медленный подписчик не помечен закрытым")
	}
}

// ── Хелперы ─────────────────────────────────────────────────────────────────

func assertRecv(t *testing.T, ch <-chan Event, wantType string) {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Type != wantType {
			t.Fatalf("получено событие %q, want %q", ev.Type, wantType)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("не получено событие %q", wantType)
	}
}

func assertNoRecv(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("неожиданное событие %q", ev.Type)
	case <-time.After(150 * time.Millisecond):
	}
}
