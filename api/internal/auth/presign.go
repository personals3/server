package auth

// Presigned URL signing.
//
// Format:  /share/{bucket}/{key}?sig={hex}&expires={unix_seconds}&method={GET|HEAD}
//
// The signature covers (method, bucket, key, expires) so:
//   - a GET signature can't be used for HEAD or DELETE,
//   - changing the key or bucket in the URL invalidates the sig,
//   - the signature is bound to a wall-clock expiry.
//
// HMAC-SHA256 with the server's JWT_SECRET as the key. We don't store
// anything in the DB — verification is pure CPU.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// SignPresignedURL returns the hex signature for a (method, bucket, key, expires) tuple.
func SignPresignedURL(secret, method, bucket, key string, expiresUnix int64) string {
	msg := canonicalShareMessage(method, bucket, key, expiresUnix)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyPresignedURL returns nil if the signature is valid and not expired.
// Constant-time compare to thwart timing oracles.
func VerifyPresignedURL(secret, method, bucket, key, sig string, expiresUnix int64) error {
	if expiresUnix < time.Now().Unix() {
		return errors.New("link expired")
	}
	expected := SignPresignedURL(secret, method, bucket, key, expiresUnix)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func canonicalShareMessage(method, bucket, key string, expiresUnix int64) string {
	// Pipe-separated to avoid ambiguity if a key contains "/".
	return fmt.Sprintf("v1|%s|%s|%s|%d", method, bucket, key, expiresUnix)
}

// SignPresignedMultipartPart binds a signature to one specific (uploadId,
// partNumber) — so a part-1 URL can't be replayed as part-2, and a leaked
// URL for upload A can't touch upload B.
func SignPresignedMultipartPart(secret, bucket, key, uploadID string,
	partNumber int, expiresUnix int64,
) string {
	msg := fmt.Sprintf("mpv1|PUT|%s|%s|%s|%d|%d",
		bucket, key, uploadID, partNumber, expiresUnix)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyPresignedMultipartPart matches SignPresignedMultipartPart.
func VerifyPresignedMultipartPart(secret, bucket, key, uploadID string,
	partNumber int, sig string, expiresUnix int64,
) error {
	if expiresUnix < time.Now().Unix() {
		return errors.New("link expired")
	}
	expected := SignPresignedMultipartPart(secret, bucket, key, uploadID, partNumber, expiresUnix)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errors.New("signature mismatch")
	}
	return nil
}

// SignPresignedMultipartFinalize signs the Complete-multipart POST.
// Bound to the uploadId so it can't be reused to complete a different
// upload (key/bucket binding is also enforced via the canonical msg).
func SignPresignedMultipartFinalize(secret, bucket, key, uploadID string,
	expiresUnix int64,
) string {
	msg := fmt.Sprintf("mpv1|FINALIZE|%s|%s|%s|%d",
		bucket, key, uploadID, expiresUnix)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyPresignedMultipartFinalize matches SignPresignedMultipartFinalize.
func VerifyPresignedMultipartFinalize(secret, bucket, key, uploadID, sig string,
	expiresUnix int64,
) error {
	if expiresUnix < time.Now().Unix() {
		return errors.New("link expired")
	}
	expected := SignPresignedMultipartFinalize(secret, bucket, key, uploadID, expiresUnix)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return errors.New("signature mismatch")
	}
	return nil
}

// ParseExpiresQS is a tiny helper that turns ?expires=... into an int64.
func ParseExpiresQS(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
