package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

type ImportHandler struct {
	DB *pgxpool.Pool
}

type importEnqueueReq struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// POST /:bucket/*?import — enqueue an async URL import.
// Returns immediately with the new job ID. Use GET /imports to poll progress.
func (h *ObjectHandler) ImportFromURL(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)
	if key == "" {
		httpx.WriteError(w, http.StatusBadRequest, "INVALID_KEY", "object key required")
		return
	}

	var req importEnqueueReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	if err := validateImportURL(req.URL); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_URL", err.Error())
		return
	}

	bucketID, err := resolveBucket(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	authHeader := req.Headers["Authorization"]
	var authPtr *string
	if authHeader != "" {
		authPtr = &authHeader
	}

	var jobID uuid.UUID
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO import_jobs (user_id, bucket_id, key, source_url, auth_header)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		u.ID, bucketID, key, req.URL, authPtr,
	).Scan(&jobID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"jobId":  jobID,
		"bucket": bucketName,
		"key":    key,
		"status": "pending",
	})
}

// ---- Import job listing / inspection / cancellation -----------------------

type ImportJobDTO struct {
	ID            uuid.UUID  `json:"id"`
	Bucket        string     `json:"bucket"`
	Key           string     `json:"key"`
	SourceURL     string     `json:"sourceUrl"`
	Status        string     `json:"status"`
	BytesDone     int64      `json:"bytesDone"`
	TotalBytes    *int64     `json:"totalBytes,omitempty"`
	ThroughputBps int64      `json:"throughputBps"`
	Error         *string    `json:"error,omitempty"`
	ObjectID      *uuid.UUID `json:"objectId,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	DoneAt        *time.Time `json:"doneAt,omitempty"`
}

// GET /imports?active=1&limit=50
//
// Default returns the user's last 50 jobs. With ?active=1 returns only
// pending+running (used for the live progress panel).
func (h *ImportHandler) ListImports(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	activeOnly := r.URL.Query().Get("active") == "1"

	whereStatus := ""
	if activeOnly {
		whereStatus = "AND i.status IN ('pending','running')"
	}

	rows, err := h.DB.Query(r.Context(), fmt.Sprintf(`
		SELECT i.id, b.name, i.key, i.source_url, i.status,
		       i.bytes_done, i.total_bytes, i.throughput_bps,
		       i.error, i.object_id,
		       i.created_at, i.started_at, i.done_at
		  FROM import_jobs i
		  JOIN buckets b ON b.id = i.bucket_id
		 WHERE i.user_id = $1 %s
		 ORDER BY i.created_at DESC
		 LIMIT $2`, whereStatus),
		u.ID, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []ImportJobDTO{}
	for rows.Next() {
		var j ImportJobDTO
		if err := rows.Scan(&j.ID, &j.Bucket, &j.Key, &j.SourceURL, &j.Status,
			&j.BytesDone, &j.TotalBytes, &j.ThroughputBps,
			&j.Error, &j.ObjectID, &j.CreatedAt, &j.StartedAt, &j.DoneAt); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, j)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"imports": out})
}

// DELETE /imports/{id} — cancel a running job, or remove a finished one.
func (h *ImportHandler) CancelOrDelete(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid uuid")
		return
	}

	// First try cancelling if running — the importer's progress ticker will
	// notice and stop the download.
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE import_jobs SET status = 'cancelled', done_at = now()
		 WHERE id = $1 AND user_id = $2 AND status IN ('pending','running')`,
		id, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if tag.RowsAffected() == 0 {
		// Either job doesn't exist, isn't ours, or is already finished.
		// Try a hard delete (e.g., dismissing a done/failed row from the list).
		tag, err = h.DB.Exec(r.Context(),
			`DELETE FROM import_jobs WHERE id = $1 AND user_id = $2`, id, u.ID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
			return
		}
		if tag.RowsAffected() == 0 {
			httpx.WriteError(w, http.StatusNotFound, "NOT_FOUND", "import not found")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- SSRF + URL validation ------------------------------------------------

func validateImportURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http(s) URLs are allowed")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("missing host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("dns lookup failed: %w", err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("host %s resolves to a blocked address (%s)", host, ip)
		}
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
		return fmt.Errorf("blocked address: %s", ip)
	}
	low := strings.ToLower(host)
	for _, bad := range []string{"localhost", "ip6-localhost"} {
		if low == bad {
			return fmt.Errorf("blocked host: %s", host)
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, cidr := range blockedCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

var blockedCIDRs = mustParseCIDRs(
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"100.64.0.0/10",
	"fc00::/7",
	"fe80::/10",
)

func mustParseCIDRs(s ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(s))
	for _, c := range s {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}
