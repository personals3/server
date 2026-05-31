-- =============================================================================
-- Part 4: Multipart upload — per-part rows
--
-- Parts arrive in parallel; an UPSERT into a real table is far cleaner
-- (and safer under concurrency) than juggling a JSONB array. The unused
-- multipart_uploads.parts column stays for backward compatibility.
-- =============================================================================

CREATE TABLE IF NOT EXISTS multipart_parts (
  upload_id    TEXT NOT NULL REFERENCES multipart_uploads(upload_id) ON DELETE CASCADE,
  part_number  INT  NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
  etag         TEXT NOT NULL,
  size_bytes   BIGINT NOT NULL CHECK (size_bytes >= 0),
  uploaded_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (upload_id, part_number)
);

CREATE INDEX IF NOT EXISTS idx_multipart_parts_upload
  ON multipart_parts(upload_id);
