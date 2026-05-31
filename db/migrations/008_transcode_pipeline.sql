-- =============================================================================
-- Split monolithic transcode jobs into parallel sub-jobs.
--
-- Before: one row per video, one worker churns through 4 qualities serially.
-- After:  one row per quality + 1 thumbnails + 1 finalize. Workers pick
--         independent qualities in parallel.
--
-- Backwards-compatible: old "video"/"audio"/"image" file_types still work
-- via the worker's legacy path. New API code emits the split form for video.
-- =============================================================================

ALTER TABLE transcode_jobs
  ADD COLUMN IF NOT EXISTS group_id UUID,
  ADD COLUMN IF NOT EXISTS params   JSONB NOT NULL DEFAULT '{}';

-- file_type now holds richer values: 'video_quality_1080p', 'video_thumbnails',
-- 'video_finalize'. Drop the old CHECK so they validate.
ALTER TABLE transcode_jobs DROP CONSTRAINT IF EXISTS transcode_jobs_file_type_check;

-- status gets two new values:
--   'waiting' — depends on sibling jobs; promoted to 'pending' once they finish
--   'skipped' — quality wasn't applicable (e.g. don't upscale a 720p video to 1080p)
ALTER TABLE transcode_jobs DROP CONSTRAINT IF EXISTS transcode_jobs_status_check;
ALTER TABLE transcode_jobs ADD CONSTRAINT transcode_jobs_status_check
  CHECK (status IN ('pending','processing','done','failed','waiting','skipped'));

-- Lookups + ordering by group when promoting waiting → pending.
CREATE INDEX IF NOT EXISTS idx_transcode_jobs_group   ON transcode_jobs(group_id);

-- Index used by the worker's "any active siblings left?" check.
CREATE INDEX IF NOT EXISTS idx_transcode_jobs_group_active
  ON transcode_jobs(group_id)
  WHERE status IN ('pending', 'processing');
