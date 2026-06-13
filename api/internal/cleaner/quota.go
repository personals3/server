package cleaner

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Quota self-heal: recompute users.used_bytes from what actually exists and
// repair rows that drifted. Replaces "run scripts/quota-reconcile.sql
// weekly" with an automatic pass — users never see drift, admins see every
// correction in the cleanup audit log.
//
// What counts (must mirror QuotaReserve's accounting):
//   - objects (live AND trashed): size + transcoded + transcode reservation
//   - prior versions (object_versions)
//   - parts of in-progress multipart uploads — THE term the manual script
//     lacks, which is what makes this safe to run mid-upload
//
// Concurrency safety, in plain language: one SQL statement does everything.
// The CTE snapshots each user's recorded used_bytes next to the recomputed
// total; the UPDATE then requires `used_bytes = recorded` — and Postgres
// re-evaluates that predicate against the row's latest committed version
// when it takes the row lock. If an upload charged the account between
// snapshot and lock, the predicate fails and that user is simply skipped
// until the next cycle. A concurrent charge can never be overwritten.
const reconcileActualCTE = `
	WITH actual AS (
		SELECT u.id,
		       u.used_bytes AS recorded,
		       COALESCE(o.total, 0) + COALESCE(v.total, 0) + COALESCE(m.total, 0) AS total
		  FROM users u
		  LEFT JOIN (
			SELECT b.owner_id,
			       SUM(o.size_bytes + COALESCE(o.transcoded_bytes, 0)
			           + COALESCE(o.transcode_reserved_bytes, 0))::bigint AS total
			  FROM objects o JOIN buckets b ON b.id = o.bucket_id
			 GROUP BY b.owner_id) o ON o.owner_id = u.id
		  LEFT JOIN (
			SELECT b.owner_id, SUM(ov.size_bytes)::bigint AS total
			  FROM object_versions ov
			  JOIN objects ob ON ob.id = ov.object_id
			  JOIN buckets b ON b.id = ob.bucket_id
			 GROUP BY b.owner_id) v ON v.owner_id = u.id
		  LEFT JOIN (
			SELECT owner_id, SUM(total_size)::bigint AS total
			  FROM multipart_uploads
			 WHERE status = 'in-progress'
			 GROUP BY owner_id) m ON m.owner_id = u.id
	)`

const reconcileUpdateSQL = reconcileActualCTE + `
	UPDATE users u
	   SET used_bytes = a.total, updated_at = now()
	  FROM actual a
	 WHERE u.id = a.id
	   AND u.used_bytes = a.recorded
	   AND abs(a.recorded - a.total) > $1
	RETURNING u.id, a.recorded, a.total`

const reconcileSelectSQL = reconcileActualCTE + `
	SELECT id, recorded, total FROM actual
	 WHERE abs(recorded - total) > $1`

func (c *Cleaner) reconcileQuotas(ctx context.Context, rec *runRecord) error {
	// Dry-run: report drifted users, change nothing.
	query := reconcileUpdateSQL
	if c.cfg.DryRun {
		query = reconcileSelectSQL
	}

	rows, err := c.db.Query(ctx, query, c.cfg.QuotaDriftThreshold)
	if err != nil {
		return fmt.Errorf("quota reconcile: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uuid.UUID
		var recorded, actual int64
		if err := rows.Scan(&id, &recorded, &actual); err != nil {
			return fmt.Errorf("quota reconcile scan: %w", err)
		}
		rec.audit("quota_reconcile", "user", id.String(),
			fmt.Sprintf("used_bytes %d → %d (drift %+d)",
				recorded, actual, recorded-actual), 0)
	}
	return rows.Err()
}
