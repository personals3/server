package cleaner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// ---------- TRASH VACUUM ---------------------------------------------------

// reapTrash hard-deletes objects whose deleted_at is older than
// cfg.TrashRetention. CASCADE removes object_versions + transcode_jobs.
// Each victim's hash-dir + segments dir is wiped from disk; quota is
// refunded to the owner.
//
// In dry-run mode this is purely a SELECT — nothing is deleted, but every
// would-be victim is logged.
func (c *Cleaner) reapTrash(ctx context.Context, rec *runRecord) error {
	type victim struct {
		ID         uuid.UUID
		BucketID   uuid.UUID
		Key        string
		OwnerID    uuid.UUID
		TotalBytes int64
	}

	// Pick candidates (no DELETE yet).
	//
	// total_bytes MUST include transcoded HLS segments and any pre-flight
	// reservation that's still parked on the row. Otherwise reaping a
	// transcoded video leaks transcoded_bytes back into the user's quota
	// counter (the row is gone, the disk is freed, but used_bytes still
	// counts the segments). This is the canonical "bucket empty but
	// used_bytes high" drift source.
	rows, err := c.db.Query(ctx, `
		SELECT o.id, o.bucket_id, o.key, b.owner_id,
		       o.size_bytes
		       + COALESCE(o.transcoded_bytes, 0)
		       + COALESCE(o.transcode_reserved_bytes, 0)
		       + COALESCE((SELECT SUM(size_bytes)
		                     FROM object_versions WHERE object_id = o.id), 0) AS total_bytes
		  FROM objects o
		  JOIN buckets b ON b.id = o.bucket_id
		 WHERE o.is_deleted = TRUE
		   AND o.deleted_at IS NOT NULL
		   AND o.deleted_at < now() - ($1::int * INTERVAL '1 second')
		 ORDER BY o.deleted_at
		 LIMIT $2`,
		int(c.cfg.TrashRetention.Seconds()), c.cfg.MaxReapsPerTick)
	if err != nil {
		return fmt.Errorf("select trash: %w", err)
	}
	defer rows.Close()

	victims := []victim{}
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.ID, &v.BucketID, &v.Key, &v.OwnerID, &v.TotalBytes); err != nil {
			return fmt.Errorf("scan victim: %w", err)
		}
		victims = append(victims, v)
	}

	// Always log; only DELETE if !dryRun.
	for _, v := range victims {
		rec.audit("trash_vacuum", "object", fmt.Sprintf("%s/%s", v.BucketID, v.Key),
			fmt.Sprintf("deleted_at > %s ago", c.cfg.TrashRetention), v.TotalBytes)
	}
	if c.cfg.DryRun {
		return nil
	}

	for _, v := range victims {
		if _, err := c.db.Exec(ctx, `DELETE FROM objects WHERE id = $1`, v.ID); err != nil {
			rec.Errors = append(rec.Errors,
				fmt.Sprintf("trash delete %s: %v", v.ID, err))
			continue
		}
		_ = c.fs.RemoveObject(v.BucketID.String(), v.Key) // wipes hash dir + versions/
		_ = os.RemoveAll(c.fs.SegmentsDir(v.ID.String()))
	}

	// Refund quota per owner.
	refunds := map[uuid.UUID]int64{}
	for _, v := range victims {
		refunds[v.OwnerID] += v.TotalBytes
	}
	for ownerID, bytes := range refunds {
		if bytes <= 0 {
			continue
		}
		_, _ = c.db.Exec(ctx, `
			UPDATE users SET used_bytes = GREATEST(used_bytes - $1, 0)
			 WHERE id = $2`, bytes, ownerID)
	}

	return nil
}

// ---------- ABANDONED MULTIPART -------------------------------------------

// reapMultipart wipes multipart_uploads rows past expires_at and their tmp
// directories. Refunds any quota that was reserved during in-progress parts.
//
// (Currently the API doesn't pre-reserve quota for parts — only on Complete —
// so refund is a no-op for now. Logged anyway for visibility.)
func (c *Cleaner) reapMultipart(ctx context.Context, rec *runRecord) error {
	type victim struct {
		UploadID string
		BucketID uuid.UUID
	}
	rows, err := c.db.Query(ctx, `
		SELECT upload_id, bucket_id
		  FROM multipart_uploads
		 WHERE status = 'in-progress'
		   AND expires_at < now()
		 ORDER BY expires_at
		 LIMIT $1`, c.cfg.MaxReapsPerTick)
	if err != nil {
		return fmt.Errorf("select expired multipart: %w", err)
	}
	defer rows.Close()

	victims := []victim{}
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.UploadID, &v.BucketID); err != nil {
			return err
		}
		victims = append(victims, v)
	}

	for _, v := range victims {
		rec.audit("multipart_sweep", "multipart",
			fmt.Sprintf("%s/tmp/%s", v.BucketID, v.UploadID),
			"expires_at < now()", 0)
	}
	if c.cfg.DryRun {
		return nil
	}
	for _, v := range victims {
		_ = c.fs.RemoveAbandonedUpload(v.BucketID.String(), v.UploadID)
		_, _ = c.db.Exec(ctx,
			`DELETE FROM multipart_uploads WHERE upload_id = $1`, v.UploadID)
	}
	return nil
}

// ---------- STUCK TRANSCODES ----------------------------------------------

// reapStuckTranscodes resets jobs that have been 'processing' for longer than
// cfg.StuckTranscodeAge with no progress_pct update. The worker that holds
// them is presumed dead; the row goes back to 'pending' so another worker
// can pick it up. attempts is bumped so a truly poisonous job eventually
// fails out via max_attempts.
func (c *Cleaner) reapStuckTranscodes(ctx context.Context, rec *runRecord) error {
	type victim struct {
		ID       uuid.UUID
		ObjectID uuid.UUID
		FileType string
	}

	// Always SELECT first so dry-run can audit without mutating.
	rows, err := c.db.Query(ctx, `
		SELECT id, object_id, file_type
		  FROM transcode_jobs
		 WHERE status = 'processing'
		   AND started_at < now() - ($1::int * INTERVAL '1 second')`,
		int(c.cfg.StuckTranscodeAge.Seconds()))
	if err != nil {
		return fmt.Errorf("select stuck transcodes: %w", err)
	}
	defer rows.Close()
	victims := []victim{}
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.ID, &v.ObjectID, &v.FileType); err != nil {
			return err
		}
		victims = append(victims, v)
	}

	for _, v := range victims {
		rec.audit("stuck_transcode", "transcode_job", v.ID.String(),
			fmt.Sprintf("processing > %s — reset to pending", c.cfg.StuckTranscodeAge), 0)
	}
	if c.cfg.DryRun {
		return nil
	}
	for _, v := range victims {
		_, _ = c.db.Exec(ctx, `
			UPDATE transcode_jobs
			   SET status = 'pending', started_at = NULL, pid = NULL,
			       worker_id = NULL, attempts = attempts + 1
			 WHERE id = $1`, v.ID)
	}
	return nil
}

// ---------- STALE URL IMPORTS ---------------------------------------------

// reapStuckImports fails URL-import jobs that have been processing too long.
// (The import_jobs table is created by migration 006; columns: id, status,
//  started_at, error, reserved_bytes etc.)
func (c *Cleaner) reapStuckImports(ctx context.Context, rec *runRecord) error {
	rows, err := c.db.Query(ctx, `
		SELECT id FROM import_jobs
		 WHERE status IN ('pending', 'processing')
		   AND COALESCE(started_at, created_at) < now() - ($1::int * INTERVAL '1 second')`,
		int(c.cfg.StuckImportAge.Seconds()))
	if err != nil {
		return fmt.Errorf("select stuck imports: %w", err)
	}
	defer rows.Close()
	victims := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		victims = append(victims, id)
	}

	for _, id := range victims {
		rec.audit("stuck_imports", "import_job", id.String(),
			fmt.Sprintf("stuck > %s — failing", c.cfg.StuckImportAge), 0)
	}
	if c.cfg.DryRun || len(victims) == 0 {
		return nil
	}
	_, _ = c.db.Exec(ctx, `
		UPDATE import_jobs
		   SET status = 'failed',
		       error = COALESCE(error, '') || E'\n[cleaner] reaped: stuck > '
		             || $1 || ' hours',
		       done_at = now()
		 WHERE id = ANY($2::uuid[])`,
		int(c.cfg.StuckImportAge.Hours()), victims)
	return nil
}

// ---------- helper used by orphan reaper -----------------------------------

// removePathSafely is used by the orphan scanner. Wraps os.RemoveAll with
// the dry-run guard.
func (c *Cleaner) removePathSafely(rec *runRecord, action, targetType, path, reason string) {
	stat, err := os.Stat(path)
	var bytes int64
	if err == nil && stat.IsDir() {
		bytes = dirSize(path)
	} else if err == nil {
		bytes = stat.Size()
	}
	if !c.cfg.DryRun {
		_ = os.RemoveAll(path)
	}
	rec.audit(action, targetType, path, reason, bytes)
}

// dirSize is a best-effort recursive size calculation. Used for audit log
// bytes_freed reporting. Failure → 0; we don't want a stat error to abort.
func dirSize(path string) int64 {
	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
