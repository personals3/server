"""Regression test: a permanently failed leaf job must not strand its
group's 'waiting' finalize job.

Only complete_job/skip_job promote waiting siblings, and the cleaner's
stuck-transcode reaper only resets 'processing' rows — so before the fix,
one quality rung exhausting its retries left the finalize 'waiting'
forever. fail_job now fails the waiting siblings alongside its existing
refund + reap + object-failed bookkeeping.

Needs a throwaway Postgres (the fixture DROPs the public schema):

  docker run --rm -d --name ps3-test-pg -p 55432:5432 \
    -e POSTGRES_PASSWORD=test postgres:16-alpine
  TEST_DATABASE_URL=postgres://postgres:test@localhost:55432/postgres \
    python -m pytest worker/tests/ -v
"""

import glob
import os
import uuid

import pytest

psycopg = pytest.importorskip("psycopg")

from worker.db import Database  # noqa: E402

MIGRATIONS = os.path.join(os.path.dirname(__file__), "..", "..", "db", "migrations", "*.sql")
RESERVED = 5000


@pytest.fixture()
def dsn(tmp_path, monkeypatch):
    url = os.getenv("TEST_DATABASE_URL")
    if not url:
        pytest.skip("TEST_DATABASE_URL not set — skipping DB-backed regression test")
    # fail_job reaps segments under STORAGE_ROOT — keep it inside tmp.
    monkeypatch.setenv("STORAGE_ROOT", str(tmp_path))
    with psycopg.connect(url, autocommit=True) as conn:
        conn.execute("DROP SCHEMA public CASCADE; CREATE SCHEMA public")
        files = sorted(glob.glob(MIGRATIONS))
        assert files, f"no migrations found at {MIGRATIONS}"
        for f in files:
            with open(f) as fh:
                conn.execute(fh.read())
    return url


def _seed_group(conn):
    """User + bucket + object with a live reservation, and a video group:
    one quality job done, one processing at its last attempt, finalize
    waiting. Returns ids."""
    with conn.cursor() as cur:
        cur.execute(
            """INSERT INTO users (email, name, quota_bytes, used_bytes)
               VALUES (%s, 'Regression Test', 1073741824, %s) RETURNING id""",
            (f"test-{uuid.uuid4()}@example.test", RESERVED),
        )
        user_id = cur.fetchone()[0]
        cur.execute(
            """INSERT INTO buckets (name, owner_id) VALUES ('regress-worker', %s)
               RETURNING id""",
            (user_id,),
        )
        bucket_id = cur.fetchone()[0]
        cur.execute(
            """INSERT INTO objects (bucket_id, key, size_bytes, etag, storage_path,
                                    transcode_status, transcode_reserved_bytes)
               VALUES (%s, 'movie.mp4', 1000, 'etag', '/x', 'processing', %s)
               RETURNING id""",
            (bucket_id, RESERVED),
        )
        object_id = cur.fetchone()[0]

        group_id = uuid.uuid4()
        jobs = {}
        for name, status, attempts in (
            ("q_done", "done", 1),
            ("q_failing", "processing", 3),  # at max_attempts — next fail is final
            ("finalize", "waiting", 0),
        ):
            cur.execute(
                """INSERT INTO transcode_jobs
                     (object_id, input_path, output_dir, file_type, status,
                      attempts, max_attempts, group_id)
                   VALUES (%s, '/in', '/out', 'video', %s, %s, 3, %s)
                   RETURNING id""",
                (object_id, status, attempts, group_id),
            )
            jobs[name] = cur.fetchone()[0]
    conn.commit()
    return user_id, object_id, jobs


def test_permanent_failure_fails_waiting_siblings(dsn):
    with psycopg.connect(dsn) as conn:
        user_id, object_id, jobs = _seed_group(conn)

        # Control: an unrelated group's waiting job must stay untouched.
        with conn.cursor() as cur:
            cur.execute(
                """INSERT INTO transcode_jobs
                     (object_id, input_path, output_dir, file_type, status,
                      attempts, max_attempts, group_id)
                   VALUES (%s, '/in', '/out', 'video', 'waiting', 0, 3, %s)
                   RETURNING id""",
                (object_id, uuid.uuid4()),
            )
            control_id = cur.fetchone()[0]
        conn.commit()

        db = Database(dsn)
        try:
            db.fail_job(str(jobs["q_failing"]), "synthetic ffmpeg crash")
        finally:
            db.close()

        with conn.cursor() as cur:
            cur.execute(
                "SELECT id, status, error FROM transcode_jobs WHERE id = ANY(%s)",
                ([jobs["q_failing"], jobs["finalize"], control_id],),
            )
            by_id = {row[0]: (row[1], row[2]) for row in cur.fetchall()}

            assert by_id[jobs["q_failing"]][0] == "failed"
            # The core regression: finalize must not be stranded in 'waiting'.
            assert by_id[jobs["finalize"]][0] == "failed"
            assert by_id[jobs["finalize"]][1] == "sibling job failed permanently"
            assert by_id[control_id][0] == "waiting"

            cur.execute(
                """SELECT transcode_status, COALESCE(transcode_reserved_bytes, 0)
                     FROM objects WHERE id = %s""",
                (object_id,),
            )
            status, reserved = cur.fetchone()
            assert status == "failed"
            assert reserved == 0

            cur.execute("SELECT used_bytes FROM users WHERE id = %s", (user_id,))
            assert cur.fetchone()[0] == 0  # reservation refunded


def test_retryable_failure_keeps_group_waiting(dsn):
    """A non-final failure reverts to 'pending' and must NOT touch siblings."""
    with psycopg.connect(dsn) as conn:
        _, _, jobs = _seed_group(conn)
        with conn.cursor() as cur:
            cur.execute(
                "UPDATE transcode_jobs SET attempts = 1 WHERE id = %s",
                (jobs["q_failing"],),
            )
        conn.commit()

        db = Database(dsn)
        try:
            db.fail_job(str(jobs["q_failing"]), "transient hiccup")
        finally:
            db.close()

        with conn.cursor() as cur:
            cur.execute(
                "SELECT status FROM transcode_jobs WHERE id = %s",
                (jobs["q_failing"],),
            )
            assert cur.fetchone()[0] == "pending"  # retryable
            cur.execute(
                "SELECT status FROM transcode_jobs WHERE id = %s",
                (jobs["finalize"],),
            )
            assert cur.fetchone()[0] == "waiting"  # untouched
