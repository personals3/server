-- =============================================================================
-- Trash bin — when an object is DELETE'd, it now moves to a per-user trash
-- instead of being purged immediately.
--
-- objects.is_deleted already exists; this migration adds:
--   - deleted_at : when the move-to-trash happened
--   - an index so the trash listing query stays fast on big buckets
--
-- The bytes stay on disk while in trash; the user can either Restore (flip
-- is_deleted back to false) or Purge (hard delete + quota refund). A future
-- background vacuum can sweep entries older than N days.
-- =============================================================================

ALTER TABLE objects
  ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- Backfill: any object already marked is_deleted=true (delete markers from
-- the versioning migration) gets a deleted_at of updated_at as a best guess.
UPDATE objects
   SET deleted_at = updated_at
 WHERE is_deleted = true AND deleted_at IS NULL;

-- Trash listing: filter by is_deleted, order by deleted_at desc
CREATE INDEX IF NOT EXISTS idx_objects_trash
  ON objects(deleted_at DESC)
  WHERE is_deleted = true;
