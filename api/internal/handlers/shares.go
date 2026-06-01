package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/auth"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

// SharesHandler is /shares — list / extend / revoke presigned URLs owned
// by the calling user.
type SharesHandler struct {
	DB        *pgxpool.Pool
	JWTSecret string // used to RECONSTRUCT signed URLs on list
}

type shareDTO struct {
	ID            uuid.UUID  `json:"id"`
	Bucket        string     `json:"bucket"`
	Key           string     `json:"key"`
	Method        string     `json:"method"`
	ExpiresAt     time.Time  `json:"expiresAt"`
	ForceDownload bool       `json:"forceDownload"`
	Revoked       bool       `json:"revoked"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastUsedAt    *time.Time `json:"lastUsedAt,omitempty"`
	UseCount      int        `json:"useCount"`
	Expired       bool       `json:"expired"` // computed: ExpiresAt < now
	Active        bool       `json:"active"`  // !Revoked && !Expired
	// URL is the signed share URL, recomputed from the stored fields on
	// every read. The sig is HMAC of (secret, method, bucket, key, expires)
	// so given those fields it's deterministic — letting the dashboard
	// surface "copy this link again" without storing the URL client-side
	// after the create modal closes. Empty when the row predates the
	// reconstruction path (we just leave it blank in that case).
	URL string `json:"url,omitempty"`
}

// GET /shares — list this user's share links.
// Query:
//   ?active=1     — only show active (default = all)
//   ?bucket=X     — filter to one bucket
//   ?key=X        — filter to one object key (typically combined with ?bucket=)
//
// The (bucket, key) filter is what the bucket-detail file preview uses to
// pull just the shares for the file you've selected, so we can show + revoke
// them inline without a round-trip to the dedicated /shares page.
func (h *SharesHandler) List(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	q := r.URL.Query()

	clauses := []string{"owner_id = $1"}
	args := []any{u.ID}
	addArg := func(clauseFmt string, val any) {
		args = append(args, val)
		clauses = append(clauses, fmt.Sprintf(clauseFmt, len(args)))
	}
	if q.Get("active") == "1" {
		clauses = append(clauses, "NOT revoked AND expires_at > now()")
	}
	if b := q.Get("bucket"); b != "" {
		addArg("bucket_name = $%d", b)
	}
	if k := q.Get("key"); k != "" {
		addArg("object_key = $%d", k)
	}
	where := " WHERE " + clauses[0]
	for _, c := range clauses[1:] {
		where += " AND " + c
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, bucket_name, object_key, method, expires_at, force_download,
		       revoked, revoked_at, created_at, last_used_at, use_count,
		       COALESCE(url_token, '')
		  FROM share_links`+where+`
		 ORDER BY created_at DESC
		 LIMIT 500`, args...)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []shareDTO{}
	now := time.Now()
	for rows.Next() {
		var s shareDTO
		var revokedAt, lastUsedAt *time.Time
		var urlToken string
		if err := rows.Scan(&s.ID, &s.Bucket, &s.Key, &s.Method, &s.ExpiresAt,
			&s.ForceDownload, &s.Revoked, &revokedAt, &s.CreatedAt,
			&lastUsedAt, &s.UseCount, &urlToken); err != nil {
			continue
		}
		s.RevokedAt = revokedAt
		s.LastUsedAt = lastUsedAt
		s.Expired = s.ExpiresAt.Before(now)
		// Surface the short URL when we have a token (new shares), falling
		// back to the legacy signed URL for old rows. Only emit when
		// usable — revoked/expired links shouldn't tempt anyone to share.
		if !s.Revoked && !s.Expired {
			if urlToken != "" {
				s.URL = "/s/" + urlToken
			} else if h.JWTSecret != "" {
				s.URL = buildShareURL(h.JWTSecret, s.Bucket, s.Key, s.Method,
					s.ExpiresAt.Unix(), s.ForceDownload)
			}
		}
		s.Active = !s.Revoked && !s.Expired
		out = append(out, s)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"count":  len(out),
		"shares": out,
	})
}

// PATCH /shares/{id} — extend or shorten expiry.
// Body: {"expiresAt": unix-seconds}  OR  {"expiresInSec": N from now}
func (h *SharesHandler) Patch(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "bad share id")
		return
	}
	var req struct {
		ExpiresAt    *int64 `json:"expiresAt"`
		ExpiresInSec *int64 `json:"expiresInSec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	var newExp time.Time
	if req.ExpiresAt != nil {
		newExp = time.Unix(*req.ExpiresAt, 0)
	} else if req.ExpiresInSec != nil {
		newExp = time.Now().Add(time.Duration(*req.ExpiresInSec) * time.Second)
	} else {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST",
			"either expiresAt (unix sec) or expiresInSec is required")
		return
	}
	// Cap at 30d from now to match CreatePresignedURL's cap.
	maxExp := time.Now().Add(30 * 24 * time.Hour)
	if newExp.After(maxExp) {
		newExp = maxExp
	}

	tag, err := h.DB.Exec(r.Context(), `
		UPDATE share_links SET expires_at = $1
		 WHERE id = $2 AND owner_id = $3`,
		newExp, id, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusNotFound, "NO_SHARE", "share not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id":        id,
		"expiresAt": newExp.Unix(),
	})
}

// DELETE /shares/{id} — revoke one share.
func (h *SharesHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "bad share id")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE share_links
		   SET revoked = true, revoked_at = now()
		 WHERE id = $1 AND owner_id = $2 AND NOT revoked`,
		id, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		// Could be already-revoked OR not-yours OR not-found — keep error generic.
		httpx.WriteError(w, http.StatusNotFound, "NO_ACTIVE_SHARE",
			"share not found or already revoked")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /shares?all=1 — revoke every active share owned by this user.
func (h *SharesHandler) RevokeAll(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	if r.URL.Query().Get("all") != "1" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST",
			"this endpoint requires ?all=1 to confirm — or use DELETE /shares/{id}")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE share_links
		   SET revoked = true, revoked_at = now()
		 WHERE owner_id = $1 AND NOT revoked`, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"revoked": tag.RowsAffected(),
	})
}

// keep pgx import live in case ErrNoRows checks land here later
var _ = pgx.ErrNoRows
var _ = errors.Is

// buildShareURL recreates the same /share/<bucket>/<key>?sig=...&expires=...
// URL the create endpoint emits, given only the stored fields. The HMAC
// signature is deterministic in (secret, method, bucket, key, expires), so
// every list request produces the same URL — no per-request randomness, no
// drift if someone copies an older list response.
func buildShareURL(secret, bucket, key, method string, expiresUnix int64, download bool) string {
	sig := auth.SignPresignedURL(secret, method, bucket, key, expiresUnix)
	urlPath := "/share/" + url.PathEscape(bucket)
	for _, seg := range strings.Split(key, "/") {
		urlPath += "/" + url.PathEscape(seg)
	}
	q := make(url.Values)
	q.Set("sig", sig)
	q.Set("expires", strconv.FormatInt(expiresUnix, 10))
	if method != "GET" {
		q.Set("method", method)
	}
	if download {
		q.Set("download", "1")
	}
	return urlPath + "?" + q.Encode()
}
