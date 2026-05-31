-- =============================================================================
-- Cross-bucket search — fast substring + multi-filter queries against objects.
--
-- We use PostgreSQL's pg_trgm extension to make ILIKE '%foo%' on object keys
-- index-backed instead of a full table scan. Without this, a single user
-- with 100K objects would see ~1s search latency; with it, the same query
-- returns in <50ms.
--
-- pg_trgm is in contrib (shipped with Postgres), no external dependency.
-- =============================================================================

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- GIN index over the key column using trigram ops. Enables fast
--   WHERE key ILIKE '%substring%'
-- queries on the active (not-deleted) subset.
CREATE INDEX IF NOT EXISTS idx_objects_key_trgm
  ON objects USING gin (key gin_trgm_ops)
  WHERE NOT is_deleted;

-- Range filters on size / time benefit from a btree composite. The dashboard
-- search frequently sorts by lastModified DESC, so put updated_at first.
CREATE INDEX IF NOT EXISTS idx_objects_updated
  ON objects(updated_at DESC, bucket_id)
  WHERE NOT is_deleted;

CREATE INDEX IF NOT EXISTS idx_objects_size
  ON objects(size_bytes)
  WHERE NOT is_deleted;

-- Content-type filter is usually loose (image/* vs video/*), so a btree on
-- the type substring would help only with normalization. Skipping for now —
-- the trgm index + bucket filter already narrows enough.
