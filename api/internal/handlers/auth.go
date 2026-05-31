package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/auth"
	"github.com/personals3/api/internal/email"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/storage"
)

type AuthHandler struct {
	DB        *pgxpool.Pool
	FS        *storage.FS
	JWTSecret string
	TOTP      *TOTPHandler // set in main.go; if nil, 2FA is silently disabled
	// Mailer + admin email used by self-serve onboarding flows.
	// AdminEmail receives a notification each time a stranger requests
	// an account or an existing user asks for more quota.
	Mailer     *email.Mailer
	AdminEmail string
	// SupportEmail is the address put in every user-facing email's
	// "need help?" line. Usually equals SMTP_REPLY_TO (e.g.
	// support@yourdomain.com). Defaults to AdminEmail if blank.
	SupportEmail string
	// LoginURL + AdminURL go into the email templates so links resolve.
	LoginURL string
	AdminURL string
	// DefaultQuotaBytes is the quota new accounts get unless the admin
	// overrides at approval time. Default 100 MB.
	DefaultQuotaBytes int64
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResp struct {
	Token string    `json:"token"`
	User  publicUser `json:"user"`
}

type publicUser struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	QuotaBytes  int64     `json:"quotaBytes"`
	UsedBytes   int64     `json:"usedBytes"`
	TotpEnabled bool      `json:"totpEnabled"`
}

// POST /auth/login — exchange email+password for a JWT.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}

	var u publicUser
	var hash string
	var totpEnabled bool
	err := h.DB.QueryRow(r.Context(), `
		SELECT id, email, name, role, quota_bytes, used_bytes,
		       COALESCE(password_hash, ''), COALESCE(totp_enabled, false)
		  FROM users WHERE email = $1 AND is_active = true`,
		req.Email,
	).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.QuotaBytes, &u.UsedBytes, &hash, &totpEnabled)
	if errors.Is(err, pgx.ErrNoRows) || hash == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "BAD_CREDS", "invalid credentials")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if err := auth.CheckPassword(hash, req.Password); err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "BAD_CREDS", "invalid credentials")
		return
	}

	// Capacity soft-block. If the admin has LOWERED max_users below the
	// existing active-user count, the "extra" users (by created_at DESC)
	// can't sign in until the cap is raised. We intentionally do this
	// AFTER the password check so we never leak who's in the system —
	// wrong password gets the standard 401 regardless.
	if cap, capErr := CapacityHere(r.Context(), h.DB, h.FS.Root()); capErr == nil {
		if overflow, _ := cap.IsUserOverflow(r.Context(), h.DB, u.ID); overflow {
			support := h.SupportEmail
			if support == "" {
				support = h.AdminEmail
			}
			httpx.WriteErrorDetails(w, http.StatusServiceUnavailable,
				"CAPACITY_OVERFLOW",
				"This instance is temporarily at capacity. Please contact "+support+" if you need access.",
				map[string]any{
					"supportEmail": support,
				})
			return
		}
	}

	// 2FA gate. If enabled, hand back a short-lived challenge token instead
	// of a JWT. The dashboard then prompts for the code and POSTs to
	// /auth/2fa/login to exchange (challenge, code) for the JWT.
	//
	// Skip the challenge entirely if the browser presents a valid
	// X-Trusted-Device token tied to this user — that token was minted
	// after a successful 2FA pass in the past 30 days (or until the user
	// revokes it). Limit 3 per user; oldest is evicted on add.
	if totpEnabled && h.TOTP != nil {
		if dt := r.Header.Get("X-Trusted-Device"); dt != "" {
			if h.consumeTrustedDevice(r, u.ID, dt) {
				// Device is trusted — issue JWT as if 2FA passed.
				tok, err := auth.IssueJWT(h.JWTSecret, u.ID, u.Email, u.Role)
				if err != nil {
					httpx.WriteError(w, http.StatusInternalServerError, "JWT", err.Error())
					return
				}
				httpx.WriteJSON(w, http.StatusOK, loginResp{Token: tok, User: u})
				return
			}
		}
		tok, err := h.TOTP.CreateChallenge(r, u.ID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "TOTP", err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"require2fa": true,
			"challenge":  tok,
		})
		return
	}

	tok, err := auth.IssueJWT(h.JWTSecret, u.ID, u.Email, u.Role)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "JWT", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, loginResp{Token: tok, User: u})
}

// ActivityDTO is a user's own recent audit entry — same shape as admin audit
// but always scoped to themselves, no admin role required.
type ActivityDTO struct {
	ID         int64     `json:"id"`
	Action     string    `json:"action"`
	Bucket     *string   `json:"bucket,omitempty"`
	Key        *string   `json:"key,omitempty"`
	SizeBytes  *int64    `json:"sizeBytes,omitempty"`
	StatusCode *int      `json:"statusCode,omitempty"`
	Timestamp  time.Time `json:"ts"`
}

// GET /auth/activity?limit=20 — current user's recent audit entries.
// Lets the dashboard show "what did I just do" without exposing admin views.
func (h *AuthHandler) Activity(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, action, bucket_name, object_key, size_bytes, status_code, ts
		  FROM audit_log
		 WHERE user_id = $1
		   AND action NOT IN ('GET_ME', 'LIST_API_KEYS', 'LIST_S3_CREDENTIALS')
		 ORDER BY ts DESC
		 LIMIT $2`, u.ID, limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()
	out := []ActivityDTO{}
	for rows.Next() {
		var a ActivityDTO
		if err := rows.Scan(&a.ID, &a.Action, &a.Bucket, &a.Key,
			&a.SizeBytes, &a.StatusCode, &a.Timestamp); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, a)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// ActiveUploadDTO — server-side state of a browser multipart upload that's
// still open (`status='in-progress'`). After a tab refresh these may be
// abandoned; the user can abort to refund the quota they reserve.
type ActiveUploadDTO struct {
	UploadID   string    `json:"uploadId"`
	Bucket     string    `json:"bucket"`
	Key        string    `json:"key"`
	TotalBytes int64     `json:"totalBytes"`
	PartsDone  int       `json:"partsDone"`
	CreatedAt  time.Time `json:"createdAt"`
}

type ActiveImportDTO struct {
	ID            string  `json:"id"`
	Bucket        string  `json:"bucket"`
	Key           string  `json:"key"`
	Status        string  `json:"status"`
	BytesDone     int64   `json:"bytesDone"`
	TotalBytes    *int64  `json:"totalBytes,omitempty"`
	ThroughputBps int64   `json:"throughputBps"`
}

type ActiveTranscodeDTO struct {
	JobID       string `json:"jobId"`
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	FileType    string `json:"fileType"`
	Status      string `json:"status"`
	Attempts    int    `json:"attempts"`
	ProgressPct int    `json:"progressPct"`
}

// GET /auth/active — what's running RIGHT NOW for this user.
// Returns all three concurrent activity streams in one round-trip so the
// dashboard can render a single "Active operations" panel.
func (h *AuthHandler) Active(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	ctx := r.Context()

	uploads := []ActiveUploadDTO{}
	imports := []ActiveImportDTO{}
	transcodes := []ActiveTranscodeDTO{}

	// In-progress multipart sessions (browser uploads still pending complete)
	rows, err := h.DB.Query(ctx, `
		SELECT mu.upload_id, b.name, mu.key, mu.total_size, mu.created_at,
		       COALESCE((SELECT COUNT(*) FROM multipart_parts WHERE upload_id = mu.upload_id), 0)
		  FROM multipart_uploads mu
		  JOIN buckets b ON b.id = mu.bucket_id
		 WHERE mu.owner_id = $1 AND mu.status = 'in-progress'
		   AND mu.expires_at > now()
		 ORDER BY mu.created_at DESC`, u.ID)
	if err == nil {
		for rows.Next() {
			var x ActiveUploadDTO
			if err := rows.Scan(&x.UploadID, &x.Bucket, &x.Key,
				&x.TotalBytes, &x.CreatedAt, &x.PartsDone); err == nil {
				uploads = append(uploads, x)
			}
		}
		rows.Close()
	}

	// URL imports
	rows, err = h.DB.Query(ctx, `
		SELECT ij.id::text, b.name, ij.key, ij.status,
		       ij.bytes_done, ij.total_bytes, ij.throughput_bps
		  FROM import_jobs ij
		  JOIN buckets b ON b.id = ij.bucket_id
		 WHERE ij.user_id = $1 AND ij.status IN ('pending', 'running')
		 ORDER BY ij.created_at DESC`, u.ID)
	if err == nil {
		for rows.Next() {
			var x ActiveImportDTO
			if err := rows.Scan(&x.ID, &x.Bucket, &x.Key, &x.Status,
				&x.BytesDone, &x.TotalBytes, &x.ThroughputBps); err == nil {
				imports = append(imports, x)
			}
		}
		rows.Close()
	}

	// Transcode jobs (scoped to this user's buckets)
	// Pull EVERY job for any object that has an active job, so we can compute
	// the weighted overall correctly (done siblings count toward 100%).
	rows, err = h.DB.Query(ctx, `
		SELECT tj.id::text, b.name, o.key, o.id::text, tj.file_type,
		       tj.status, tj.attempts, tj.progress_pct
		  FROM transcode_jobs tj
		  JOIN objects o ON o.id = tj.object_id
		  JOIN buckets b ON b.id = o.bucket_id
		 WHERE b.owner_id = $1
		   AND tj.object_id IN (
		     SELECT DISTINCT object_id FROM transcode_jobs
		      WHERE status IN ('pending', 'processing', 'waiting')
		   )
		 ORDER BY o.id, tj.created_at`, u.ID)

	// Group jobs per object so we can compute weighted overall, and also
	// surface the still-active jobs in the existing per-job list.
	type perObject struct {
		Bucket string
		Key    string
		Jobs   []PipelineJobDTO
	}
	byObject := map[string]*perObject{}

	if err == nil {
		for rows.Next() {
			var jobID, bucket, key, objectID, ft, status string
			var attempts, pct int
			if err := rows.Scan(&jobID, &bucket, &key, &objectID, &ft,
				&status, &attempts, &pct); err != nil {
				continue
			}
			if status == "pending" || status == "processing" {
				transcodes = append(transcodes, ActiveTranscodeDTO{
					JobID: jobID, Bucket: bucket, Key: key, FileType: ft,
					Status: status, Attempts: attempts, ProgressPct: pct,
				})
			}
			po, ok := byObject[objectID]
			if !ok {
				po = &perObject{Bucket: bucket, Key: key}
				byObject[objectID] = po
			}
			po.Jobs = append(po.Jobs, PipelineJobDTO{
				FileType: ft, Status: status, ProgressPct: pct,
			})
		}
		rows.Close()
	}

	type objectProgressDTO struct {
		Bucket     string `json:"bucket"`
		Key        string `json:"key"`
		OverallPct int    `json:"overallPct"`
		JobCount   int    `json:"jobCount"`
	}
	objectProgress := []objectProgressDTO{}
	for _, po := range byObject {
		objectProgress = append(objectProgress, objectProgressDTO{
			Bucket:     po.Bucket,
			Key:        po.Key,
			OverallPct: computeOverallPct(po.Jobs),
			JobCount:   len(po.Jobs),
		})
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"uploads":        uploads,
		"imports":        imports,
		"transcodes":     transcodes,
		"objectProgress": objectProgress,
	})
}

// DELETE /auth/active/uploads/{uploadId} — abort an abandoned multipart upload.
// Refunds quota, deletes tmp dir, marks status='aborted'. Server-side cleanup
// that the browser can't do after a refresh.
func (h *AuthHandler) AbortUpload(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	uploadID := chi.URLParam(r, "uploadId")

	var bucketID uuid.UUID
	var totalSize int64
	err := h.DB.QueryRow(r.Context(), `
		SELECT bucket_id, total_size FROM multipart_uploads
		 WHERE upload_id = $1 AND owner_id = $2 AND status = 'in-progress'`,
		uploadID, u.ID,
	).Scan(&bucketID, &totalSize)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_UPLOAD", "upload not found or already finished")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if totalSize > 0 {
		_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -totalSize)
	}

	_, _ = h.DB.Exec(r.Context(),
		`UPDATE multipart_uploads SET status='aborted' WHERE upload_id = $1`, uploadID)

	// Best-effort tmp dir cleanup
	_ = h.FS.RemoveAbandonedUpload(bucketID.String(), uploadID)

	w.WriteHeader(http.StatusNoContent)
}

// GET /auth/me — return the current authenticated user.
//
// Fetches totp_enabled fresh from the DB so the dashboard knows whether to
// show "Set up 2FA" vs "Disable 2FA" — the middleware-cached User struct
// doesn't carry it.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	var totpEnabled bool
	var usedBytes int64
	// Read used_bytes fresh — the middleware-cached value can lag transcode
	// publishes by minutes, and the dashboard relies on this for its bar.
	_ = h.DB.QueryRow(r.Context(),
		`SELECT COALESCE(totp_enabled, false), used_bytes FROM users WHERE id = $1`, u.ID,
	).Scan(&totpEnabled, &usedBytes)
	httpx.WriteJSON(w, http.StatusOK, publicUser{
		ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role,
		QuotaBytes: u.QuotaBytes, UsedBytes: usedBytes,
		TotpEnabled: totpEnabled,
	})
}

// StorageBreakdown is the per-bucket / trash / versions / reserved-transcode
// slice of used_bytes. Powers the dashboard's stacked storage chart.
type StorageBreakdown struct {
	QuotaBytes int64                   `json:"quotaBytes"`
	UsedBytes  int64                   `json:"usedBytes"`
	Buckets    []StorageBreakdownEntry `json:"buckets"`
	TrashBytes int64                   `json:"trashBytes"`
	// versionsBytes = sum of non-current object_versions across all buckets.
	// (Snapshots taken on overwrite/delete in versioned buckets.)
	VersionsBytes int64 `json:"versionsBytes"`
	// reservedBytes = transcode pre-flight reservations that haven't settled
	// to actual transcoded_bytes yet. Lives inside the per-bucket numbers
	// too; surfaced separately so the chart can color it as "in-flight".
	ReservedBytes int64 `json:"reservedBytes"`
}

// StorageBreakdownEntry is one bucket's slice. Bytes are split so the
// dashboard can stack them in its chart.
type StorageBreakdownEntry struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	OriginalBytes   int64  `json:"originalBytes"`   // sum of size_bytes for live objects
	TranscodedBytes int64  `json:"transcodedBytes"` // sum of transcoded_bytes for live objects
	ReservedBytes   int64  `json:"reservedBytes"`   // sum of transcode_reserved_bytes
	TrashBytes      int64  `json:"trashBytes"`      // size+transcoded for is_deleted=true
	TotalBytes      int64  `json:"totalBytes"`      // all of the above
	Archived        bool   `json:"archived"`
}

// GET /auth/storage — per-bucket + per-category breakdown.
func (h *AuthHandler) Storage(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())

	rows, err := h.DB.Query(r.Context(), `
		SELECT b.id::text, b.name, b.archived,
		       COALESCE(SUM(CASE WHEN NOT o.is_deleted THEN o.size_bytes ELSE 0 END), 0)        AS orig,
		       COALESCE(SUM(CASE WHEN NOT o.is_deleted THEN COALESCE(o.transcoded_bytes,0) ELSE 0 END), 0) AS trb,
		       COALESCE(SUM(COALESCE(o.transcode_reserved_bytes, 0)), 0)                       AS rsv,
		       COALESCE(SUM(CASE WHEN o.is_deleted THEN o.size_bytes + COALESCE(o.transcoded_bytes,0) ELSE 0 END), 0) AS trsh
		  FROM buckets b
		  LEFT JOIN objects o ON o.bucket_id = b.id
		 WHERE b.owner_id = $1
		 GROUP BY b.id, b.name, b.archived
		 ORDER BY b.name`, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := StorageBreakdown{QuotaBytes: u.QuotaBytes}
	for rows.Next() {
		var e StorageBreakdownEntry
		if err := rows.Scan(&e.ID, &e.Name, &e.Archived,
			&e.OriginalBytes, &e.TranscodedBytes, &e.ReservedBytes, &e.TrashBytes); err != nil {
			continue
		}
		e.TotalBytes = e.OriginalBytes + e.TranscodedBytes + e.ReservedBytes + e.TrashBytes
		out.TrashBytes += e.TrashBytes
		out.ReservedBytes += e.ReservedBytes
		out.Buckets = append(out.Buckets, e)
	}

	// Versions live in object_versions, not objects — separate query.
	_ = h.DB.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(ov.size_bytes), 0)
		  FROM object_versions ov
		  JOIN objects o ON o.id = ov.object_id
		  JOIN buckets b ON b.id = o.bucket_id
		 WHERE b.owner_id = $1`, u.ID,
	).Scan(&out.VersionsBytes)

	// Used = authoritative value from users.used_bytes (kept in sync by
	// QuotaReserve). The breakdown sum can drift slightly during in-flight
	// transactions; we show the authoritative number for the bar but the
	// breakdown for the per-bucket details.
	_ = h.DB.QueryRow(r.Context(),
		`SELECT used_bytes FROM users WHERE id = $1`, u.ID,
	).Scan(&out.UsedBytes)

	httpx.WriteJSON(w, http.StatusOK, out)
}

type createKeyReq struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type createKeyResp struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Key        string     `json:"key"`        // plaintext, shown ONCE
	KeyPrefix  string     `json:"keyPrefix"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// POST /auth/keys — create a new API key for the current user.
func (h *AuthHandler) CreateKey(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())

	var req createKeyReq
	_ = json.NewDecoder(r.Body).Decode(&req) // name + expiresAt optional

	plaintext, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "GEN_KEY", err.Error())
		return
	}
	hash := auth.HashAPIKey(plaintext)

	var id uuid.UUID
	var createdAt time.Time
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO api_keys (user_id, key_prefix, key_hash, name, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		RETURNING id, created_at`,
		u.ID, prefix, hash, req.Name, req.ExpiresAt,
	).Scan(&id, &createdAt)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, createKeyResp{
		ID:        id,
		Name:      req.Name,
		Key:       plaintext,
		KeyPrefix: prefix,
		ExpiresAt: req.ExpiresAt,
		CreatedAt: createdAt,
	})
}

type apiKeyDTO struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"keyPrefix"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// GET /auth/keys — list current user's keys (no secrets).
func (h *AuthHandler) ListKeys(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, COALESCE(name, ''), key_prefix, last_used_at, expires_at, created_at
		  FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC`, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []apiKeyDTO{}
	for rows.Next() {
		var k apiKeyDTO
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.LastUsedAt, &k.ExpiresAt, &k.CreatedAt); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, k)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// DELETE /auth/keys/:id — revoke a key.
func (h *AuthHandler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid uuid")
		return
	}
	tag, err := h.DB.Exec(r.Context(),
		`DELETE FROM api_keys WHERE id = $1 AND user_id = $2`, id, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusNotFound, "NOT_FOUND", "key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
