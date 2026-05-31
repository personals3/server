-- =============================================================================
-- Bucket archival — opt-out flag for ancient / cold-storage buckets.
--
-- Archived buckets:
--   - Are excluded from cleaner shard sweeps (no walk overhead)
--   - Are excluded from V2 bloom scans
--   - Still serve reads + writes normally — archival is purely a
--     "stop spending cleanup cycles on this bucket" hint
--
-- Useful when you have a few buckets that hold years of cold data and
-- millions of files, and you don't want the cleaner re-walking them on
-- every dirty-flag bump.
-- =============================================================================

ALTER TABLE buckets
  ADD COLUMN IF NOT EXISTS archived BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS idx_buckets_archived
  ON buckets(archived)
  WHERE archived = true;
