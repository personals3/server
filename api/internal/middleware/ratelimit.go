package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/personals3/api/internal/httpx"
)

// RateLimit returns middleware that allows max `perMinute` requests per
// authenticated user per minute. Admins get 10x the limit.
//
// Algorithm: fixed-window per minute. Key expires after 60s automatically.
// Simple, accurate enough, and atomic via INCR.
func RateLimit(rdb *redis.Client, perMinute int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := UserFromContext(r.Context())
			if u == nil {
				// Unauthenticated request — shouldn't happen if Authenticator runs first.
				next.ServeHTTP(w, r)
				return
			}

			limit := perMinute
			if u.Role == "admin" {
				limit *= 10
			}

			minute := time.Now().Unix() / 60
			key := fmt.Sprintf("ratelimit:%s:%d", u.ID.String(), minute)

			ctx := r.Context()
			pipe := rdb.Pipeline()
			incr := pipe.Incr(ctx, key)
			pipe.Expire(ctx, key, 65*time.Second) // 5s grace
			if _, err := pipe.Exec(ctx); err != nil {
				// Fail open: if Valkey is down, let request through (don't break the world)
				next.ServeHTTP(w, r)
				return
			}

			count := incr.Val()
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(int64(limit)-count, 10))

			if count > int64(limit) {
				w.Header().Set("Retry-After", "60")
				httpx.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED",
					fmt.Sprintf("rate limit %d req/min exceeded", limit))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
