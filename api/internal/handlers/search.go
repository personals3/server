package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

// SearchHandler powers GET /search — cross-bucket object search scoped to
// the calling user.
//
// Query params (all optional):
//
//	q           — substring of the object key (ILIKE '%q%')
//	bucket      — restrict to one bucket name
//	type        — content-type prefix (e.g. "image", "video/mp4")
//	ext         — file extension without dot (e.g. "jpg" matches .jpg keys)
//	minSize,maxSize — bytes
//	from,to     — ISO timestamps for updated_at range
//	transcodeStatus — "none|pending|processing|done|failed"
//	limit       — page size, default 50, max 500
//	offset      — pagination offset
//	sort        — "modified" (default) | "size" | "key"
//	dir         — "desc" (default) | "asc"
//
// Trash items (is_deleted=true) are always excluded. The pg_trgm index
// added in migration 014 makes substring queries on key fast.
type SearchHandler struct {
	DB *pgxpool.Pool
}

type searchHit struct {
	Bucket          string    `json:"bucket"`
	Key             string    `json:"key"`
	Size            int64     `json:"size"`
	ContentType     string    `json:"contentType"`
	ETag            string    `json:"etag"`
	LastModified    time.Time `json:"lastModified"`
	TranscodeStatus string    `json:"transcodeStatus"`
}

func (h *SearchHandler) Search(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	q := r.URL.Query()

	// ---- Build the WHERE clauses + args incrementally so we can stitch
	//      together however many filters the caller actually used.
	clauses := []string{
		`b.owner_id = $1`,
		`o.is_deleted = FALSE`,
	}
	args := []any{u.ID}

	add := func(clause string, val any) {
		args = append(args, val)
		// Replace the next $N placeholder.
		clause = strings.ReplaceAll(clause, "$$", "$"+strconv.Itoa(len(args)))
		clauses = append(clauses, clause)
	}

	if s := strings.TrimSpace(q.Get("q")); s != "" {
		add(`o.key ILIKE '%' || $$ || '%'`, s)
	}
	if s := strings.TrimSpace(q.Get("bucket")); s != "" {
		add(`b.name = $$`, s)
	}
	if s := strings.TrimSpace(q.Get("type")); s != "" {
		add(`o.content_type LIKE $$ || '%'`, s)
	}
	if s := strings.TrimSpace(q.Get("ext")); s != "" {
		// Match keys ending with .{ext} (case-insensitive).
		add(`o.key ILIKE '%' || $$`, "."+strings.TrimPrefix(strings.ToLower(s), "."))
	}
	if s := q.Get("minSize"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			add(`o.size_bytes >= $$`, n)
		}
	}
	if s := q.Get("maxSize"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			add(`o.size_bytes <= $$`, n)
		}
	}
	if s := q.Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			add(`o.updated_at >= $$`, t)
		}
	}
	if s := q.Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			add(`o.updated_at <= $$`, t)
		}
	}
	if s := strings.TrimSpace(q.Get("transcodeStatus")); s != "" {
		add(`o.transcode_status = $$`, s)
	}

	// ---- Sort + pagination
	limit := 50
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	offset := 0
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n >= 0 {
		offset = n
	}
	orderBy := "o.updated_at"
	switch q.Get("sort") {
	case "size":
		orderBy = "o.size_bytes"
	case "key":
		orderBy = "o.key"
	}
	dir := "DESC"
	if strings.ToLower(q.Get("dir")) == "asc" {
		dir = "ASC"
	}

	whereSQL := strings.Join(clauses, " AND ")

	// Total count for pagination (cheap — same WHERE, no LIMIT).
	var total int
	countSQL := `SELECT COUNT(*)
	               FROM objects o
	               JOIN buckets b ON b.id = o.bucket_id
	              WHERE ` + whereSQL
	if err := h.DB.QueryRow(r.Context(), countSQL, args...).Scan(&total); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	args = append(args, limit, offset)
	listSQL := `
		SELECT b.name, o.key, o.size_bytes, o.content_type, o.etag,
		       o.updated_at, o.transcode_status
		  FROM objects o
		  JOIN buckets b ON b.id = o.bucket_id
		 WHERE ` + whereSQL + `
		 ORDER BY ` + orderBy + ` ` + dir + `
		 LIMIT $` + strconv.Itoa(len(args)-1) +
		` OFFSET $` + strconv.Itoa(len(args))

	rows, err := h.DB.Query(r.Context(), listSQL, args...)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []searchHit{}
	for rows.Next() {
		var hit searchHit
		if err := rows.Scan(&hit.Bucket, &hit.Key, &hit.Size, &hit.ContentType,
			&hit.ETag, &hit.LastModified, &hit.TranscodeStatus); err != nil {
			continue
		}
		out = append(out, hit)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"count":   len(out),
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"results": out,
	})
}

// keep uuid import live so future filters (specific user/bucket UUID) compile.
var _ = uuid.UUID{}
