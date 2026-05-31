# Database Schema

PostgreSQL 16. All migrations live in `db/migrations/` and run automatically
on a fresh DB. Below is the logical ER, then each table in detail.

## ER diagram

```
                    ┌──────────┐
                    │  users   │◄──┐
                    └────┬─────┘   │
        ┌────────────────┼──────────┼──────────────┐
        │                │          │              │
        ▼                ▼          │              ▼
  ┌──────────┐    ┌──────────┐      │      ┌────────────────┐
  │ api_keys │    │s3_creden-│      │      │    buckets     │
  │          │    │  tials   │      │      │                │
  └──────────┘    └──────────┘      │      └───────┬────────┘
                                    │              │
                                    │              ▼
                                    │      ┌────────────────┐
                                    │      │    objects     │◄──┐
                                    │      └────────────────┘   │
                                    │                           │
                                    │      ┌────────────────┐   │
                                    └──────┤multipart_      │   │
                                           │  uploads       │   │
                                           └───────┬────────┘   │
                                                   │            │
                                                   ▼            │
                                           ┌────────────────┐   │
                                           │multipart_parts │   │
                                           └────────────────┘   │
                                                                │
                                           ┌────────────────┐   │
                                           │transcode_jobs  ├───┘
                                           └────────────────┘

                    ┌──────────────────────┐
                    │    audit_log         │   (no FK to users —
                    │   (append-only)      │    SET NULL on delete)
                    └──────────────────────┘

                    ┌──────────────────────┐
                    │   system_config      │   (no FKs — global k/v)
                    └──────────────────────┘
```

---

## `users`

The principal — anyone who can log in or upload via API.

```sql
CREATE TABLE users (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email           TEXT UNIQUE NOT NULL,
  name            TEXT NOT NULL,
  password_hash   TEXT,                       -- bcrypt; NULL = no password login
  quota_bytes     BIGINT NOT NULL DEFAULT 10737418240,    -- 10 GB
  used_bytes      BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
  role            TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin','user')),
  is_active       BOOLEAN NOT NULL DEFAULT true,
  created_at      TIMESTAMPTZ DEFAULT now(),
  updated_at      TIMESTAMPTZ DEFAULT now()
);
```

| Field | Notes |
|---|---|
| `quota_bytes` | Set by admin. Sum across all users should not exceed system effective total (unless overcommit allowed) |
| `used_bytes` | Maintained atomically by `QuotaReserve` — incremented on PUT, decremented on DELETE |
| `role` | `admin` sees the Admin sidebar and can call `/admin/*` endpoints |
| `is_active` | Soft delete. Inactive users can't log in or use API keys |

---

## `api_keys`

Bearer tokens for the native API (`Authorization: Bearer psk_...`).

```sql
CREATE TABLE api_keys (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key_prefix      TEXT NOT NULL,           -- first 8 chars of psk_xxxxxxxx
  key_hash        TEXT NOT NULL UNIQUE,    -- SHA-256 of full plaintext
  name            TEXT,
  last_used_at    TIMESTAMPTZ,
  expires_at      TIMESTAMPTZ,             -- NULL = never expires
  created_at      TIMESTAMPTZ DEFAULT now()
);
```

The plaintext key is shown ONCE on creation. The DB only stores SHA-256.
SHA-256 (not bcrypt) is fine because random 256-bit tokens are pre-image
resistant — there's nothing to brute-force.

---

## `s3_credentials`

AWS-format access keys for SigV4 clients (boto3, aws-cli, rclone).

```sql
CREATE TABLE s3_credentials (
  access_key_id   TEXT PRIMARY KEY,         -- "AKIA..." 20 chars
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  secret_key      TEXT NOT NULL,            -- stored in plaintext (see note)
  name            TEXT,
  last_used_at    TIMESTAMPTZ,
  created_at      TIMESTAMPTZ DEFAULT now()
);
```

**Why plaintext secret:** SigV4 requires HMAC'ing every request with the
original secret. We must regenerate the signing key on every request — a
one-way hash (bcrypt/SHA) would prevent that. This is the same trade-off
AWS makes internally.

---

## `buckets`

Per-user namespaces. The same name can be reused across different owners.

```sql
CREATE TABLE buckets (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name            TEXT NOT NULL,
  owner_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  is_public       BOOLEAN DEFAULT false,    -- future: public buckets
  versioning      BOOLEAN DEFAULT false,    -- future
  created_at      TIMESTAMPTZ DEFAULT now(),
  UNIQUE(owner_id, name),
  CHECK (length(name) BETWEEN 3 AND 63),
  CHECK (name ~ '^[a-z0-9][a-z0-9.-]*[a-z0-9]$')
);
```

The name regex matches S3's rules. Cascade delete: deleting a user wipes
their buckets (and via FK chain, all their objects).

---

## `objects`

Every uploaded file.

```sql
CREATE TABLE objects (
  id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  bucket_id                UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  key                      TEXT NOT NULL,         -- e.g. "videos/2024/holiday.mp4"
  version_id               TEXT,                  -- versioning support
  size_bytes               BIGINT NOT NULL,
  etag                     TEXT NOT NULL,         -- MD5 single / md5-of-md5s-N multipart
  content_type             TEXT NOT NULL DEFAULT 'application/octet-stream',
  storage_path             TEXT NOT NULL,         -- absolute path on disk
  metadata                 JSONB DEFAULT '{}',
  transcoded               JSONB DEFAULT '{}',    -- {master:"...", qualities:[...]}
                                                  -- OR {quota:{...}} for skipped/failed
  transcoded_bytes         BIGINT NOT NULL DEFAULT 0 CHECK (transcoded_bytes >= 0),
                                                  -- sum of /storage/segments/{id}/* on disk
  transcode_reserved_bytes BIGINT NOT NULL DEFAULT 0 CHECK (transcode_reserved_bytes >= 0),
                                                  -- pre-flight estimate, charged at enqueue,
                                                  -- settled at publish (see "Transcode quota")
  transcode_status         TEXT DEFAULT 'none'
                             CHECK (transcode_status IN (
                               'none','pending','processing','done','failed',
                               'skipped_quota','failed_quota')),
  is_deleted               BOOLEAN DEFAULT false,
  shard_path               TEXT,                  -- cleaner V4 adaptive shard pointer
  created_at               TIMESTAMPTZ DEFAULT now(),
  updated_at               TIMESTAMPTZ DEFAULT now(),
  UNIQUE(bucket_id, key)
);

CREATE INDEX idx_objects_bucket_key      ON objects(bucket_id, key);
CREATE INDEX idx_objects_key_prefix      ON objects(bucket_id, key text_pattern_ops);
CREATE INDEX idx_objects_transcode_status ON objects(transcode_status)
  WHERE transcode_status IN ('pending', 'processing');
```

### Transcode status state machine

```
   ┌──────┐  upload non-media file
   │ none │◄──────────────────────────┐
   └──┬───┘                           │
      │ upload media + bucket auto_transcode_mode!=off
      ▼
  ┌─────────┐  pre-flight estimate exceeds quota_bytes
  │ pending │──────────────────────────► skipped_quota
  └──┬──────┘     (no jobs enqueued; original intact)
     │ worker picks up
     ▼
 ┌────────────┐  ffmpeg crash, attempts exhausted
 │ processing │──────────────────────► failed
 └──┬─────────┘
    │ all rungs done + finalize publish
    ▼
   ┌──────┐    actual segments bytes exceed reservation
   │ done │──────────────────────────► failed_quota
   └──┬───┘    (segments reaped, transcoded_bytes=0)
      │ user re-triggers via DELETE /:b/:k?transcodes
      ▼
   none → pending → ...
```

| State           | Meaning                                                          | Segments on disk? | Original usable? |
|-----------------|------------------------------------------------------------------|-------------------|------------------|
| `none`          | Not a media file, or auto-transcode disabled                     | no                | yes              |
| `pending`       | Job(s) queued, reservation charged                               | no (yet)          | yes              |
| `processing`    | Worker actively encoding                                         | partial           | yes              |
| `done`          | All rungs + finalize complete                                    | **yes**           | yes              |
| `failed`        | FFmpeg crashed past max_attempts; reservation refunded           | no (reaped)       | yes              |
| `skipped_quota` | Pre-flight estimate exceeded quota; no jobs ever ran             | no                | yes              |
| `failed_quota`  | Output exceeded the reservation at publish; reaped               | no (reaped)       | yes              |

In every terminal state the original file remains downloadable. Only
`done` produces a streamable HLS master.

### Quota accounting

`users.used_bytes` is maintained transactionally and equals:

```
  SUM(size_bytes + transcoded_bytes + transcode_reserved_bytes)  ← objects (any is_deleted)
+ SUM(object_versions.size_bytes)                                ← scoped to owner's buckets
```

Per object the breakdown is:

```
size_bytes                  ← original upload, charged on PUT/multipart complete
transcoded_bytes            ← HLS segments after publish, 0 until done
transcode_reserved_bytes    ← in-flight estimate, non-zero while pending/processing
```

The reservation column closes a TOCTOU race: two concurrent uploads of
the same large file could both pre-flight against the same `used_bytes`
snapshot. With reservation, the second one's pre-flight sees the first's
estimate already debited and gets `skipped_quota` deterministically. See
[../storage-management.md](../storage-management.md) for the full lifecycle.

The `text_pattern_ops` index makes `LIKE 'photos/%'` prefix scans fast —
critical for `ListObjectsV2` with a `prefix` parameter.

---

## `multipart_uploads`

In-progress multipart uploads. Lives until completion, abort, or 7-day expiry.

```sql
CREATE TABLE multipart_uploads (
  upload_id       TEXT PRIMARY KEY,         -- UUID
  bucket_id       UUID REFERENCES buckets(id) ON DELETE CASCADE,
  key             TEXT NOT NULL,
  owner_id        UUID REFERENCES users(id),
  content_type    TEXT DEFAULT 'application/octet-stream',
  parts           JSONB DEFAULT '[]',       -- unused; see multipart_parts table
  total_size      BIGINT DEFAULT 0,
  status          TEXT DEFAULT 'in-progress'
                    CHECK (status IN ('in-progress','complete','aborted')),
  created_at      TIMESTAMPTZ DEFAULT now(),
  expires_at      TIMESTAMPTZ DEFAULT (now() + INTERVAL '7 days')
);
```

---

## `multipart_parts`

One row per uploaded chunk. Replaces the JSONB array approach (safer under
concurrent part uploads).

```sql
CREATE TABLE multipart_parts (
  upload_id    TEXT REFERENCES multipart_uploads(upload_id) ON DELETE CASCADE,
  part_number  INT CHECK (part_number BETWEEN 1 AND 10000),
  etag         TEXT NOT NULL,               -- MD5 of part
  size_bytes   BIGINT NOT NULL,
  uploaded_at  TIMESTAMPTZ DEFAULT now(),
  PRIMARY KEY (upload_id, part_number)
);
```

UPSERT (`ON CONFLICT (upload_id, part_number)`) allows clients to safely
re-upload a part if a transient error happens — the new ETag and size win.

---

## `transcode_jobs`

Queue table polled by Python workers via `SELECT ... FOR UPDATE SKIP LOCKED`.

```sql
CREATE TABLE transcode_jobs (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  object_id       UUID REFERENCES objects(id) ON DELETE CASCADE,
  input_path      TEXT NOT NULL,
  output_dir      TEXT NOT NULL,
  file_type       TEXT CHECK (file_type IN ('video','audio','image')),
  status          TEXT DEFAULT 'pending'
                    CHECK (status IN ('pending','processing','done','failed')),
  attempts        INT DEFAULT 0,
  max_attempts    INT DEFAULT 3,
  priority        INT DEFAULT 5,            -- 1=highest, 10=lowest
  worker_id       TEXT,
  error           TEXT,
  created_at      TIMESTAMPTZ DEFAULT now(),
  started_at      TIMESTAMPTZ,
  done_at         TIMESTAMPTZ
);

CREATE INDEX idx_transcode_jobs_pending
  ON transcode_jobs(priority, created_at)
  WHERE status = 'pending';
```

Partial index — only pending jobs in the index, so polling is fast even
with millions of completed jobs in the table.

---

## `audit_log`

Append-only event stream. Never updated, only INSERTed (asynchronously).

```sql
CREATE TABLE audit_log (
  id              BIGSERIAL PRIMARY KEY,
  user_id         UUID REFERENCES users(id) ON DELETE SET NULL,
  action          TEXT NOT NULL,            -- 'PUT_OBJECT', 'LOGIN', etc.
  bucket_name     TEXT,
  object_key      TEXT,
  size_bytes      BIGINT,
  status_code     INT,
  ip_address      INET,
  user_agent      TEXT,
  details         JSONB DEFAULT '{}',
  ts              TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX idx_audit_user_ts   ON audit_log(user_id, ts DESC);
CREATE INDEX idx_audit_action_ts ON audit_log(action, ts DESC);
CREATE INDEX idx_audit_ts        ON audit_log(ts DESC);
```

`SET NULL` on user delete preserves the audit trail even after the user
is gone (compliance + forensics).

---

## `system_config`

Admin-tunable runtime knobs.

```sql
CREATE TABLE system_config (
  key         TEXT PRIMARY KEY,
  value       TEXT NOT NULL,
  description TEXT,
  updated_at  TIMESTAMPTZ DEFAULT now()
);
```

Seeded keys (all values are strings; the API parses them):

| Key | Default | Meaning |
|---|---|---|
| `total_quota_bytes` | `0` | System-wide cap. `0` = auto from physical disk minus reserved |
| `reserved_bytes` | `5368709120` | Headroom for OS/DB/logs (5 GiB) |
| `disk_full_threshold_pct` | `95` | Reject uploads when disk reaches this % |
| `overcommit_allowed` | `false` | Allow SUM(quotas) > effective total |

---

## Triggers

```sql
CREATE OR REPLACE FUNCTION touch_updated_at() RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_users_updated   BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE TRIGGER trg_objects_updated BEFORE UPDATE ON objects
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
```

That's the only stored logic. Everything else is in the application layer.
