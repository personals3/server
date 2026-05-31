-- Pre-flight quota reservation for transcodes.
--
-- Holds the estimated HLS output bytes the API has already reserved against
-- the user's used_bytes — before the worker actually produces any segments.
--
-- Lifecycle:
--   pre-flight  →  QuotaReserve(+estimate); UPDATE objects SET
--                  transcode_reserved_bytes = estimate, transcode_status='pending'
--   publish OK  →  delta = actual - reserved; if delta>0 reserve more (or fail),
--                  if delta<0 refund. Set transcode_reserved_bytes=0,
--                  transcoded_bytes = actual, transcode_status='done'
--   publish FAIL→ shutil.rmtree(segments); refund reserved; set status='failed_quota'
--   re-trigger  →  refund old reserved before pre-flight again
--
-- This closes the TOCTOU race where two concurrent uploads both pass
-- pre-flight against the same pre-reservation snapshot of used_bytes.

ALTER TABLE objects
  ADD COLUMN IF NOT EXISTS transcode_reserved_bytes BIGINT NOT NULL DEFAULT 0
    CHECK (transcode_reserved_bytes >= 0);
