package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// readRandom and base64URL are shared between apikey.go and sigv4.go.

func readRandom(b []byte) (int, error) {
	return io.ReadFull(rand.Reader, b)
}

func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// API key format:  psk_<8-char-prefix>.<43-char-secret>
//
// - "psk_" identifies the token as a PersonalS3 key.
// - The 8-char prefix is stored in plaintext for display ("psk_abc12345...")
//   and to give a fast index for the lookup.
// - The 43-char secret is high-entropy (32 random bytes, base64url-encoded).
// - We store SHA-256(full_key) in the DB; we don't use bcrypt because the
//   secret has 256 bits of entropy — slow hashing buys nothing.
const (
	APIKeyPrefix    = "psk_"
	keyPrefixLength = 8
	secretBytes     = 32
)

// GenerateAPIKey returns (full_plaintext_key, prefix_for_display).
// The plaintext is returned ONCE — only the hash is stored.
func GenerateAPIKey() (key string, prefix string, err error) {
	prefixBytes := make([]byte, 4) // 4 bytes -> 8 hex chars
	if _, err = rand.Read(prefixBytes); err != nil {
		return "", "", err
	}
	prefix = hex.EncodeToString(prefixBytes)

	secret := make([]byte, secretBytes)
	if _, err = rand.Read(secret); err != nil {
		return "", "", err
	}
	secretEncoded := base64.RawURLEncoding.EncodeToString(secret)

	key = APIKeyPrefix + prefix + "." + secretEncoded
	return key, prefix, nil
}

// HashAPIKey returns the SHA-256 hex digest of the full plaintext key.
// Constant-time comparison is unnecessary because the hashes are
// indistinguishable to an attacker without knowing the original key.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ParseAPIKey extracts (prefix, full_key) from a Bearer token. Returns an
// error if the token doesn't look like a PersonalS3 API key.
func ParseAPIKey(token string) (prefix, fullKey string, err error) {
	if !strings.HasPrefix(token, APIKeyPrefix) {
		return "", "", errors.New("not an API key")
	}
	parts := strings.SplitN(token[len(APIKeyPrefix):], ".", 2)
	if len(parts) != 2 || len(parts[0]) != keyPrefixLength {
		return "", "", errors.New("malformed API key")
	}
	return parts[0], token, nil
}
