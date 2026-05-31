-- Two new terminal states for transcode_status:
--
--   skipped_quota — pre-flight estimated the transcode would exceed the
--                   user's remaining quota; jobs never enqueued. Original
--                   object kept intact, no segments produced.
--
--   failed_quota  — pre-flight passed but actual segment size at publish
--                   point would exceed the quota. Segments reaped from disk,
--                   original kept intact, transcoded_bytes stays 0.
--
-- Both states are end states (no auto-retry). User can manually re-trigger
-- a transcode after freeing space.

ALTER TABLE objects DROP CONSTRAINT IF EXISTS objects_transcode_status_check;
ALTER TABLE objects ADD CONSTRAINT objects_transcode_status_check
  CHECK (transcode_status IN ('none', 'pending', 'processing', 'done',
                              'failed', 'skipped_quota', 'failed_quota'));
