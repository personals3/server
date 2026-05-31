-- =============================================================================
-- Quota reconciliation — recompute users.used_bytes from EVERY component:
--
--   live objects (size_bytes + transcoded_bytes)
-- + all prior versions
-- = what's actually on disk for each user.
--
-- The cleaner enforces disk↔DB consistency but quota drift can happen via:
--   - Multipart upload reconciliation bugs (especially mid-stream failures)
--   - Bucket force-delete bugs (historical; fixed in v4.x)
--   - Manual SQL changes
--   - Imports that crashed mid-write
--
-- Idempotent. Safe to run as often as you want. Recommended weekly via cron.
-- =============================================================================

\echo === Current drift ===
SELECT u.email,
       pg_size_pretty(u.used_bytes) AS recorded,
       pg_size_pretty(
         (SELECT COALESCE(SUM(size_bytes + COALESCE(transcoded_bytes,0) + COALESCE(transcode_reserved_bytes,0)),0)
            FROM objects
           WHERE bucket_id IN (SELECT id FROM buckets WHERE owner_id = u.id))
       + (SELECT COALESCE(SUM(ov.size_bytes),0) FROM object_versions ov
            JOIN objects o ON o.id = ov.object_id
            WHERE o.bucket_id IN (SELECT id FROM buckets WHERE owner_id = u.id))
       ) AS actual,
       pg_size_pretty(
         u.used_bytes
         - ((SELECT COALESCE(SUM(size_bytes + COALESCE(transcoded_bytes,0) + COALESCE(transcode_reserved_bytes,0)),0)
              FROM objects
             WHERE bucket_id IN (SELECT id FROM buckets WHERE owner_id = u.id))
          + (SELECT COALESCE(SUM(ov.size_bytes),0) FROM object_versions ov
              JOIN objects o ON o.id = ov.object_id
              WHERE o.bucket_id IN (SELECT id FROM buckets WHERE owner_id = u.id)))
       ) AS drift
  FROM users u
 ORDER BY u.email;

\echo === Applying corrections ===
UPDATE users u
   SET used_bytes =
       (SELECT COALESCE(SUM(size_bytes + COALESCE(transcoded_bytes,0) + COALESCE(transcode_reserved_bytes,0)),0)
          FROM objects
         WHERE bucket_id IN (SELECT id FROM buckets WHERE owner_id = u.id))
     + (SELECT COALESCE(SUM(ov.size_bytes),0) FROM object_versions ov
          JOIN objects o ON o.id = ov.object_id
          WHERE o.bucket_id IN (SELECT id FROM buckets WHERE owner_id = u.id));

\echo === Done. Re-run the SELECT above (drift should be 0 bytes for everyone). ===
