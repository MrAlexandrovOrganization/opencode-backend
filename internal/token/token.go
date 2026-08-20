// Package token — генерация и хэширование API-токенов. В хранилище хранится
// только SHA-256 хэш, сам токен показывается клиенту один раз.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// New генерирует новый непрозрачный токен (32 случайных байта, base64url).
func New() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Hash возвращает SHA-256 хэш токена для хранения.
func Hash(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}
