-- =============================================================================
-- Per-user transcode limits — duration + max rung height.
--
-- Why: Transcoding is the most expensive thing the worker does. A 4K 2-hour
-- upload can pin a worker for hours. We want sensible defaults plus per-user
-- overrides so an admin can grant exceptions (e.g. a friend uploading a
-- wedding video) without raising the limit for everyone.
--
-- Semantics:
--   * Uploads are NEVER blocked. The object is stored verbatim.
--   * Transcoding is gated:
--       - If source duration > effective limit → pipeline skipped entirely
--         (transcode_status = 'skipped_duration_limit'). Object is still
--         downloadable; just no HLS variants.
--       - For acceptable duration: only rungs whose height ≤ effective
--         height limit are produced. Higher rungs silently dropped.
--   * "Effective limit" = COALESCE(users.max_*, system_config.default_*).
--     NULL on the user means "use system default", not "unlimited".
--
-- Enforcement happens API-side in insertVideoPipeline() at job-enqueue time.
-- =============================================================================

INSERT INTO system_config (key, value, description) VALUES
  ('default_max_transcode_duration_seconds', '1800',
    'Default cap on video duration (seconds) for the transcoding pipeline. Sources longer than this are stored but not transcoded into HLS. Per-user override via users.max_transcode_duration_seconds. Set to 0 for unlimited.'),
  ('default_max_transcode_height', '1080',
    'Default cap on HLS rung height (pixels). Rungs above this are silently dropped from the ladder; the object is still streamable at lower rungs. Per-user override via users.max_transcode_height. Set to 0 for unlimited.')
ON CONFLICT (key) DO NOTHING;

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS max_transcode_duration_seconds INTEGER,
  ADD COLUMN IF NOT EXISTS max_transcode_height INTEGER;

COMMENT ON COLUMN users.max_transcode_duration_seconds IS
  'Per-user override for max transcode source duration in seconds. NULL = use system_config.default_max_transcode_duration_seconds. 0 = unlimited (use sparingly).';

COMMENT ON COLUMN users.max_transcode_height IS
  'Per-user override for max HLS rung height in pixels. NULL = use system_config.default_max_transcode_height. 0 = unlimited.';
