-- =============================================================================
-- Object versioning — keeps prior versions when an object is overwritten or
-- deleted in a bucket where buckets.versioning = TRUE.
--
-- Layout:
--   objects(...)            ← always the CURRENT version (or a delete marker
--                              if is_deleted=TRUE with versioning on).
--   object_versions(...)    ← every prior version, oldest at the top.
--
-- Storage on disk: prior versions live next to the current data file, at
--   buckets/{bucketId}/objects/{sha256(key)}/versions/{versionId}
-- =============================================================================

CREATE TABLE IF NOT EXISTS object_versions (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  object_id         UUID NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
  version_id        TEXT NOT NULL,                  -- opaque token (UUID hex)
  size_bytes        BIGINT NOT NULL CHECK (size_bytes >= 0),
  etag              TEXT NOT NULL,
  content_type      TEXT NOT NULL DEFAULT 'application/octet-stream',
  storage_path      TEXT NOT NULL,                  -- absolute path of snapshotted file
  metadata          JSONB NOT NULL DEFAULT '{}',
  -- Set when this row represents the "delete" event itself (no file on disk).
  -- Used so we can show "deleted at X" in version history.
  is_delete_marker  BOOLEAN NOT NULL DEFAULT false,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(object_id, version_id)
);

-- Listing previous versions, newest first
CREATE INDEX IF NOT EXISTS idx_object_versions_object_created
  ON object_versions(object_id, created_at DESC);
