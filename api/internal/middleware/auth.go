// Package middleware contains the chi-compatible HTTP middleware:
// authentication, rate limiting, quota enforcement, audit logging.
package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/auth"
	"github.com/personals3/api/internal/httpx"
)

type ctxKey int

const userKey ctxKey = iota

// User is the authenticated principal attached to each request.
type User struct {
	ID         uuid.UUID
	Email      string
	Name       string
	Role       string // "admin" | "user"
	QuotaBytes int64
	UsedBytes  int64
}

// UserFromContext returns the authenticated user, or nil if unauthenticated.
func UserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(userKey).(*User)
	return u
}

// WithUser attaches a User to a context. Exported so handlers that
// authenticate via a non-Bearer credential (e.g. signed-URL multipart
// finalize) can synthesize the same context shape that downstream
// authenticated handlers rely on via MustUser.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// MustUser panics if no user is attached — call only inside an authenticated handler.
func MustUser(ctx context.Context) *User {
	u := UserFromContext(ctx)
	if u == nil {
		panic("MustUser called on unauthenticated context")
	}
	return u
}

// Authenticator returns middleware that requires one of:
//   - Authorization: Bearer <JWT>           — dashboard sessions
//   - Authorization: Bearer psk_xxx.yyy     — our native API keys
//   - Authorization: AWS4-HMAC-SHA256 ...   — AWS SDK / aws-cli (SigV4)
//
// Unauthenticated requests get 401.
func Authenticator(db *pgxpool.Pool, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				httpx.WriteError(w, http.StatusUnauthorized, "NO_AUTH",
					"Authorization header required")
				return
			}

			var user *User
			var err error

			if strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256 ") {
				user, err = resolveSigV4(r, db)
			} else {
				tok := extractBearer(r)
				if tok == "" {
					httpx.WriteError(w, http.StatusUnauthorized, "NO_AUTH",
						"Authorization: Bearer or AWS4-HMAC-SHA256 required")
					return
				}
				user, err = resolveToken(r.Context(), db, jwtSecret, tok)
			}

			if err != nil {
				httpx.WriteError(w, http.StatusUnauthorized, "BAD_AUTH", err.Error())
				return
			}

			ctx := context.WithValue(r.Context(), userKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func resolveSigV4(r *http.Request, db *pgxpool.Pool) (*User, error) {
	comps, err := auth.ParseAuthHeader(r.Header.Get("Authorization"))
	if err != nil {
		return nil, err
	}

	// Look up the secret for this access key
	var secret string
	var u User
	err = db.QueryRow(r.Context(), `
		SELECT c.secret_key, u.id, u.email, u.name, u.role, u.quota_bytes, u.used_bytes
		  FROM s3_credentials c
		  JOIN users u ON u.id = c.user_id
		 WHERE c.access_key_id = $1 AND u.is_active = true`,
		comps.AccessKeyID,
	).Scan(&secret, &u.ID, &u.Email, &u.Name, &u.Role, &u.QuotaBytes, &u.UsedBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("invalid access key id")
	}
	if err != nil {
		return nil, err
	}

	if err := auth.VerifySigV4(r, comps, secret); err != nil {
		return nil, err
	}

	// Async last_used_at update
	akid := comps.AccessKeyID
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = db.Exec(bgCtx,
			`UPDATE s3_credentials SET last_used_at = now() WHERE access_key_id = $1`, akid)
	}()

	return &u, nil
}

// AdminOnly rejects non-admin users.
func AdminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := UserFromContext(r.Context())
		if u == nil || u.Role != "admin" {
			httpx.WriteError(w, http.StatusForbidden, "ADMIN_REQUIRED",
				"this endpoint requires admin role")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func resolveToken(ctx context.Context, db *pgxpool.Pool, jwtSecret, tok string) (*User, error) {
	// API key first (more common for storage clients)
	if strings.HasPrefix(tok, auth.APIKeyPrefix) {
		return resolveAPIKey(ctx, db, tok)
	}
	// Otherwise assume JWT
	return resolveJWT(ctx, db, jwtSecret, tok)
}

func resolveAPIKey(ctx context.Context, db *pgxpool.Pool, key string) (*User, error) {
	prefix, fullKey, err := auth.ParseAPIKey(key)
	if err != nil {
		return nil, err
	}
	hash := auth.HashAPIKey(fullKey)

	var u User
	var keyID uuid.UUID
	err = db.QueryRow(ctx, `
		SELECT k.id, u.id, u.email, u.name, u.role, u.quota_bytes, u.used_bytes
		  FROM api_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE k.key_prefix = $1 AND k.key_hash = $2
		   AND u.is_active = true
		   AND (k.expires_at IS NULL OR k.expires_at > now())`,
		prefix, hash,
	).Scan(&keyID, &u.ID, &u.Email, &u.Name, &u.Role, &u.QuotaBytes, &u.UsedBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("invalid API key")
	}
	if err != nil {
		return nil, err
	}

	// Async last_used_at update (don't block the request)
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = db.Exec(bgCtx,
			`UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
	}()

	return &u, nil
}

func resolveJWT(ctx context.Context, db *pgxpool.Pool, secret, raw string) (*User, error) {
	claims, err := auth.VerifyJWT(secret, raw)
	if err != nil {
		return nil, err
	}

	var u User
	err = db.QueryRow(ctx, `
		SELECT id, email, name, role, quota_bytes, used_bytes
		  FROM users WHERE id = $1 AND is_active = true`,
		claims.UserID,
	).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.QuotaBytes, &u.UsedBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("user not found or inactive")
	}
	return &u, err
}
