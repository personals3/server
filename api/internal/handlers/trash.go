package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

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

// TrashHandler serves /trash — the per-user soft-deleted-objects view.
//
//   GET    /trash                  → list every is_deleted=true object the user owns
//   POST   /trash?restore          → restore one or more by {bucket, key}
//   DELETE /trash?purge            → permanently delete one or more (refunds quota)
//   DELETE /trash                  → empty everything (purges every trashed item)
type TrashHandler struct {
	DB  *pgxpool.Pool
	FS  *storage.FS
	RDB *redis.Client
}

type trashItem struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

type trashItemDTO struct {
	Bucket      string    `json:"bucket"`
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	DeletedAt   time.Time `json:"deletedAt"`
	// Hint to the dashboard whether restore will pick a prior version
	// (versioning bucket) vs. just flip is_deleted (non-versioning bucket).
	FromVersionedBucket bool `json:"fromVersionedBucket"`
}

// GET /trash — list the user's soft-deleted objects.
//
// Newest deletions first. Pagination is a future concern; we cap at 1000.
func (h *TrashHandler) List(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())

	rows, err := h.DB.Query(r.Context(), `
		SELECT b.name, o.key, o.size_bytes, o.content_type,
		       COALESCE(o.deleted_at, o.updated_at) AS deleted_at,
		       b.versioning
		  FROM objects o
		  JOIN buckets b ON b.id = o.bucket_id
		 WHERE b.owner_id = $1 AND o.is_deleted = TRUE
		 ORDER BY deleted_at DESC NULLS LAST
		 LIMIT 1000`, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []trashItemDTO{}
	var total int64
	for rows.Next() {
		var it trashItemDTO
		if err := rows.Scan(&it.Bucket, &it.Key, &it.Size, &it.ContentType,
			&it.DeletedAt, &it.FromVersionedBucket); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, it)
		total += it.Size
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":      out,
		"count":      len(out),
		"totalBytes": total, // still counts toward quota
	})
}

// POST /trash?restore — restore one or more soft-deleted objects.
// Body: { "items": [{"bucket", "key"}, ...] }
//
// Versioning bucket: flip is_deleted=false on the row (the latest non-delete
// version on disk is already at `data` for non-versioning, OR will be
// promoted via the latest version row for versioning. To keep the code
// simple, we just flip is_deleted. For versioning buckets where the user
// wants a specific version, they should use the per-object restore flow
// instead — this one just resurrects whatever the row points to.)
func (h *TrashHandler) Restore(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	items, err := decodeTrashItems(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}

	type itemErr struct {
		Bucket string `json:"bucket"`
		Key    string `json:"key"`
		Error  string `json:"error"`
	}
	restored := 0
	errs := []itemErr{}

	for _, it := range items {
		// Versioning bucket needs us to promote the latest stored version
		// back to `data` because soft-delete moved the file aside.
		var bucketID uuid.UUID
		var versioning bool
		err := h.DB.QueryRow(r.Context(),
			`SELECT id, versioning FROM buckets WHERE owner_id = $1 AND name = $2`,
			u.ID, it.Bucket,
		).Scan(&bucketID, &versioning)
		if errors.Is(err, pgx.ErrNoRows) {
			errs = append(errs, itemErr{it.Bucket, it.Key, "bucket not found"})
			continue
		}
		if err != nil {
			errs = append(errs, itemErr{it.Bucket, it.Key, err.Error()})
			continue
		}

		if versioning {
			if e := h.restoreVersioned(r, bucketID, it.Key); e != nil {
				errs = append(errs, itemErr{it.Bucket, it.Key, e.Error()})
				continue
			}
		} else {
			tag, e := h.DB.Exec(r.Context(), `
				UPDATE objects
				   SET is_deleted = false, deleted_at = NULL, updated_at = now()
				 WHERE bucket_id = $1 AND key = $2 AND is_deleted = TRUE`,
				bucketID, it.Key)
			if e != nil {
				errs = append(errs, itemErr{it.Bucket, it.Key, e.Error()})
				continue
			}
			if tag.RowsAffected() == 0 {
				errs = append(errs, itemErr{it.Bucket, it.Key, "not in trash"})
				continue
			}
		}
		restored++
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"restored": restored,
		"errors":   errs,
	})
}

// restoreVersioned undoes a versioning-bucket soft delete: promote the
// newest non-delete-marker version back into `data`, drop the latest
// delete-marker row, then flip is_deleted=false.
func (h *TrashHandler) restoreVersioned(r *http.Request, bucketID uuid.UUID, key string) error {
	var objectID uuid.UUID
	if err := h.DB.QueryRow(r.Context(),
		`SELECT id FROM objects WHERE bucket_id = $1 AND key = $2 AND is_deleted = TRUE`,
		bucketID, key,
	).Scan(&objectID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("not in trash")
		}
		return err
	}

	// Find the newest non-delete-marker version.
	var versionID, etag, contentType string
	var size int64
	err := h.DB.QueryRow(r.Context(), `
		SELECT version_id, size_bytes, etag, content_type
		  FROM object_versions
		 WHERE object_id = $1 AND is_delete_marker = FALSE
		 ORDER BY created_at DESC
		 LIMIT 1`, objectID,
	).Scan(&versionID, &size, &etag, &contentType)
	if errors.Is(err, pgx.ErrNoRows) {
		// No real version to bring back; just un-mark deleted and hope
		// the data slot still has content. (Shouldn't happen, but safe.)
		_, e := h.DB.Exec(r.Context(),
			`UPDATE objects SET is_deleted = false, deleted_at = NULL,
			                    updated_at = now() WHERE id = $1`, objectID)
		return e
	}
	if err != nil {
		return err
	}

	// Promote the version back to the data slot.
	newSize, newETag, err := h.FS.PromoteVersion(bucketID.String(), key, versionID)
	if err != nil {
		return err
	}

	storagePath := h.FS.ObjectPath(bucketID.String(), key)
	if _, err := h.DB.Exec(r.Context(), `
		UPDATE objects
		   SET size_bytes   = $1,
		       etag         = $2,
		       content_type = $3,
		       storage_path = $4,
		       is_deleted   = false,
		       deleted_at   = NULL,
		       updated_at   = now()
		 WHERE id = $5`,
		newSize, newETag, contentType, storagePath, objectID,
	); err != nil {
		return err
	}

	// Drop the most-recent delete-marker so it doesn't confuse the version list.
	_, _ = h.DB.Exec(r.Context(), `
		DELETE FROM object_versions
		 WHERE id IN (
		   SELECT id FROM object_versions
		    WHERE object_id = $1 AND is_delete_marker = TRUE
		    ORDER BY created_at DESC LIMIT 1
		 )`, objectID)

	return nil
}

// DELETE /trash?purge → permanently delete items from the trash.
// Body: { "items": [{"bucket", "key"}, ...] }   (omit to empty everything)
//
// Refunds quota. Deletes the file from disk, the segments dir, ALL prior
// versions for that object (and their files). The DB row is hard-deleted.
func (h *TrashHandler) Purge(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())

	// "Empty trash" → no body, delete every is_deleted row for this user.
	if r.URL.Query().Get("all") == "1" {
		h.emptyAll(w, r, u.ID)
		return
	}

	items, err := decodeTrashItems(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}

	type itemErr struct {
		Bucket string `json:"bucket"`
		Key    string `json:"key"`
		Error  string `json:"error"`
	}
	purged := 0
	errs := []itemErr{}
	var refund int64

	for _, it := range items {
		bytes, e := h.purgeOne(r, u.ID, it.Bucket, it.Key)
		if e != nil {
			errs = append(errs, itemErr{it.Bucket, it.Key, e.Error()})
			continue
		}
		refund += bytes
		purged++
	}

	if refund > 0 {
		_ = middleware.QuotaReserve(r.Context(), h.DB, u.ID, -refund)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"purged":        purged,
		"errors":        errs,
		"refundedBytes": refund,
	})
}

// emptyAll iterates every is_deleted row for the user and hard-deletes them.
// Quota refund is summed and applied once at the end.
func (h *TrashHandler) emptyAll(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	rows, err := h.DB.Query(r.Context(), `
		SELECT b.name, o.key
		  FROM objects o
		  JOIN buckets b ON b.id = o.bucket_id
		 WHERE b.owner_id = $1 AND o.is_deleted = TRUE`, userID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	purged := 0
	var refund int64
	for rows.Next() {
		var bucket, key string
		if err := rows.Scan(&bucket, &key); err != nil {
			continue
		}
		if bytes, e := h.purgeOne(r, userID, bucket, key); e == nil {
			refund += bytes
			purged++
		}
	}

	if refund > 0 {
		_ = middleware.QuotaReserve(r.Context(), h.DB, userID, -refund)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"purged":        purged,
		"refundedBytes": refund,
	})
}

// purgeOne hard-deletes one trashed object: removes the row (cascading into
// object_versions + transcode_jobs), wipes the on-disk hash dir, segments
// dir, and returns the bytes freed for quota accounting.
func (h *TrashHandler) purgeOne(r *http.Request, userID uuid.UUID, bucket, key string) (int64, error) {
	var bucketID, objectID uuid.UUID
	var totalSize int64
	var shardPath string

	// Get bucket + object + sum of (current size + all version sizes) + shard.
	err := h.DB.QueryRow(r.Context(), `
		SELECT b.id, o.id,
		       o.size_bytes
		       + COALESCE(o.transcoded_bytes, 0)
		       + COALESCE(o.transcode_reserved_bytes, 0)
		       + COALESCE((SELECT SUM(size_bytes)
		                     FROM object_versions WHERE object_id = o.id), 0),
		       COALESCE(o.shard_path, '')
		  FROM objects o
		  JOIN buckets b ON b.id = o.bucket_id
		 WHERE b.owner_id = $1 AND b.name = $2 AND o.key = $3
		   AND o.is_deleted = TRUE`,
		userID, bucket, key,
	).Scan(&bucketID, &objectID, &totalSize, &shardPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("not in trash")
	}
	if err != nil {
		return 0, err
	}

	// Hard-delete the row; CASCADE removes object_versions + transcode_jobs.
	if _, err := h.DB.Exec(r.Context(),
		`DELETE FROM objects WHERE id = $1`, objectID,
	); err != nil {
		return 0, err
	}

	// Shard index — drop this hash dir from its leaf's count.
	_ = sharding.OnObjectRemoved(r.Context(), h.DB, bucketID, shardPath)

	// Cancel any worker mid-transcode on this object before we wipe its files.
	cache.PublishCancelObject(r.Context(), h.RDB, objectID, "trash purged")

	// Wipe on-disk artifacts. RemoveObject blows away the hash dir which
	// includes versions/. SegmentsDir is the transcoder output tree.
	_ = h.FS.RemoveObject(bucketID.String(), key)
	_ = os.RemoveAll(h.FS.SegmentsDir(objectID.String()))

	return totalSize, nil
}

// decodeTrashItems pulls {items:[{bucket,key},...]} out of the request body.
// Empty list returns an error so callers don't accidentally no-op.
func decodeTrashItems(r *http.Request) ([]trashItem, error) {
	var body struct {
		Items []trashItem `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Items) == 0 {
		return nil, errors.New("items array is required and non-empty (or use ?all=1 to empty everything)")
	}
	if len(body.Items) > 1000 {
		return nil, errors.New("max 1000 items per request")
	}
	return body.Items, nil
}
