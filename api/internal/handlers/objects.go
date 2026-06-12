package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/personals3/api/internal/cache"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/sharding"
	"github.com/personals3/api/internal/storage"
)

var _ = regexp.MustCompile

// regexpMustCompile is a thin wrapper used by the videoQualityRE init; here
// so the import is unambiguously used.
func regexpMustCompile(s string) *regexp.Regexp { return regexp.MustCompile(s) }

type ObjectHandler struct {
	DB  *pgxpool.Pool
	FS  *storage.FS
	RDB *redis.Client
}

type ObjectDTO struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	ContentType  string    `json:"contentType"`
	LastModified time.Time `json:"lastModified"`
}

// resolveBucket returns the bucket id for (currentUser, name). 404 if not found.
func resolveBucket(r *http.Request, db *pgxpool.Pool, bucketName string) (uuid.UUID, error) {
	u := middleware.MustUser(r.Context())
	var id uuid.UUID
	err := db.QueryRow(r.Context(),
		`SELECT id FROM buckets WHERE owner_id = $1 AND name = $2`, u.ID, bucketName,
	).Scan(&id)
	return id, err
}

// resolveBucketFull returns id + the versioning flag in one round trip. Used
// on the hot paths (PUT, DELETE) that need to know whether to snapshot.
func resolveBucketFull(r *http.Request, db *pgxpool.Pool, bucketName string) (uuid.UUID, bool, error) {
	u := middleware.MustUser(r.Context())
	var id uuid.UUID
	var versioning bool
	err := db.QueryRow(r.Context(),
		`SELECT id, versioning FROM buckets WHERE owner_id = $1 AND name = $2`,
		u.ID, bucketName,
	).Scan(&id, &versioning)
	return id, versioning, err
}

// newVersionID returns a short opaque token used to identify one snapshot.
// Format: lowercase-hex random 16 bytes — fits cleanly in a URL.
func newVersionID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// keyParam pulls the catch-all "*" route param (the object key, which may
// have slashes) and URL-decodes it. Chi preserves the encoded form in
// catch-all params, so we must decode ourselves — otherwise filenames with
// spaces or special chars get stored doubly-encoded and become un-deletable.
func keyParam(r *http.Request) string {
	raw := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

// PUT /:bucket/*
// Auth required. Enforces atomic quota + global disk-fullness check.
func (h *ObjectHandler) PutObject(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)
	if key == "" {
		httpx.WriteError(w, http.StatusBadRequest, "INVALID_KEY", "object key required")
		return
	}

	// Global disk health check (separate from per-user quota)
	if err := middleware.CheckDiskHealthy(r.Context(), h.DB, h.FS.Root()); err != nil {
		httpx.WriteError(w, http.StatusInsufficientStorage, "DISK_FULL",
			"system storage is too full to accept new uploads (admin: lower threshold or add disk)")
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

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// ----- Pre-check quota using Content-Length (if known) ---------------------
	// Subtract any existing object's size from the upcoming reservation, since
	// PUT to an existing key replaces it (net delta, not gross).
	//
	// EXCEPT: when versioning is on we KEEP the old bytes (snapshot to
	// versions/), so the reservation is the full new size, not a delta.
	var (
		existingSize  int64
		existingObjID uuid.UUID
		existingETag  string
		existingCT    string
		hasExisting   bool
	)
	err = h.DB.QueryRow(r.Context(),
		`SELECT id, size_bytes, etag, content_type
		   FROM objects
		  WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&existingObjID, &existingSize, &existingETag, &existingCT)
	if err == nil {
		hasExisting = true
	}
	// pgx.ErrNoRows → leave hasExisting=false, existingSize=0

	contentLength := r.ContentLength // -1 if unknown
	// reserved = bytes actually charged up front, NOT the raw delta
	// computed from Content-Length. A shrinking overwrite (new size <
	// existing, versioning off) computes a negative delta, but that
	// credit must wait until the new bytes are safely on disk — so
	// nothing is applied here and the post-write reconciliation below
	// settles it. Folding the unapplied negative delta into `reserved`
	// used to cancel that settlement: the freed bytes stayed charged
	// until quota-reconcile.sql, and any size divergence on top computed
	// its adjustment from a reservation that never happened.
	reserved := int64(0)
	if contentLength >= 0 {
		delta := contentLength
		if !versioning {
			delta -= existingSize // in-place replacement: net delta
		} // versioning: old bytes stay on disk as a snapshot; no subtract
		if delta > 0 {
			if err := middleware.QuotaReserve(r.Context(), h.DB, u.ID, delta); err != nil {
				if errors.Is(err, middleware.ErrQuotaExceeded) {
					httpx.WriteError(w, http.StatusInsufficientStorage, "QUOTA_EXCEEDED",
						"this upload would exceed your storage quota")
					return
				}
				httpx.WriteError(w, http.StatusInternalServerError, "QUOTA", err.Error())
				return
			}
			reserved = delta
		}
	}

	// ----- Versioning: snapshot the current data file BEFORE we overwrite it --
	// Atomic os.Rename; failure here aborts the PUT so we don't lose the old
	// bytes on a half-completed snapshot.
	var snapVersionID, snapPath string
	if versioning && hasExisting {
		snapVersionID = newVersionID()
		snapPath, err = h.FS.SnapshotCurrent(bucketID.String(), key, snapVersionID)
		if err != nil {
			if reserved > 0 {
				_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -reserved)
			}
			httpx.WriteError(w, http.StatusInternalServerError, "SNAPSHOT", err.Error())
			return
		}
	}

	// ----- Write to disk -------------------------------------------------------
	size, etag, err := h.FS.WriteObject(bucketID.String(), key, r.Body)
	if err != nil {
		// Refund the reservation + un-snapshot if we already moved the old file aside.
		if reserved > 0 {
			_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -reserved)
		}
		if snapPath != "" {
			// Best-effort: move snapshot back to data so the bucket isn't left empty.
			_ = os.Rename(snapPath, h.FS.ObjectPath(bucketID.String(), key))
		}
		httpx.WriteError(w, http.StatusInternalServerError, "FS_WRITE", err.Error())
		return
	}

	// ----- Reconcile quota if actual size differs from reservation -------------
	// With versioning on, old bytes stay on disk → delta is the full new size.
	// Without versioning, delta is new - old (in-place replacement).
	var actualDelta int64
	if versioning {
		actualDelta = size
	} else {
		actualDelta = size - existingSize
	}
	if actualDelta != reserved {
		adjustment := actualDelta - reserved
		if err := quotaAdjust(r.Context(), h.DB, u.ID, adjustment); err != nil {
			// Quota accounting no longer matches the file we just wrote —
			// whether the adjustment was rejected (body larger than
			// Content-Length claimed) or simply never landed (DB error).
			// Undo the write, restore any snapshot, and refund what was
			// actually charged (the pre-reservation only ran when positive).
			_ = h.FS.RemoveCurrentOnly(bucketID.String(), key)
			if snapPath != "" {
				_ = os.Rename(snapPath, h.FS.ObjectPath(bucketID.String(), key))
			}
			if reserved > 0 {
				_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -reserved)
			}
			if errors.Is(err, middleware.ErrQuotaExceeded) {
				httpx.WriteError(w, http.StatusInsufficientStorage, "QUOTA_EXCEEDED",
					"upload exceeded your storage quota")
				return
			}
			httpx.WriteError(w, http.StatusInternalServerError, "QUOTA", err.Error())
			return
		}
	}

	storagePath := h.FS.ObjectPath(bucketID.String(), key)

	var objectID uuid.UUID
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO objects (bucket_id, key, size_bytes, etag, content_type, storage_path)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (bucket_id, key) DO UPDATE SET
		  size_bytes   = EXCLUDED.size_bytes,
		  etag         = EXCLUDED.etag,
		  content_type = EXCLUDED.content_type,
		  storage_path = EXCLUDED.storage_path,
		  is_deleted   = false,
		  updated_at   = now()
		RETURNING id`,
		bucketID, key, size, etag, contentType, storagePath,
	).Scan(&objectID)
	if err != nil {
		// Worst case: file is on disk + quota charged, but no DB row. Best-effort cleanup.
		_ = h.FS.RemoveCurrentOnly(bucketID.String(), key)
		_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -actualDelta)
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// ----- Record the snapshot as a prior version ------------------------------
	if snapVersionID != "" {
		_, _ = h.DB.Exec(r.Context(), `
			INSERT INTO object_versions
			  (object_id, version_id, size_bytes, etag, content_type, storage_path)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			existingObjID, snapVersionID, existingSize, existingETag, existingCT, snapPath,
		)
	}

	// ----- Shard index (V4) -------------------------------------------------
	// Only bump the shard for genuinely new objects; an UPSERT that hit the
	// ON CONFLICT path didn't add to the count.
	if !hasExisting {
		_ = sharding.OnObjectAdded(r.Context(), h.DB, bucketID, objectID, key)
	}

	// Fire-and-forget transcode enqueue (respects bucket auto_transcode_mode
	// and any X-PS3-Transcode header on this request).
	enqueueTranscode(r.Context(), h.DB, h.FS, objectID, bucketID, key, contentType, r)

	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("X-Object-Size", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
}

// GET /:bucket/*
func (h *ObjectHandler) GetObject(w http.ResponseWriter, r *http.Request) {
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

	var objectID uuid.UUID
	var size int64
	var etag, contentType string
	var updatedAt time.Time
	err = h.DB.QueryRow(r.Context(), `
		SELECT id, size_bytes, etag, content_type, updated_at
		  FROM objects
		 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&objectID, &size, &etag, &contentType, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// On-the-fly resize when any of ?w/?h/?fit/?q/?fmt is present.
	// Returns true if it handled the response.
	srcPath := h.FS.ObjectPath(bucketID.String(), key)
	if serveResizedFS(w, r, h.FS, objectID, srcPath, contentType) {
		return
	}

	file, err := h.FS.OpenObject(bucketID.String(), key)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "FS_READ", err.Error())
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	// http.ServeContent sets Content-Length and handles Range requests.
	http.ServeContent(w, r, key, updatedAt, file)
}

// HEAD /:bucket/*
func (h *ObjectHandler) HeadObject(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)

	bucketID, err := resolveBucket(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var size int64
	var etag, contentType string
	var updatedAt time.Time
	err = h.DB.QueryRow(r.Context(), `
		SELECT size_bytes, etag, content_type, updated_at
		  FROM objects
		 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&size, &etag, &contentType, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Last-Modified", updatedAt.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

// DELETE /:bucket/* — soft delete.
//
// Every bucket: the object's row is marked is_deleted=true with deleted_at=now().
// The file stays on disk and still counts toward the user's quota until the
// trash bin is emptied (or a future background vacuum sweeps it).
//
// Versioning ON additionally snapshots the current data file into versions/
// and writes a delete-marker version row. Restore picks the latest non-marker
// version. Quota is also not refunded on soft delete.
//
// A future "permanent delete" shortcut from the UI will hit ?purge to skip
// the trash entirely.
func (h *ObjectHandler) DeleteObject(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)

	bucketID, versioning, err := resolveBucketFull(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if versioning {
		h.deleteObjectVersioned(w, r, bucketID, key)
		return
	}

	// --- non-versioned: soft-delete (UPDATE, not DELETE). File stays on disk. ---
	var objectID uuid.UUID
	err = h.DB.QueryRow(r.Context(), `
		UPDATE objects
		   SET is_deleted = true, deleted_at = now(), updated_at = now()
		 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted
		 RETURNING id`,
		bucketID, key).Scan(&objectID)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	// Tell the worker to stop any in-flight transcode for this object so
	// the worker thread frees up immediately instead of finishing useless work.
	cache.PublishCancelObject(r.Context(), h.RDB, objectID, "object soft-deleted")
	w.WriteHeader(http.StatusNoContent)
}

// PurgeObject hard-deletes a single object regardless of trash state. This is
// the "shortcut for permanent delete" path — caller pays a small UX cost
// (probably a confirm dialog or shift-click) in exchange for skipping the
// trash entirely. Refunds quota for the current bytes + every stored version.
//
// Idempotent for already-purged keys: returns 404.
func (h *ObjectHandler) PurgeObject(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
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

	var objectID uuid.UUID
	var totalSize int64
	var shardPath string
	err = h.DB.QueryRow(r.Context(), `
		SELECT id,
		       size_bytes
		       + COALESCE(transcoded_bytes, 0)
		       + COALESCE(transcode_reserved_bytes, 0)
		       + COALESCE((SELECT SUM(size_bytes)
		                     FROM object_versions WHERE object_id = objects.id), 0),
		       COALESCE(shard_path, '')
		  FROM objects
		 WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&objectID, &totalSize, &shardPath)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// CASCADE removes object_versions + transcode_jobs in one shot.
	if _, err := h.DB.Exec(r.Context(),
		`DELETE FROM objects WHERE id = $1`, objectID,
	); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Shard index bookkeeping — the deleted hash dir is no longer alive.
	_ = sharding.OnObjectRemoved(r.Context(), h.DB, bucketID, shardPath)

	// Best-effort cancel for any worker mid-transcode on this object before
	// we wipe its segments dir.
	cache.PublishCancelObject(r.Context(), h.RDB, objectID, "object purged")
	_ = h.FS.RemoveObject(bucketID.String(), key)
	_ = os.RemoveAll(h.FS.SegmentsDir(objectID.String()))
	_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -totalSize)

	w.WriteHeader(http.StatusNoContent)
}

// softDeleteOne implements the versioning-soft-delete flow for one key.
// Returns pgx.ErrNoRows if the key doesn't exist (or is already deleted),
// otherwise nil on success.
func (h *ObjectHandler) softDeleteOne(r *http.Request, bucketID uuid.UUID, key string) error {
	var objectID uuid.UUID
	var size int64
	var etag, contentType string
	err := h.DB.QueryRow(r.Context(),
		`SELECT id, size_bytes, etag, content_type
		   FROM objects
		  WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&objectID, &size, &etag, &contentType)
	if err != nil {
		return err
	}

	snapVID := newVersionID()
	snapPath, err := h.FS.SnapshotCurrent(bucketID.String(), key, snapVID)
	if err != nil {
		return err
	}
	_, _ = h.DB.Exec(r.Context(), `
		INSERT INTO object_versions
		  (object_id, version_id, size_bytes, etag, content_type, storage_path)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		objectID, snapVID, size, etag, contentType, snapPath)
	_, _ = h.DB.Exec(r.Context(), `
		INSERT INTO object_versions
		  (object_id, version_id, size_bytes, etag, content_type, storage_path, is_delete_marker)
		VALUES ($1, $2, 0, '', '', '', true)`,
		objectID, newVersionID())
	_, err = h.DB.Exec(r.Context(),
		`UPDATE objects SET is_deleted = true, deleted_at = now(), updated_at = now()
		 WHERE id = $1`, objectID)
	if err == nil {
		cache.PublishCancelObject(r.Context(), h.RDB, objectID, "object soft-deleted (versioned bulk)")
	}
	return err
}

// deleteObjectVersioned is the versioning-on branch of DeleteObject. Snapshots
// the current data, inserts a delete-marker version row, and leaves the
// objects row in place with is_deleted=true.
func (h *ObjectHandler) deleteObjectVersioned(w http.ResponseWriter, r *http.Request, bucketID uuid.UUID, key string) {
	var objectID uuid.UUID
	var size int64
	var etag, contentType string
	err := h.DB.QueryRow(r.Context(),
		`SELECT id, size_bytes, etag, content_type
		   FROM objects
		  WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&objectID, &size, &etag, &contentType)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// 1. Snapshot the current data file aside.
	snapVID := newVersionID()
	snapPath, err := h.FS.SnapshotCurrent(bucketID.String(), key, snapVID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "SNAPSHOT", err.Error())
		return
	}

	// 2. Record this snapshot as a prior version.
	_, _ = h.DB.Exec(r.Context(), `
		INSERT INTO object_versions
		  (object_id, version_id, size_bytes, etag, content_type, storage_path)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		objectID, snapVID, size, etag, contentType, snapPath)

	// 3. Insert the delete-marker row.
	_, _ = h.DB.Exec(r.Context(), `
		INSERT INTO object_versions
		  (object_id, version_id, size_bytes, etag, content_type, storage_path, is_delete_marker)
		VALUES ($1, $2, 0, '', '', '', true)`,
		objectID, newVersionID())

	// 4. Mark the current row as deleted. The row stays (for restore) but
	//    will be hidden from listings, GET, and HEAD.
	_, err = h.DB.Exec(r.Context(),
		`UPDATE objects SET is_deleted = true, deleted_at = now(), updated_at = now()
		 WHERE id = $1`, objectID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	cache.PublishCancelObject(r.Context(), h.RDB, objectID, "object soft-deleted (versioned)")
	w.WriteHeader(http.StatusNoContent)
}

// ObjectInfoDTO is returned by GET /:bucket/*?info.
type ObjectInfoDTO struct {
	ObjectID        uuid.UUID        `json:"objectId"`
	Bucket          string           `json:"bucket"`
	Key             string           `json:"key"`
	Size            int64            `json:"size"`
	ETag            string           `json:"etag"`
	ContentType     string           `json:"contentType"`
	LastModified    time.Time        `json:"lastModified"`
	Transcoded      any              `json:"transcoded"`
	TranscodeStatus string           `json:"transcodeStatus"`
	// Per-job breakdown of the transcoding pipeline. Empty when no jobs exist.
	Pipeline        []PipelineJobDTO `json:"pipeline,omitempty"`
	// Overall progress 0-100, weighted by encoding cost (height²-proportional).
	// A 1080p done weighs ~5x as much as a 360p done, so the bar advances
	// roughly linearly in wall-time instead of jumping at each job boundary.
	OverallPct      int              `json:"overallPct"`
}

type PipelineJobDTO struct {
	FileType    string  `json:"fileType"`              // e.g. "video_quality_720p"
	Status      string  `json:"status"`                // pending/processing/done/failed/skipped/waiting
	ProgressPct int     `json:"progressPct"`           // 0-100
	Weight      float64 `json:"weight,omitempty"`      // fraction of total work (0..1)
	Error       string  `json:"error,omitempty"`
}

// weightForFileType returns a relative work weight per pipeline sub-job.
//
//	video_quality_{height}p → height² × 16/9      (pixel-area proportional)
//	video_thumbnails        → ~5% of the largest quality
//	video_finalize          → ~0.5% (just file I/O + master.m3u8 write)
//	audio / image           → 1.0 each (legacy single-job paths)
//
// Returned values are unnormalized; the caller normalizes against the sum.
func weightForFileType(fileType string) float64 {
	if m := videoQualityRE.FindStringSubmatch(fileType); m != nil {
		h, _ := strconv.Atoi(m[1])
		// pixels = h * (h * 16/9). Divide by a large constant just for nicer numbers.
		return float64(h) * float64(h) * 16.0 / 9.0 / 1_000_000.0
	}
	switch fileType {
	case "video_thumbnails":
		return 0.5 // fast — a 1080p quality is ~2.0 so this is ~25% of it; tweak later
	case "video_finalize":
		return 0.01
	case "video": // legacy monolithic
		return 1.0
	case "audio":
		return 1.0
	case "image":
		return 1.0
	}
	return 1.0
}

var videoQualityRE = regexpMustCompile(`^video_quality_(\d+)p$`)

// computeOverallPct turns a slice of pipeline jobs into a single weighted
// percentage. "done" and "skipped" count as 100%; "processing" uses progress_pct.
// Everything else (pending, waiting, failed) counts as 0%.
func computeOverallPct(jobs []PipelineJobDTO) int {
	if len(jobs) == 0 {
		return 0
	}
	var totalW, doneW float64
	for _, j := range jobs {
		w := weightForFileType(j.FileType)
		totalW += w
		switch j.Status {
		case "done", "skipped":
			doneW += w
		case "processing":
			doneW += w * float64(j.ProgressPct) / 100.0
		}
	}
	if totalW == 0 {
		return 0
	}
	pct := int(doneW / totalW * 100)
	if pct > 100 { pct = 100 }
	if pct < 0   { pct = 0 }
	return pct
}

// GET /:bucket/*?info — full object metadata (no body).
func (h *ObjectHandler) ObjectInfo(w http.ResponseWriter, r *http.Request) {
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

	var out ObjectInfoDTO
	out.Bucket = bucketName
	out.Key = key
	var transcodedJSON []byte
	err = h.DB.QueryRow(r.Context(), `
		SELECT id, size_bytes, etag, content_type, updated_at,
		       transcoded::text, transcode_status
		  FROM objects
		 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&out.ObjectID, &out.Size, &out.ETag, &out.ContentType,
		&out.LastModified, &transcodedJSON, &out.TranscodeStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if len(transcodedJSON) > 0 {
		var t any
		if json.Unmarshal(transcodedJSON, &t) == nil {
			out.Transcoded = t
		}
	}

	// Pipeline breakdown — only fetch when transcoding is active or recently done
	pipeRows, err := h.DB.Query(r.Context(), `
		SELECT file_type, status, progress_pct, COALESCE(error, '')
		  FROM transcode_jobs
		 WHERE object_id = $1
		 ORDER BY
		   CASE
		     WHEN file_type LIKE 'video_quality_%' THEN 1
		     WHEN file_type = 'video_thumbnails'   THEN 2
		     WHEN file_type = 'video_finalize'     THEN 3
		     ELSE 0
		   END,
		   file_type`,
		out.ObjectID)
	if err == nil {
		for pipeRows.Next() {
			var p PipelineJobDTO
			if err := pipeRows.Scan(&p.FileType, &p.Status, &p.ProgressPct, &p.Error); err == nil {
				out.Pipeline = append(out.Pipeline, p)
			}
		}
		pipeRows.Close()
	}

	// Annotate per-job weight + compute the weighted overall.
	var totalW float64
	for _, j := range out.Pipeline {
		totalW += weightForFileType(j.FileType)
	}
	if totalW > 0 {
		for i, j := range out.Pipeline {
			out.Pipeline[i].Weight = weightForFileType(j.FileType) / totalW
		}
	}
	out.OverallPct = computeOverallPct(out.Pipeline)

	httpx.WriteJSON(w, http.StatusOK, out)
}

// POST /:bucket?delete — bulk delete a list of keys.
//
// Body: { "keys": ["a/b.txt", "c.jpg", ...] }
// Response: { "deleted": N, "errors": [{"key", "error"}], "refundedBytes": M }
//
// Soft-delete each key (trash bin). Use POST /trash?purge to permanently
// remove items from the trash and refund quota.
func (h *ObjectHandler) BulkDelete(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")

	var body struct{ Keys []string `json:"keys"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	if len(body.Keys) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "NO_KEYS", "keys array is required and non-empty")
		return
	}
	if len(body.Keys) > 1000 {
		httpx.WriteError(w, http.StatusBadRequest, "TOO_MANY", "max 1000 keys per request")
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

	type delErr struct {
		Key   string `json:"key"`
		Error string `json:"error"`
	}
	deleted := 0
	errs := []delErr{}

	for _, key := range body.Keys {
		if versioning {
			// Soft-delete each key: snapshot to versions/, mark deleted.
			if err := h.softDeleteOne(r, bucketID, key); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					errs = append(errs, delErr{Key: key, Error: "not found"})
				} else {
					errs = append(errs, delErr{Key: key, Error: err.Error()})
				}
				continue
			}
			deleted++
			continue
		}

		// Non-versioned: soft-delete via UPDATE. File stays on disk.
		var objectID uuid.UUID
		err = h.DB.QueryRow(r.Context(), `
			UPDATE objects
			   SET is_deleted = true, deleted_at = now(), updated_at = now()
			 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted
			 RETURNING id`,
			bucketID, key).Scan(&objectID)
		if errors.Is(err, pgx.ErrNoRows) {
			errs = append(errs, delErr{Key: key, Error: "not found"})
			continue
		}
		if err != nil {
			errs = append(errs, delErr{Key: key, Error: err.Error()})
			continue
		}
		cache.PublishCancelObject(r.Context(), h.RDB, objectID, "object soft-deleted (bulk)")
		deleted++
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"deleted": deleted,
		"errors":  errs,
		// Soft delete refunds nothing; the bytes are still on disk in trash.
		"refundedBytes": 0,
		"movedToTrash":  deleted,
	})
}

// GET /:bucket — list objects.
//
// Query params:
//   ?prefix=foo/        — keys must start with this
//   ?delimiter=/        — group keys by the next "/" after prefix:
//                         keys with no further delimiter return in Contents,
//                         everything else rolls up into CommonPrefixes ("folders")
//   ?max-keys=N         — cap, default & max 1000
//
// This matches S3 ListObjectsV2 semantics. The dashboard uses delimiter=/
// to render a folder browser; flat listings (delimiter omitted) still work
// for rclone, scripts, etc.
func (h *ObjectHandler) ListObjects(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")
	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	maxKeysStr := r.URL.Query().Get("max-keys")
	maxKeys := 1000
	if maxKeysStr != "" {
		if n, err := strconv.Atoi(maxKeysStr); err == nil && n > 0 && n <= 1000 {
			maxKeys = n
		}
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

	// Always fetch by prefix; the delimiter grouping is done in-process so the
	// query stays a simple index scan. For very large folders this is fine
	// because we cap at 1000 keys.
	rows, err := h.DB.Query(r.Context(), `
		SELECT key, size_bytes, etag, content_type, updated_at
		  FROM objects
		 WHERE bucket_id = $1
		   AND NOT is_deleted
		   AND key LIKE $2 || '%'
		 ORDER BY key
		 LIMIT $3`, bucketID, prefix, maxKeys)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	raw := []ObjectDTO{}
	for rows.Next() {
		var o ObjectDTO
		if err := rows.Scan(&o.Key, &o.Size, &o.ETag, &o.ContentType, &o.LastModified); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		raw = append(raw, o)
	}

	// Split raw rows into direct children (objects in the "current folder")
	// and common prefixes (folders) when delimiter is set.
	out := raw
	commonPrefixes := []string{}
	if delimiter != "" {
		out = out[:0]
		seen := map[string]bool{}
		for _, o := range raw {
			after := strings.TrimPrefix(o.Key, prefix)
			if i := strings.Index(after, delimiter); i >= 0 {
				cp := prefix + after[:i+len(delimiter)]
				if !seen[cp] {
					seen[cp] = true
					commonPrefixes = append(commonPrefixes, cp)
				}
				continue
			}
			out = append(out, o)
		}
	}

	if isS3Client(r) {
		res := listBucketResultV2{
			XMLNS: s3XMLNS, Name: bucketName, Prefix: prefix, Delimiter: delimiter,
			KeyCount: len(out) + len(commonPrefixes), MaxKeys: maxKeys,
			IsTruncated: len(raw) == maxKeys,
		}
		for _, o := range out {
			res.Contents = append(res.Contents, s3Object{
				Key: o.Key, LastModified: o.LastModified, ETag: `"` + o.ETag + `"`,
				Size: o.Size, StorageClass: "STANDARD",
			})
		}
		for _, cp := range commonPrefixes {
			res.CommonPrefixes = append(res.CommonPrefixes, s3CommonPrefix{Prefix: cp})
		}
		writeXML(w, http.StatusOK, res)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"bucket":         bucketName,
		"prefix":         prefix,
		"delimiter":      delimiter,
		"maxKeys":        maxKeys,
		"count":          len(out),
		"objects":        out,
		"commonPrefixes": commonPrefixes,
		"truncated":      len(raw) == maxKeys,
	})
}
