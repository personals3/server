-- =============================================================================
-- Smart transcoding controls per bucket
--
-- Modes:
--   'none'        — never auto-transcode (default for backups / general storage)
--   'media'       — auto-transcode video, audio, AND images (full streaming setup)
--   'photos_only' — only image thumbnails; never video/audio (saves CPU on
--                   photo libraries that don't need video streaming)
-- =============================================================================

ALTER TABLE buckets
  ADD COLUMN IF NOT EXISTS auto_transcode_mode TEXT
  NOT NULL DEFAULT 'none'
  CHECK (auto_transcode_mode IN ('none', 'media', 'photos_only'));

-- Existing buckets keep 'none'. New buckets default 'none' too (safe).
-- Web dashboard creation form will pre-select 'media' for the user.
