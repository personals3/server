package middleware

import (
	"context"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditCapture wraps ResponseWriter so we can read back the status code
// and bytes written after the handler returns.
type auditCapture struct {
	http.ResponseWriter
	status   int
	bytesOut int64
}

func (c *auditCapture) WriteHeader(s int) {
	c.status = s
	c.ResponseWriter.WriteHeader(s)
}

func (c *auditCapture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(b)
	c.bytesOut += int64(n)
	return n, err
}

// Flush forwards to the underlying writer if it supports flushing, so SSE
// handlers (e.g. /admin/quota-events) can do w.(http.Flusher) through the
// audit wrapper instead of getting a failed assertion → 500.
func (c *auditCapture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Audit returns middleware that records every authenticated request to
// the audit_log table. Writes happen in a goroutine so they don't add
// latency to the response path.
func Audit(db *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cap := &auditCapture{ResponseWriter: w}
			next.ServeHTTP(cap, r)

			u := UserFromContext(r.Context())
			var userID *uuid.UUID
			if u != nil {
				userID = &u.ID
			}

			pattern := ""
			if rc := chi.RouteContext(r.Context()); rc != nil {
				pattern = rc.RoutePattern()
			}
			action := actionFromRoute(r.Method, pattern)
			bucket, key := parseBucketKey(r.URL.Path)
			size := bestEffortSize(r, cap)

			go writeAuditAsync(db, auditRow{
				UserID:    userID,
				Action:    action,
				Bucket:    bucket,
				Key:       key,
				SizeBytes: size,
				Status:    cap.status,
				IPAddress: clientIP(r),
				UserAgent: r.UserAgent(),
			})
		})
	}
}

type auditRow struct {
	UserID    *uuid.UUID
	Action    string
	Bucket    string
	Key       string
	SizeBytes int64
	Status    int
	IPAddress string
	UserAgent string
}

func writeAuditAsync(db *pgxpool.Pool, row auditRow) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*1e9) // 5s
	defer cancel()

	var bucketPtr, keyPtr *string
	if row.Bucket != "" {
		bucketPtr = &row.Bucket
	}
	if row.Key != "" {
		keyPtr = &row.Key
	}
	var ipPtr *string
	if row.IPAddress != "" {
		ipPtr = &row.IPAddress
	}

	_, err := db.Exec(ctx, `
		INSERT INTO audit_log
		  (user_id, action, bucket_name, object_key, size_bytes,
		   status_code, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		row.UserID, row.Action, bucketPtr, keyPtr, row.SizeBytes,
		row.Status, ipPtr, row.UserAgent)
	if err != nil {
		log.Printf("audit log write failed: %v", err)
	}
}

// actionFromRoute returns an S3-style action name from chi's matched route
// pattern (e.g. "/{bucket}/*"), not the raw URL — that way "/auth/me" and
// "/{bucket}/key" don't get confused by slash-counting.
func actionFromRoute(method, pattern string) string {
	switch pattern {
	case "/":
		if method == http.MethodGet {
			return "LIST_BUCKETS"
		}
	case "/auth/login":
		return "LOGIN"
	case "/auth/me":
		return "GET_ME"
	case "/auth/keys":
		if method == http.MethodPost {
			return "CREATE_API_KEY"
		}
		return "LIST_API_KEYS"
	case "/auth/keys/{id}":
		return "REVOKE_API_KEY"
	case "/{bucket}":
		switch method {
		case http.MethodPut:
			return "CREATE_BUCKET"
		case http.MethodDelete:
			return "DELETE_BUCKET"
		case http.MethodHead:
			return "HEAD_BUCKET"
		case http.MethodGet:
			return "LIST_OBJECTS"
		}
	case "/{bucket}/*":
		switch method {
		case http.MethodPut:
			return "PUT_OBJECT"
		case http.MethodGet:
			return "GET_OBJECT"
		case http.MethodHead:
			return "HEAD_OBJECT"
		case http.MethodDelete:
			return "DELETE_OBJECT"
		}
	}
	return method
}

func parseBucketKey(path string) (bucket, key string) {
	// Strip leading slash, split on first "/"
	if len(path) == 0 || path == "/" {
		return "", ""
	}
	p := path
	if p[0] == '/' {
		p = p[1:]
	}
	for i, c := range p {
		if c == '/' {
			return p[:i], p[i+1:]
		}
	}
	return p, ""
}

func bestEffortSize(r *http.Request, c *auditCapture) int64 {
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		if r.ContentLength > 0 {
			return r.ContentLength
		}
	}
	if r.Method == http.MethodGet {
		if cl := c.Header().Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
				return n
			}
		}
		return c.bytesOut
	}
	return 0
}

// clientIP returns just the IP — no port, no proxy chain — because
// the audit_log.ip_address column is PostgreSQL INET, which rejects "ip:port".
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For: "client, proxy1, proxy2" — first is the original client
		if i := strings.Index(xff, ","); i > 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return strings.TrimSpace(ip)
	}
	// RemoteAddr is "host:port" or "[::1]:port" — peel off the port.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
