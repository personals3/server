package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/storage"
)

type BucketHandler struct {
	DB *pgxpool.Pool
	FS *storage.FS
}

type BucketDTO struct {
	ID                uuid.UUID `json:"id"`
	Name              string    `json:"name"`
	AutoTranscodeMode string    `json:"autoTranscodeMode"` // none / media / photos_only
	IsPublic          bool      `json:"isPublic"`          // anonymous read at /public/{name}/...
	Versioning        bool      `json:"versioning"`        // keep prior versions on overwrite/delete
	Archived          bool      `json:"archived"`          // cleaner skips this bucket's shards
	CreatedAt         time.Time `json:"createdAt"`
}

var bucketNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)

// reservedBucketNames can't be used as bucket names because they collide
// with API routes (admin endpoints, auth endpoints, health checks, the
// nginx /stream/ static prefix, etc.).
var reservedBucketNames = map[string]bool{
	"admin":   true,
	"auth":    true,
	"healthz": true,
	"stream":  true,
	"imports": true,
	"static":  true,
	"public":  true,
	"trash":   true,
	"share":   true,
	"shares":  true,
	"search":  true,
}

// GET /
func (h *BucketHandler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())

	rows, err := h.DB.Query(r.Context(),
		`SELECT id, name, auto_transcode_mode, is_public, versioning, archived, created_at
		   FROM buckets WHERE owner_id = $1 ORDER BY name`, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []BucketDTO{}
	for rows.Next() {
		var b BucketDTO
		if err := rows.Scan(&b.ID, &b.Name, &b.AutoTranscodeMode, &b.IsPublic, &b.Versioning, &b.Archived, &b.CreatedAt); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, b)
	}

	if isS3Client(r) {
		res := listAllMyBucketsResult{
			XMLNS: s3XMLNS,
			Owner: s3Owner{ID: u.ID.String(), DisplayName: u.Email},
		}
		for _, b := range out {
			res.Buckets.Bucket = append(res.Buckets.Bucket, s3Bucket{
				Name: b.Name, CreationDate: b.CreatedAt,
			})
		}
		writeXML(w, http.StatusOK, res)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"buckets": out})
}

// PUT /:bucket
func (h *BucketHandler) CreateBucket(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	name := chi.URLParam(r, "bucket")
	if !bucketNameRE.MatchString(name) || len(name) < 3 || len(name) > 63 {
		httpx.WriteError(w, http.StatusBadRequest, "INVALID_NAME",
			"bucket name must be 3-63 lowercase letters/digits/hyphens, no leading/trailing hyphen")
		return
	}
	if reservedBucketNames[name] {
		httpx.WriteError(w, http.StatusBadRequest, "RESERVED_NAME",
			"this bucket name is reserved by the system")
		return
	}

	// Optional body for non-S3 clients: { "autoTranscodeMode": "media" }.
	// S3 clients send no body so this is fine to skip.
	mode := "none"
	if r.Body != nil && r.ContentLength != 0 {
		var body struct {
			AutoTranscodeMode string `json:"autoTranscodeMode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.AutoTranscodeMode == "media" ||
			body.AutoTranscodeMode == "photos_only" ||
			body.AutoTranscodeMode == "none" {
			mode = body.AutoTranscodeMode
		}
	}

	var bucketID uuid.UUID
	err := h.DB.QueryRow(r.Context(),
		`INSERT INTO buckets (name, owner_id, auto_transcode_mode)
		 VALUES ($1, $2, $3) RETURNING id`,
		name, u.ID, mode,
	).Scan(&bucketID)
	if err != nil {
		if isUniqueViolation(err) {
			// AWS S3 returns BucketAlreadyOwnedByYou as a no-op success in
			// most regions. rclone (and other clients) rely on this when
			// they call CreateBucket on every operation.
			err = h.DB.QueryRow(r.Context(),
				`SELECT id FROM buckets WHERE owner_id = $1 AND name = $2`,
				u.ID, name).Scan(&bucketID)
			if err == nil {
				w.Header().Set("Location", "/"+name)
				if isS3Client(r) {
					w.WriteHeader(http.StatusOK)
					return
				}
				var existingMode string
				_ = h.DB.QueryRow(r.Context(),
					`SELECT auto_transcode_mode FROM buckets WHERE id = $1`, bucketID,
				).Scan(&existingMode)
				httpx.WriteJSON(w, http.StatusOK, BucketDTO{
					ID: bucketID, Name: name, AutoTranscodeMode: existingMode, CreatedAt: time.Now(),
				})
				return
			}
		}
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if err := h.FS.CreateBucketDir(bucketID.String()); err != nil {
		_, _ = h.DB.Exec(r.Context(), `DELETE FROM buckets WHERE id = $1`, bucketID)
		httpx.WriteError(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}

	w.Header().Set("Location", "/"+name)
	// S3 clients expect an empty body with status 200; our dashboard wants JSON.
	if isS3Client(r) {
		w.WriteHeader(http.StatusOK)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, BucketDTO{ID: bucketID, Name: name, AutoTranscodeMode: mode, CreatedAt: time.Now()})
}

// PATCH /:bucket — update bucket settings (currently just autoTranscodeMode).
func (h *BucketHandler) UpdateBucket(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	name := chi.URLParam(r, "bucket")

	var req struct {
		AutoTranscodeMode *string `json:"autoTranscodeMode"`
		IsPublic          *bool   `json:"isPublic"`
		Versioning        *bool   `json:"versioning"`
		Archived          *bool   `json:"archived"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	if req.AutoTranscodeMode != nil {
		v := *req.AutoTranscodeMode
		if v != "none" && v != "media" && v != "photos_only" {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_MODE",
				"autoTranscodeMode must be 'none', 'media', or 'photos_only'")
			return
		}
	}

	tag, err := h.DB.Exec(r.Context(),
		`UPDATE buckets SET auto_transcode_mode = COALESCE($1, auto_transcode_mode),
		                    is_public           = COALESCE($2, is_public),
		                    versioning          = COALESCE($3, versioning),
		                    archived            = COALESCE($4, archived)
		  WHERE owner_id = $5 AND name = $6`,
		req.AutoTranscodeMode, req.IsPublic, req.Versioning, req.Archived, u.ID, name)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HEAD /:bucket
func (h *BucketHandler) HeadBucket(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	name := chi.URLParam(r, "bucket")

	var id uuid.UUID
	err := h.DB.QueryRow(r.Context(),
		`SELECT id FROM buckets WHERE owner_id = $1 AND name = $2`, u.ID, name,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// DELETE /:bucket          → only if empty (returns 409 BUCKET_NOT_EMPTY otherwise)
// DELETE /:bucket?force=1  → wipes all objects + segment dirs + refunds quota,
//                            then deletes the bucket. Idempotent.
func (h *BucketHandler) DeleteBucket(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	name := chi.URLParam(r, "bucket")
	force := r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1"

	var bucketID uuid.UUID
	err := h.DB.QueryRow(r.Context(),
		`SELECT id FROM buckets WHERE owner_id = $1 AND name = $2`, u.ID, name,
	).Scan(&bucketID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// User-facing "is this bucket empty?" only counts live objects (NOT trash).
	// That's what the dashboard's confirmation dialog should show.
	var visibleCount int
	var visibleSize int64
	err = h.DB.QueryRow(r.Context(),
		`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		   FROM objects WHERE bucket_id = $1 AND NOT is_deleted`, bucketID,
	).Scan(&visibleCount, &visibleSize)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if visibleCount > 0 && !force {
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"code":        "BUCKET_NOT_EMPTY",
			"message":     fmt.Sprintf("bucket has %d objects (%d bytes); pass ?force=true to wipe", visibleCount, visibleSize),
			"objectCount": visibleCount,
			"totalBytes":  visibleSize,
		})
		return
	}

	if force {
		// For quota refund we need EVERY byte still on disk for this bucket:
		//   - live objects (size_bytes + transcoded HLS + pre-flight reservation)
		//   - soft-deleted objects sitting in trash (is_deleted=true) — same shape
		//   - every prior version under object_versions
		// CASCADE will wipe all three when the bucket row is dropped, so the
		// only way the user's quota stays correct is if we subtract them now.
		// Missing transcoded_bytes here is what caused "bucket empty but
		// used_bytes high" drift after force-deleting buckets with HLS content.
		var totalRefund int64
		err = h.DB.QueryRow(r.Context(), `
			SELECT
			  COALESCE((SELECT SUM(size_bytes
			                       + COALESCE(transcoded_bytes, 0)
			                       + COALESCE(transcode_reserved_bytes, 0))
			              FROM objects
			             WHERE bucket_id = $1), 0)
			+ COALESCE((SELECT SUM(ov.size_bytes)
			              FROM object_versions ov
			              JOIN objects o ON o.id = ov.object_id
			             WHERE o.bucket_id = $1), 0)`,
			bucketID,
		).Scan(&totalRefund)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
			return
		}

		// Collect object IDs so we can clean their segment dirs.
		rows, err := h.DB.Query(r.Context(),
			`SELECT id FROM objects WHERE bucket_id = $1`, bucketID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
			return
		}
		var objectIDs []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil { continue }
			objectIDs = append(objectIDs, id)
		}
		rows.Close()

		// Refund FIRST so a partial filesystem cleanup still leaves accounting balanced.
		if totalRefund > 0 {
			_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -totalRefund)
		}

		// Remove all transcoded output (segments dirs). The bucket dir itself
		// is wiped below via RemoveBucketDir.
		for _, id := range objectIDs {
			_ = os.RemoveAll(h.FS.SegmentsDir(id.String()))
		}
	}

	// DB cascade deletes objects + multipart_uploads + transcode_jobs.
	if _, err := h.DB.Exec(r.Context(),
		`DELETE FROM buckets WHERE id = $1`, bucketID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	_ = h.FS.RemoveBucketDir(bucketID.String())
	w.WriteHeader(http.StatusNoContent)
}
