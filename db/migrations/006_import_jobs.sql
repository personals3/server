-- =============================================================================
-- Async URL imports
--
-- Same queue-table pattern as transcode_jobs. Importer goroutines in the API
-- poll with FOR UPDATE SKIP LOCKED, run the download, write progress to this
-- table every ~1s so the dashboard can show a live progress bar.
-- =============================================================================

CREATE TABLE IF NOT EXISTS import_jobs (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  bucket_id       UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  key             TEXT NOT NULL,
  source_url      TEXT NOT NULL,
  -- The Authorization header value, if any. Stored as-is so the importer
  -- can re-send it. Same sensitivity trade-off as s3_credentials.secret_key —
  -- only the owner can read these via /admin/* or via the import-list endpoint
  -- (which never returns it).
  auth_header     TEXT,
  status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','running','done','failed','cancelled')),
  bytes_done      BIGINT NOT NULL DEFAULT 0 CHECK (bytes_done >= 0),
  total_bytes     BIGINT,                              -- NULL = unknown (no Content-Length)
  throughput_bps  BIGINT NOT NULL DEFAULT 0,
  worker_id       TEXT,
  error           TEXT,
  -- Track the object_id created on success so the UI can link to it
  object_id       UUID,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at      TIMESTAMPTZ,
  done_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_import_jobs_pending
  ON import_jobs(created_at)
  WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_import_jobs_user_recent
  ON import_jobs(user_id, created_at DESC);
