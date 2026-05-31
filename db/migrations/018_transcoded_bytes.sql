-- =============================================================================
-- Track transcoded-segment disk usage per object.
--
-- HLS variants + thumbnails for a video can easily 3-5x the original's size
-- (5 quality rungs × thumbnail JPEGs). Previously these bytes were on disk
-- but didn't count toward the user's used_bytes — surprising and wrong.
--
-- objects.transcoded_bytes is updated at the publish point (worker's
-- video_finalize step, OR a single-job audio/image complete) and refunded
-- on DeleteTranscodes / PurgeObject / trash vacuum.
-- =============================================================================

ALTER TABLE objects
  ADD COLUMN IF NOT EXISTS transcoded_bytes BIGINT NOT NULL DEFAULT 0
  CHECK (transcoded_bytes >= 0);
