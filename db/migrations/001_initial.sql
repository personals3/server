-- =============================================================================
-- PersonalS3 - Initial Schema (Part 1)
-- Creates: users, buckets, objects, multipart_uploads, transcode_jobs, audit_log
-- =============================================================================

-- pgcrypto provides gen_random_uuid() and crypt() (bcrypt-compatible hashing)
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- -----------------------------------------------------------------------------
-- USERS
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email           TEXT UNIQUE NOT NULL,
  name            TEXT NOT NULL,
  password_hash   TEXT,                                       -- nullable: API-only users
  quota_bytes     BIGINT NOT NULL DEFAULT 10737418240,        -- 10 GB default
  used_bytes      BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  role            TEXT NOT NULL DEFAULT 'user'
                    CHECK (role IN ('admin', 'user')),
  is_active       BOOLEAN NOT NULL DEFAULT true,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- API keys (a user can have multiple)
CREATE TABLE IF NOT EXISTS api_keys (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key_prefix      TEXT NOT NULL,                              -- first 8 chars, for display
  key_hash        TEXT NOT NULL UNIQUE,                       -- bcrypt of full key
  name            TEXT,                                       -- user-given label
  last_used_at    TIMESTAMPTZ,
  expires_at      TIMESTAMPTZ,                                -- nullable: never expire
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_api_keys_user ON api_keys(user_id);
CREATE INDEX idx_api_keys_prefix ON api_keys(key_prefix);

-- -----------------------------------------------------------------------------
-- BUCKETS
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS buckets (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name            TEXT NOT NULL,
  owner_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  is_public       BOOLEAN NOT NULL DEFAULT false,
  versioning      BOOLEAN NOT NULL DEFAULT false,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(owner_id, name),
  CHECK (length(name) BETWEEN 3 AND 63),
  CHECK (name ~ '^[a-z0-9][a-z0-9.-]*[a-z0-9]$')              -- S3 naming rules
);

CREATE INDEX idx_buckets_owner ON buckets(owner_id);

-- -----------------------------------------------------------------------------
-- OBJECTS
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS objects (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  bucket_id          UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  key                TEXT NOT NULL,
  version_id         TEXT,                                    -- for future versioning
  size_bytes         BIGINT NOT NULL CHECK (size_bytes >= 0),
  etag               TEXT NOT NULL,                           -- MD5 or MD5-of-MD5s-{N}
  content_type       TEXT NOT NULL DEFAULT 'application/octet-stream',
  storage_path       TEXT NOT NULL,                           -- absolute path on disk
  metadata           JSONB NOT NULL DEFAULT '{}',             -- user-defined headers
  -- Transcoding outputs (Part 5): { "hls": "path/master.m3u8", "thumbnails": [...] }
  transcoded         JSONB NOT NULL DEFAULT '{}',
  transcode_status   TEXT NOT NULL DEFAULT 'none'
                       CHECK (transcode_status IN ('none', 'pending', 'processing', 'done', 'failed')),
  is_deleted         BOOLEAN NOT NULL DEFAULT false,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(bucket_id, key)
);

CREATE INDEX idx_objects_bucket_key ON objects(bucket_id, key);
-- text_pattern_ops enables fast prefix scans (ListObjectsV2 with ?prefix=)
CREATE INDEX idx_objects_key_prefix ON objects(bucket_id, key text_pattern_ops);
CREATE INDEX idx_objects_transcode_status ON objects(transcode_status)
  WHERE transcode_status IN ('pending', 'processing');

-- -----------------------------------------------------------------------------
-- MULTIPART UPLOADS (in-progress state)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS multipart_uploads (
  upload_id       TEXT PRIMARY KEY,                          -- random UUID
  bucket_id       UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  key             TEXT NOT NULL,
  owner_id        UUID NOT NULL REFERENCES users(id),
  content_type    TEXT NOT NULL DEFAULT 'application/octet-stream',
  -- parts: array of { partNumber, etag, size, path } as JSON
  parts           JSONB NOT NULL DEFAULT '[]',
  total_size      BIGINT NOT NULL DEFAULT 0,                  -- running total
  status          TEXT NOT NULL DEFAULT 'in-progress'
                    CHECK (status IN ('in-progress', 'complete', 'aborted')),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at      TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '7 days')
);

CREATE INDEX idx_multipart_owner ON multipart_uploads(owner_id);
CREATE INDEX idx_multipart_expires ON multipart_uploads(expires_at)
  WHERE status = 'in-progress';

-- -----------------------------------------------------------------------------
-- TRANSCODE JOBS (queue table; workers poll with FOR UPDATE SKIP LOCKED)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS transcode_jobs (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  object_id       UUID NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
  input_path      TEXT NOT NULL,
  output_dir      TEXT NOT NULL,
  file_type       TEXT NOT NULL CHECK (file_type IN ('video', 'audio', 'image')),
  status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'processing', 'done', 'failed')),
  attempts        INT NOT NULL DEFAULT 0,
  max_attempts    INT NOT NULL DEFAULT 3,
  priority        INT NOT NULL DEFAULT 5,                     -- 1=highest, 10=lowest
  worker_id       TEXT,                                       -- which worker claimed it
  error           TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at      TIMESTAMPTZ,
  done_at         TIMESTAMPTZ
);

-- Index for the worker poll query: pending jobs by priority + created_at
CREATE INDEX idx_transcode_jobs_pending
  ON transcode_jobs(priority, created_at)
  WHERE status = 'pending';

CREATE INDEX idx_transcode_jobs_object ON transcode_jobs(object_id);

-- -----------------------------------------------------------------------------
-- AUDIT LOG (immutable, append-only)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_log (
  id              BIGSERIAL PRIMARY KEY,
  user_id         UUID REFERENCES users(id) ON DELETE SET NULL,
  action          TEXT NOT NULL,                              -- 'PUT_OBJECT', 'GET_OBJECT', etc.
  bucket_name     TEXT,
  object_key      TEXT,
  size_bytes      BIGINT,
  status_code     INT,
  ip_address      INET,
  user_agent      TEXT,
  details         JSONB NOT NULL DEFAULT '{}',
  ts              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_user_ts ON audit_log(user_id, ts DESC);
CREATE INDEX idx_audit_action_ts ON audit_log(action, ts DESC);
CREATE INDEX idx_audit_ts ON audit_log(ts DESC);

-- -----------------------------------------------------------------------------
-- TRIGGERS — keep updated_at in sync
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION touch_updated_at() RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_users_updated
  BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TRIGGER trg_objects_updated
  BEFORE UPDATE ON objects
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
