-- =============================================================================
-- Cleaner service — periodic garbage collection.
--
-- cleanup_runs holds one row per tick; the per-event audit trail lives in
-- NDJSON files under /storage/.cleanup/ to avoid hot-row contention.
--
-- transcode_jobs.pid lets the cleaner / cancel-pubsub identify the actual
-- ffmpeg process that's working a job. Set by the worker when it starts
-- ffmpeg, cleared on completion.
-- =============================================================================

CREATE TABLE IF NOT EXISTS cleanup_runs (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at     TIMESTAMPTZ,
  dry_run         BOOLEAN NOT NULL DEFAULT false,
  reaped_counts   JSONB NOT NULL DEFAULT '{}',
  bytes_freed     BIGINT NOT NULL DEFAULT 0,
  log_path        TEXT NOT NULL DEFAULT '',
  errors          JSONB NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS idx_cleanup_runs_started
  ON cleanup_runs(started_at DESC);

ALTER TABLE transcode_jobs
  ADD COLUMN IF NOT EXISTS pid INT;

CREATE INDEX IF NOT EXISTS idx_transcode_jobs_pid
  ON transcode_jobs(pid)
  WHERE pid IS NOT NULL;
