-- Per-job progress 0-100. Worker pipes ffmpeg's -progress output, parses
-- out_time_us, divides by duration, writes here every ~1s.
-- Dashboard polls and renders progress bars.

ALTER TABLE transcode_jobs
  ADD COLUMN IF NOT EXISTS progress_pct INT NOT NULL DEFAULT 0
    CHECK (progress_pct BETWEEN 0 AND 100);
