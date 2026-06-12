-- find-stranded-transcodes.sql
--
-- Finds transcode jobs stranded in 'waiting' by the fail_job bug fixed in
-- the worker (a permanently failed sibling never promoted or failed its
-- group's finalize job, and the cleaner only resets 'processing' rows).
--
-- Run the SELECT first to see what's stranded; the UPDATE below applies
-- the same terminal state the fixed worker now writes. Read-only until
-- you uncomment the UPDATE.
--
--   docker compose exec postgres psql -U <user> <db> -f /dev/stdin \
--     < scripts/find-stranded-transcodes.sql

-- Stranded = still 'waiting', in a group with at least one permanently
-- failed sibling and no runnable one left (nothing pending/processing).
SELECT tj.id,
       tj.object_id,
       tj.group_id,
       tj.created_at,
       o.key                AS object_key,
       o.transcode_status   AS object_status,
       COALESCE(o.transcode_reserved_bytes, 0) AS still_reserved_bytes
  FROM transcode_jobs tj
  JOIN objects o ON o.id = tj.object_id
 WHERE tj.status = 'waiting'
   AND EXISTS (SELECT 1 FROM transcode_jobs s
                WHERE s.group_id = tj.group_id
                  AND s.status = 'failed')
   AND NOT EXISTS (SELECT 1 FROM transcode_jobs s
                    WHERE s.group_id = tj.group_id
                      AND s.status IN ('pending', 'processing'))
 ORDER BY tj.created_at;

-- Remediation (uncomment to apply): mirrors what the fixed fail_job does.
-- fail_job already refunded the reservation and marked the object failed
-- when the sibling died, so only the job rows need the terminal state.
--
-- UPDATE transcode_jobs tj
--    SET status = 'failed', done_at = now(),
--        error = 'sibling job failed permanently [backfill]'
--  WHERE tj.status = 'waiting'
--    AND EXISTS (SELECT 1 FROM transcode_jobs s
--                 WHERE s.group_id = tj.group_id AND s.status = 'failed')
--    AND NOT EXISTS (SELECT 1 FROM transcode_jobs s
--                     WHERE s.group_id = tj.group_id
--                       AND s.status IN ('pending', 'processing'));
