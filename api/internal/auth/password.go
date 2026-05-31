// Package auth handles credentials: passwords (bcrypt), API keys (SHA-256),
// and JWT session tokens.
package auth

import "golang.org/x/crypto/bcrypt"

// HashPassword returns the bcrypt hash of pw. Cost 10 is a good default
// (~75ms per hash on a modern CPU).
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), 10)
	return string(h), err
}

// CheckPassword returns nil if pw matches hash, else an error.
func CheckPassword(hash, pw string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw))
}
