package token

import (
	"testing"
)

func TestHashDeterministic(t *testing.T) {
	a, b := Hash("secret"), Hash("secret")
	if a != b {
		t.Fatalf("Hash не детерминирован: %s != %s", a, b)
	}
	if Hash("secret") == Hash("other") {
		t.Fatal("разные токены дали одинаковый хэш")
	}
}

func TestNewUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		tok := New()
		if len(tok) < 40 {
			t.Fatalf("токен подозрительно короткий: %q", tok)
		}
		if seen[tok] {
			t.Fatal("дубликат токена")
		}
		seen[tok] = true
	}
}
