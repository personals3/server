package handlers

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/personals3/api/internal/cache"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

// VersionDTO is one row returned by ?versions.
type VersionDTO struct {
	VersionID      string    `json:"versionId"`
	Size           int64     `json:"size"`
	ETag           string    `json:"etag,omitempty"`
	ContentType    string    `json:"contentType,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	IsCurrent      bool      `json:"isCurrent"`
	IsDeleteMarker bool      `json:"isDeleteMarker"`
}

// GET /:bucket/*?versions — list prior versions plus the current one.
// Newest first. Returns 404 if the bucket or object doesn't exist.
//
// "current" is a synthesized row pointing at objects.{...}; "prior" rows
// come from object_versions. A deleted (versioned) object has no current
// row visible, just the delete-marker as the most recent entry.
func (h *ObjectHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)

	bucketID, err := resolveBucket(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Find the object row (deleted or not — we want versions either way).
	var objectID uuid.UUID
	var curSize int64
	var curETag, curCT string
	var curUpdated time.Time
	var curDeleted bool
	err = h.DB.QueryRow(r.Context(), `
		SELECT id, size_bytes, etag, content_type, updated_at, is_deleted
		  FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&objectID, &curSize, &curETag, &curCT, &curUpdated, &curDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	rows, err := h.DB.Query(r.Context(), `
		SELECT version_id, size_bytes, etag, content_type, created_at, is_delete_marker
		  FROM object_versions
		 WHERE object_id = $1
		 ORDER BY created_at DESC`, objectID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []VersionDTO{}
	// Current first (if it's still a real object — not deleted).
	if !curDeleted {
		out = append(out, VersionDTO{
			VersionID:   "current",
			Size:        curSize,
			ETag:        curETag,
			ContentType: curCT,
			CreatedAt:   curUpdated,
			IsCurrent:   true,
		})
	}
	for rows.Next() {
		var v VersionDTO
		if err := rows.Scan(&v.VersionID, &v.Size, &v.ETag, &v.ContentType, &v.CreatedAt, &v.IsDeleteMarker); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, v)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"bucket":  bucketName,
		"key":     key,
		"count":   len(out),
		"deleted": curDeleted,
		"versions": out,
	})
}

// POST /:bucket/*?restore&versionId=X — copy a stored version back into
// the current data slot. The currently-live bytes (if any) are first
// snapshotted as a NEW version, so restore is itself reversible.
func (h *ObjectHandler) RestoreVersion(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)
	versionID := r.URL.Query().Get("versionId")
	if versionID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "versionId is required")
		return
	}

	bucketID, versioning, err := resolveBucketFull(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if !versioning {
		httpx.WriteError(w, http.StatusBadRequest, "VERSIONING_OFF",
			"this bucket does not have versioning enabled")
		return
	}

	// Find object + the target version.
	var objectID uuid.UUID
	var curSize int64
	var curETag, curCT string
	var curDeleted bool
	err = h.DB.QueryRow(r.Context(), `
		SELECT id, size_bytes, etag, content_type, is_deleted
		  FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&objectID, &curSize, &curETag, &curCT, &curDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	var vSize int64
	var vETag, vCT string
	var vIsDelMarker bool
	err = h.DB.QueryRow(r.Context(), `
		SELECT size_bytes, etag, content_type, is_delete_marker
		  FROM object_versions WHERE object_id = $1 AND version_id = $2`,
		objectID, versionID,
	).Scan(&vSize, &vETag, &vCT, &vIsDelMarker)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_VERSION", "version not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if vIsDelMarker {
		httpx.WriteError(w, http.StatusBadRequest, "DELETE_MARKER",
			"can't restore a delete marker — use DELETE to delete or restore an older version")
		return
	}

	// Quota: we will write vSize new bytes (the restored content).
	// If the current data file still exists (not deleted), we'll snapshot it
	// into versions/ first, so that also stays accounted-for as-is.
	if err := middleware.QuotaReserve(r.Context(), h.DB, u.ID, vSize); err != nil {
		if errors.Is(err, middleware.ErrQuotaExceeded) {
			httpx.WriteError(w, http.StatusInsufficientStorage, "QUOTA_EXCEEDED",
				"restoring this version would exceed your quota")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "QUOTA", err.Error())
		return
	}

	// 1. Snapshot the current data (only if there is one; deleted objects have none).
	if !curDeleted {
		newVID := newVersionID()
		snapPath, sErr := h.FS.SnapshotCurrent(bucketID.String(), key, newVID)
		if sErr != nil {
			_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -vSize)
			httpx.WriteError(w, http.StatusInternalServerError, "SNAPSHOT", sErr.Error())
			return
		}
		_, _ = h.DB.Exec(r.Context(), `
			INSERT INTO object_versions
			  (object_id, version_id, size_bytes, etag, content_type, storage_path)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			objectID, newVID, curSize, curETag, curCT, snapPath)
	}

	// 2. Copy the target version back into the current data slot.
	newSize, newETag, err := h.FS.PromoteVersion(bucketID.String(), key, versionID)
	if err != nil {
		// Worst case: data slot is empty and the snapshot we just made stays put.
		// User can re-restore the snapshot they just lost via the version list.
		_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -vSize)
		httpx.WriteError(w, http.StatusInternalServerError, "PROMOTE", err.Error())
		return
	}

	// 3. The previous data is being replaced — any transcode tied to the
	//    OLD content is now invalid. Cancel any in-flight worker, reap the
	//    old segments dir on disk, refund both the published transcoded_bytes
	//    AND any pre-flight reservation. Caller will need to re-trigger the
	//    transcode for the restored content if they want HLS.
	cache.PublishCancelObject(r.Context(), h.RDB, objectID, "version restored")
	_ = os.RemoveAll(h.FS.SegmentsDir(objectID.String()))
	var oldTrb, oldRsv int64
	_ = h.DB.QueryRow(r.Context(),
		`SELECT COALESCE(transcoded_bytes,0), COALESCE(transcode_reserved_bytes,0)
		   FROM objects WHERE id=$1`, objectID).Scan(&oldTrb, &oldRsv)
	if refund := oldTrb + oldRsv; refund > 0 {
		_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -refund)
	}

	// 4. Update the row to reflect the restored content. Un-mark is_deleted.
	storagePath := h.FS.ObjectPath(bucketID.String(), key)
	_, err = h.DB.Exec(r.Context(), `
		UPDATE objects
		   SET size_bytes               = $1,
		       etag                     = $2,
		       content_type             = $3,
		       storage_path             = $4,
		       is_deleted               = false,
		       transcoded               = '{}'::jsonb,
		       transcoded_bytes         = 0,
		       transcode_reserved_bytes = 0,
		       transcode_status         = 'none',
		       updated_at               = now()
		 WHERE id = $5`,
		newSize, newETag, vCT, storagePath, objectID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	_, _ = h.DB.Exec(r.Context(),
		`DELETE FROM transcode_jobs WHERE object_id = $1`, objectID)

	w.Header().Set("ETag", `"`+newETag+`"`)
	w.Header().Set("X-Object-Size", strconv.FormatInt(newSize, 10))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"restored":  versionID,
		"size":      newSize,
		"etag":      newETag,
	})
}

// DELETE /:bucket/*?versionId=X — permanently purge a single old version.
// Removes the file from disk + the object_versions row. Quota is refunded.
// Caller must own the bucket.
func (h *ObjectHandler) DeleteVersion(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)
	versionID := r.URL.Query().Get("versionId")
	if versionID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "versionId is required")
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

	var objectID uuid.UUID
	if err := h.DB.QueryRow(r.Context(),
		`SELECT id FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&objectID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	var size int64
	var isDelMarker bool
	err = h.DB.QueryRow(r.Context(), `
		DELETE FROM object_versions
		 WHERE object_id = $1 AND version_id = $2
		 RETURNING size_bytes, is_delete_marker`,
		objectID, versionID,
	).Scan(&size, &isDelMarker)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_VERSION", "version not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if !isDelMarker {
		// Remove the file from disk and refund.
		_ = h.FS.RemoveVersion(bucketID.String(), key, versionID)
		_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -size)
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /:bucket/*?versionId=X — download a specific historical version's bytes.
func (h *ObjectHandler) GetVersion(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)
	versionID := r.URL.Query().Get("versionId")
	if versionID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "versionId is required")
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

	var objectID uuid.UUID
	if err := h.DB.QueryRow(r.Context(),
		`SELECT id FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&objectID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	var size int64
	var etag, contentType string
	var createdAt time.Time
	var isDelMarker bool
	err = h.DB.QueryRow(r.Context(), `
		SELECT size_bytes, etag, content_type, created_at, is_delete_marker
		  FROM object_versions WHERE object_id = $1 AND version_id = $2`,
		objectID, versionID,
	).Scan(&size, &etag, &contentType, &createdAt, &isDelMarker)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_VERSION", "version not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if isDelMarker {
		httpx.WriteError(w, http.StatusGone, "DELETE_MARKER",
			"this version is a delete marker — no bytes to download")
		return
	}

	file, err := h.FS.OpenVersion(bucketID.String(), key, versionID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "FS_READ", err.Error())
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Version-Id", versionID)
	http.ServeContent(w, r, key, createdAt, file)
}
