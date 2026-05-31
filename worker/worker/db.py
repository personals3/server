"""PostgreSQL job-queue access using SELECT ... FOR UPDATE SKIP LOCKED."""

from __future__ import annotations

import json
import logging
import os
import shutil
from contextlib import contextmanager
from typing import Optional, Iterator

import psycopg
from psycopg.rows import dict_row

log = logging.getLogger(__name__)


def _dir_size(path: str) -> int:
    """Sum size_bytes of every regular file under `path`. Returns 0 on any
    error (missing dir, perms). Used at transcode publish-point so the
    user gets charged for the actual segments output."""
    total = 0
    try:
        for root, _, files in os.walk(path):
            for name in files:
                try:
                    total += os.path.getsize(os.path.join(root, name))
                except OSError:
                    pass
    except OSError:
        pass
    return total


class Database:
    def __init__(self, dsn: str):
        self.dsn = dsn
        self.conn: Optional[psycopg.Connection] = None

    def connect(self) -> psycopg.Connection:
        if self.conn is None or self.conn.closed:
            self.conn = psycopg.connect(self.dsn, autocommit=False)
        return self.conn

    def close(self) -> None:
        if self.conn and not self.conn.closed:
            self.conn.close()

    @contextmanager
    def cursor(self) -> Iterator[psycopg.Cursor]:
        conn = self.connect()
        with conn.cursor(row_factory=dict_row) as cur:
            try:
                yield cur
                conn.commit()
            except Exception:
                conn.rollback()
                raise

    # ---------------------------------------------------------------- claim
    def claim_job(self, worker_id: str) -> Optional[dict]:
        """Atomically pick one pending job and mark it processing.

        Orders by priority ASC (1 = highest) then created_at so older jobs
        of the same priority get picked first. FOR UPDATE SKIP LOCKED keeps
        many workers from racing on the same row.

        Returns dict with group_id (nullable) and params (jsonb dict).
        """
        with self.cursor() as cur:
            cur.execute(
                """
                WITH next_job AS (
                    SELECT id FROM transcode_jobs
                     WHERE status = 'pending'
                       AND attempts < max_attempts
                     ORDER BY priority, created_at
                     LIMIT 1
                     FOR UPDATE SKIP LOCKED
                )
                UPDATE transcode_jobs SET
                    status     = 'processing',
                    started_at = now(),
                    worker_id  = %s,
                    attempts   = attempts + 1
                WHERE id = (SELECT id FROM next_job)
                RETURNING id, object_id, input_path, output_dir, file_type,
                          attempts, max_attempts,
                          group_id, params;
                """,
                (worker_id,),
            )
            return cur.fetchone()

    # ---------------------------------------------------------------- done
    def complete_job(self, job_id: str, output: dict) -> None:
        """Mark a single job done.

        Three cases:
          1. Job has no group_id (legacy monolithic OR audio/image) →
             behave as before, write objects.transcoded + status='done'.
          2. Job IS the 'video_finalize' step → write objects.transcoded
             + status='done'. This is the publish point for video pipelines.
          3. Job is a video_quality_* or video_thumbnails subtask → just
             mark the job done. Don't touch objects.transcoded yet (the
             finalize step will). If this was the last sibling, promote
             any waiting jobs (the finalize) to 'pending'.
        """
        with self.cursor() as cur:
            cur.execute(
                "SELECT group_id, file_type FROM transcode_jobs WHERE id = %s",
                (job_id,),
            )
            row = cur.fetchone()
            if not row:
                return  # job vanished; nothing to do
            group_id = row["group_id"]
            file_type = row["file_type"]

            cur.execute(
                "UPDATE transcode_jobs SET status='done', done_at=now() WHERE id=%s",
                (job_id,),
            )

            is_publish_point = (group_id is None) or (file_type == "video_finalize")
            if is_publish_point:
                # 1. Resolve object_id + capture the PREVIOUS transcoded_bytes
                #    (zero on first transcode; non-zero if we're re-transcoding).
                cur.execute(
                    """
                    SELECT o.id, o.bucket_id, COALESCE(o.transcoded_bytes, 0) AS old_bytes,
                           COALESCE(o.transcode_reserved_bytes, 0) AS reserved,
                           b.owner_id
                      FROM transcode_jobs tj
                      JOIN objects o ON o.id = tj.object_id
                      JOIN buckets b ON b.id = o.bucket_id
                     WHERE tj.id = %s
                     FOR UPDATE OF o
                    """,
                    (job_id,),
                )
                row2 = cur.fetchone()
                if row2 is not None:
                    object_id = row2["id"]
                    owner_id  = row2["owner_id"]
                    old_bytes = int(row2["old_bytes"])
                    reserved  = int(row2["reserved"])

                    # 2. Measure the segments dir on disk.
                    storage_root = os.getenv("STORAGE_ROOT", "/storage")
                    segments_dir = os.path.join(storage_root, "segments", str(object_id))
                    new_bytes = _dir_size(segments_dir)

                    # 3. Reconcile the reservation. API pre-flight charged
                    #    `reserved` against the user when this transcode was
                    #    enqueued. Now we know the truth:
                    #      - delta_user  = new_bytes - reserved - old_bytes
                    #        (re-transcode: old_bytes was the previously
                    #         published size; subtract so we only book the
                    #         change. For a fresh transcode old_bytes == 0.)
                    #      - if delta_user > 0  → reserve the difference
                    #      - if delta_user < 0  → refund the over-reservation
                    #    Failed reserve (overshoot beyond quota) → reap +
                    #    refund the entire reservation, mark failed_quota.
                    delta_user = new_bytes - reserved - old_bytes
                    charged = True
                    if delta_user > 0:
                        cur.execute(
                            """
                            UPDATE users
                               SET used_bytes = used_bytes + %s
                             WHERE id = %s
                               AND used_bytes + %s <= quota_bytes
                            RETURNING used_bytes
                            """,
                            (delta_user, owner_id, delta_user),
                        )
                        charged = cur.fetchone() is not None
                    elif delta_user < 0:
                        cur.execute(
                            """
                            UPDATE users
                               SET used_bytes = GREATEST(used_bytes + %s, 0)
                             WHERE id = %s
                            """,
                            (delta_user, owner_id),
                        )

                    if not charged:
                        # Output exceeded the reservation AND we can't extend.
                        # Reap segments + refund the FULL reservation (the
                        # delta-reserve above already failed cleanly).
                        log.warning(
                            "transcode publish refused (quota): object=%s "
                            "actual=%d reserved=%d delta=%d; reaping",
                            object_id, new_bytes, reserved, delta_user,
                        )
                        try:
                            shutil.rmtree(segments_dir, ignore_errors=True)
                        except Exception:
                            log.exception("failed to reap segments_dir=%s", segments_dir)
                        if reserved > 0:
                            cur.execute(
                                """UPDATE users
                                      SET used_bytes = GREATEST(used_bytes - %s, 0)
                                    WHERE id = %s""",
                                (reserved, owner_id),
                            )
                        # Snapshot user state for the dashboard message.
                        cur.execute(
                            "SELECT quota_bytes, used_bytes FROM users WHERE id = %s",
                            (owner_id,),
                        )
                        q_row = cur.fetchone()
                        quota_bytes = int(q_row["quota_bytes"]) if q_row else 0
                        used_bytes  = int(q_row["used_bytes"])  if q_row else 0
                        available   = max(quota_bytes - used_bytes, 0)
                        deficit     = max(new_bytes - available, 0)
                        detail = json.dumps({
                            "quota": {
                                "actualBytes":    new_bytes,
                                "reservedBytes": reserved,
                                "availableBytes": available,
                                "quotaBytes":     quota_bytes,
                                "usedBytes":      used_bytes,
                                "deficitBytes":   deficit,
                                "reason":         "failed_quota",
                            }
                        })
                        cur.execute(
                            """UPDATE objects SET transcode_status='failed_quota',
                                                 transcoded = %s::jsonb,
                                                 transcode_reserved_bytes = 0,
                                                 updated_at = now()
                                WHERE id = %s""",
                            (detail, object_id),
                        )
                    else:
                        # 4. Happy path — record the publish. The reservation
                        #    has been settled into transcoded_bytes; clear the
                        #    reserved column so it doesn't double-count.
                        cur.execute(
                            """
                            UPDATE objects
                               SET transcoded = %s::jsonb,
                                   transcoded_bytes = %s,
                                   transcode_reserved_bytes = 0,
                                   transcode_status = 'done',
                                   updated_at = now()
                             WHERE id = %s
                            """,
                            (json.dumps(output), new_bytes, object_id),
                        )

            # Per-rung incremental publish: as each video_quality_* or
            # video_thumbnails job completes, measure its own subdir and
            # move that many bytes from `reserved` to `transcoded_bytes`.
            # used_bytes stays unchanged (we're just shifting columns),
            # but the dashboard's stacked bar now ticks up smoothly as
            # rungs land instead of jumping at finalize.
            #
            # The finalize step still settles any remaining delta (master
            # manifest + thumbnails + estimate error). If a rung's actual
            # output happens to be larger than what reservation has left,
            # the GREATEST clamps to zero and finalize's check-and-reserve
            # picks up the overflow as failed_quota.
            elif file_type.startswith("video_quality_") or file_type == "video_thumbnails":
                cur.execute(
                    """
                    SELECT o.id, b.owner_id
                      FROM transcode_jobs tj
                      JOIN objects o ON o.id = tj.object_id
                      JOIN buckets b ON b.id = o.bucket_id
                     WHERE tj.id = %s
                     FOR UPDATE OF o
                    """,
                    (job_id,),
                )
                row3 = cur.fetchone()
                if row3 is not None:
                    object_id = row3["id"]
                    storage_root = os.getenv("STORAGE_ROOT", "/storage")
                    if file_type.startswith("video_quality_"):
                        rung = file_type[len("video_quality_"):]   # e.g. "1080p"
                        rung_dir = os.path.join(storage_root, "segments",
                                                str(object_id), rung)
                        rung_bytes = _dir_size(rung_dir)
                    else:
                        # Thumbnails land directly under segments/{O}/ as
                        # thumb_*.jpg; measure those specifically so we
                        # don't double-count the per-rung subdirs.
                        seg_dir = os.path.join(storage_root, "segments",
                                               str(object_id))
                        rung_bytes = 0
                        if os.path.isdir(seg_dir):
                            for f in os.listdir(seg_dir):
                                if f.startswith("thumb_"):
                                    try:
                                        rung_bytes += os.path.getsize(
                                            os.path.join(seg_dir, f))
                                    except OSError:
                                        pass
                    if rung_bytes > 0:
                        cur.execute(
                            """
                            UPDATE objects
                               SET transcoded_bytes = COALESCE(transcoded_bytes,0) + %s,
                                   transcode_reserved_bytes =
                                     GREATEST(COALESCE(transcode_reserved_bytes,0) - %s, 0),
                                   updated_at = now()
                             WHERE id = %s
                            """,
                            (rung_bytes, rung_bytes, object_id),
                        )

            if group_id is not None:
                self._promote_waiting(cur, group_id)

    # ------------------------------------------------------------- skipped
    def skip_job(self, job_id: str, reason: str) -> None:
        """Mark a job 'skipped' — it ran but produced nothing (e.g. wouldn't
        upscale to 1080p when source is 720p). Treated as success for the
        purposes of group dependency tracking."""
        with self.cursor() as cur:
            cur.execute(
                "SELECT group_id FROM transcode_jobs WHERE id = %s", (job_id,),
            )
            row = cur.fetchone()
            if not row:
                return
            group_id = row["group_id"]

            cur.execute(
                """UPDATE transcode_jobs SET status='skipped', done_at=now(),
                          error=%s WHERE id=%s""",
                (reason[:2000], job_id),
            )
            if group_id is not None:
                self._promote_waiting(cur, group_id)

    # --- internal: promote any 'waiting' jobs in a group whose deps are done.
    def _promote_waiting(self, cur, group_id) -> None:
        # Are any leaf (non-waiting) siblings still active?
        cur.execute(
            """
            SELECT 1 FROM transcode_jobs
             WHERE group_id = %s AND status IN ('pending', 'processing')
             LIMIT 1
            """,
            (group_id,),
        )
        if cur.fetchone():
            return  # something still running; keep waiting
        # All leaves done — promote waiting jobs (typically just the finalize).
        cur.execute(
            "UPDATE transcode_jobs SET status='pending' WHERE group_id=%s AND status='waiting'",
            (group_id,),
        )

    # ---------------------------------------------------------------- fail
    def fail_job(self, job_id: str, error: str) -> None:
        """Mark a job failed. Auto-retries until attempts >= max_attempts."""
        with self.cursor() as cur:
            cur.execute(
                """
                UPDATE transcode_jobs SET
                  status = CASE WHEN attempts >= max_attempts THEN 'failed'
                                ELSE 'pending' END,
                  error  = %s,
                  done_at = CASE WHEN attempts >= max_attempts THEN now()
                                 ELSE NULL END
                WHERE id = %s
                RETURNING status
                """,
                (error[:2000], job_id),
            )
            row = cur.fetchone()
            if row and row["status"] == "failed":
                # Refund the pre-flight reservation. The transcode produced
                # nothing usable, so the user shouldn't be charged. Also reap
                # any partial segments so disk and DB agree.
                cur.execute(
                    """
                    SELECT o.id, o.bucket_id, COALESCE(o.transcode_reserved_bytes, 0) AS reserved,
                           b.owner_id
                      FROM transcode_jobs tj
                      JOIN objects o ON o.id = tj.object_id
                      JOIN buckets b ON b.id = o.bucket_id
                     WHERE tj.id = %s
                     FOR UPDATE OF o
                    """,
                    (job_id,),
                )
                r = cur.fetchone()
                if r is not None:
                    if int(r["reserved"]) > 0:
                        cur.execute(
                            """UPDATE users
                                  SET used_bytes = GREATEST(used_bytes - %s, 0)
                                WHERE id = %s""",
                            (int(r["reserved"]), r["owner_id"]),
                        )
                    storage_root = os.getenv("STORAGE_ROOT", "/storage")
                    segments_dir = os.path.join(storage_root, "segments", str(r["id"]))
                    try:
                        shutil.rmtree(segments_dir, ignore_errors=True)
                    except Exception:
                        log.exception("failed to reap segments_dir=%s", segments_dir)
                    cur.execute(
                        """UPDATE objects SET transcode_status = 'failed',
                                              transcode_reserved_bytes = 0,
                                              updated_at = now()
                            WHERE id = %s""",
                        (r["id"],),
                    )

    def healthcheck(self) -> bool:
        try:
            with self.connect().cursor() as cur:
                cur.execute("SELECT 1")
                cur.fetchone()
                return True
        except Exception as e:
            log.warning("db healthcheck failed: %s", e)
            return False

    def update_progress(self, job_id: str, pct: int) -> None:
        """Heartbeat from a running job. Clamp to 0-99 (100 reserved for done).
        Single-row UPDATE — cheap enough to call every second."""
        pct = max(0, min(99, int(pct)))
        with self.cursor() as cur:
            cur.execute(
                "UPDATE transcode_jobs SET progress_pct = %s WHERE id = %s",
                (pct, job_id),
            )

    def job_still_exists(self, job_id: str) -> bool:
        """True if the job row hasn't been cascade-deleted (i.e. its object
        is still in the DB). Use self.cursor() context manager so each call
        commits its transaction — otherwise we see stale snapshots."""
        with self.cursor() as cur:
            cur.execute("SELECT 1 FROM transcode_jobs WHERE id = %s", (job_id,))
            return cur.fetchone() is not None
