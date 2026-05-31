-- =============================================================================
-- V4 cleaner — adaptive Merkle-trie shard index
--
-- See docs/come-up-designs/cleaner.md for the full design rationale.
--
-- Each bucket owns a tree of shards keyed by hex-nibble prefixes of
-- sha256(key). Leaves hold actual objects; internal nodes (is_leaf=FALSE)
-- exist only to make longest-prefix lookups easy.
--
-- Address space:
--   depth=0 → ""                  → 1 root per bucket
--   depth=1 → "0".."f"            → 16 shards
--   depth=2 → "00".."ff"          → 256 shards
--   ...
--   depth=16 → 16 nibbles         → 1.8e19 shards  (effectively IPv6 territory)
--
-- The trie expands on demand: when a leaf exceeds SHARD_SPLIT_THRESHOLD,
-- the cleaner promotes it to internal and creates 16 child leaves.
-- =============================================================================

CREATE TABLE IF NOT EXISTS object_shard_index (
  bucket_id     UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  -- "" for root, "a" / "ab" / "abc" ... up to 16 hex chars (= 64-bit address)
  shard_path    TEXT NOT NULL,
  depth         INT  NOT NULL,
  is_leaf       BOOLEAN NOT NULL DEFAULT TRUE,
  -- Commutative running hash. Each PUT does:
  --   state_hash := digest(state_hash || object_id::text::bytea, 'sha256')
  -- but in XOR-style so order doesn't matter (see api/internal/sharding).
  state_hash    BYTEA NOT NULL DEFAULT '\x00'::bytea,
  object_count  INT  NOT NULL DEFAULT 0,
  last_walk_at  TIMESTAMPTZ,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (bucket_id, shard_path),
  CHECK (depth >= 0 AND depth <= 16),
  CHECK (length(shard_path) = depth),
  CHECK (shard_path ~ '^[0-9a-f]*$')  -- valid hex (or empty)
);

-- Dirty-shard select: heart of the cleaner's per-tick query.
-- Partial index keeps it tiny even with millions of shard rows.
CREATE INDEX IF NOT EXISTS idx_shard_dirty
  ON object_shard_index(bucket_id, shard_path)
  WHERE is_leaf = TRUE
    AND (last_walk_at IS NULL OR last_walk_at < updated_at);

-- Split candidates: leaves that have outgrown their threshold.
-- Cleaner drains this set every tick.
CREATE INDEX IF NOT EXISTS idx_shard_split_candidates
  ON object_shard_index(bucket_id, object_count DESC)
  WHERE is_leaf = TRUE AND object_count > 5000;

-- Prefix lookup index used by AssignToLeaf — "given this hash, find its leaf".
-- shard_path text_pattern_ops enables fast LIKE 'prefix%' scans for the
-- longest-matching-prefix walk.
CREATE INDEX IF NOT EXISTS idx_shard_lookup
  ON object_shard_index(bucket_id, shard_path text_pattern_ops)
  WHERE is_leaf = TRUE;

-- Per-object shard assignment. Set at INSERT time, kept in sync if the
-- containing shard splits. Index lets the cleaner enumerate one shard's
-- objects in O(log N).
ALTER TABLE objects
  ADD COLUMN IF NOT EXISTS shard_path TEXT;

CREATE INDEX IF NOT EXISTS idx_objects_shard
  ON objects(bucket_id, shard_path)
  WHERE NOT is_deleted;

-- Trigger: keep object_shard_index.updated_at fresh on any change. Used by
-- the cleaner's dirty-shard query.
CREATE OR REPLACE FUNCTION touch_shard_index_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_shard_index_updated ON object_shard_index;
CREATE TRIGGER trg_shard_index_updated
  BEFORE UPDATE ON object_shard_index
  FOR EACH ROW
  EXECUTE FUNCTION touch_shard_index_updated_at();
