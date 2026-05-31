"""Worker entry point.

Polls transcode_jobs every WORKER_POLL_INTERVAL seconds (default 2s).

Concurrency model: the container spawns WORKER_CONCURRENCY threads
(default 4), each with its own DB connection. Threads claim jobs
independently via FOR UPDATE SKIP LOCKED. ffmpeg runs inside subprocess.run
which releases the GIL, so Python threads parallelize cleanly.

Scaling out further (more containers): docker compose up -d --scale worker=N
"""

from __future__ import annotations

import logging
import os
import signal
import socket
import sys
import threading
import time
import traceback

from .cancel import registry as cancel_registry, start_subscriber
from .db import Database
from .transcoder import transcode, JobCancelled, JobSkipped

log = logging.getLogger("worker")

POLL_INTERVAL = float(os.getenv("WORKER_POLL_INTERVAL", "2"))
WORKER_CONCURRENCY = max(1, int(os.getenv("WORKER_CONCURRENCY", "4")))
HOSTNAME = os.getenv("HOSTNAME", socket.gethostname()) or "worker"


class Worker:
    """One worker thread — owns its own DB connection and polling loop."""

    # Shared stop flag across all threads in this process.
    _stop_event = threading.Event()

    def __init__(self, worker_id: str) -> None:
        dsn = os.environ.get("DATABASE_URL")
        if not dsn:
            log.error("DATABASE_URL is required")
            sys.exit(1)
        self.db = Database(dsn)
        self.worker_id = worker_id

    def run(self) -> None:
        log.info("worker %s starting (poll=%ss)", self.worker_id, POLL_INTERVAL)

        idle_loops = 0
        while not Worker._stop_event.is_set():
            try:
                job = self.db.claim_job(self.worker_id)
            except Exception as e:
                log.error("[%s] claim_job failed: %s", self.worker_id, e)
                Worker._stop_event.wait(5)
                continue

            if not job:
                idle_loops += 1
                if idle_loops % 30 == 0:    # log heartbeat every ~60s
                    log.debug("[%s] idle (no jobs)", self.worker_id)
                Worker._stop_event.wait(POLL_INTERVAL)
                continue

            idle_loops = 0
            self._process(job)

        self.db.close()
        log.info("worker %s stopped", self.worker_id)

    def _process(self, job: dict) -> None:
        job_id = job["id"]
        object_id = job["object_id"]
        group_id = job.get("group_id")
        params = job.get("params") or {}
        log.info(
            "[%s] job %s: %s %s (group=%s, attempt %d/%d)",
            self.worker_id, job_id, job["file_type"], job["input_path"],
            group_id, job["attempts"], job["max_attempts"],
        )
        # Register this job so a cancel pub/sub message can find + kill its ffmpeg.
        cancel_registry.register_job(str(job_id), str(object_id))
        try:
            output = transcode(
                input_path=job["input_path"],
                output_dir=job["output_dir"],
                file_type=job["file_type"],
                params=params,
                is_cancelled=lambda: not self.db.job_still_exists(str(job_id)),
                progress_cb=lambda pct: self.db.update_progress(str(job_id), pct),
                job_id=str(job_id),  # passed through so video.py can register the ffmpeg PID
            )
            self.db.complete_job(str(job_id), output)
            log.info("[%s] job %s done", self.worker_id, job_id)
        except JobSkipped as e:
            log.info("[%s] job %s skipped: %s", self.worker_id, job_id, e)
            self.db.skip_job(str(job_id), str(e))
        except JobCancelled as e:
            log.info("[%s] job %s cancelled: %s", self.worker_id, job_id, e)
            # Don't call fail_job — the job row is already gone (cascade-deleted).
            import shutil
            try: shutil.rmtree(job["output_dir"])
            except Exception: pass
        except Exception as e:
            err = f"{type(e).__name__}: {e}"
            log.error("[%s] job %s failed: %s", self.worker_id, job_id, err)
            log.debug("traceback:\n%s", traceback.format_exc())
            try:
                self.db.fail_job(str(job_id), err)
            except Exception as e2:
                log.error("[%s] fail_job also failed: %s", self.worker_id, e2)
        finally:
            cancel_registry.unregister_job(str(job_id))


def configure_logging() -> None:
    level = os.getenv("LOG_LEVEL", "INFO").upper()
    logging.basicConfig(
        level=level,
        format="%(asctime)s %(levelname)s %(name)s | %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )


def _stop_handler(*_args) -> None:
    log.info("received signal — finishing current jobs then stopping")
    Worker._stop_event.set()


def main() -> None:
    configure_logging()
    signal.signal(signal.SIGTERM, _stop_handler)
    signal.signal(signal.SIGINT, _stop_handler)

    log.info(
        "spawning %d worker thread(s) (concurrency=%s); host=%s",
        WORKER_CONCURRENCY, WORKER_CONCURRENCY, HOSTNAME,
    )

    # Cancel-on-delete subscriber — listens to Valkey, kills local ffmpegs
    # when the API tells us an object has been deleted/replaced.
    start_subscriber(Worker._stop_event)

    threads: list[threading.Thread] = []
    workers: list[Worker] = []
    for i in range(WORKER_CONCURRENCY):
        wid = f"{HOSTNAME}-{i+1}"
        w = Worker(wid)
        workers.append(w)
        t = threading.Thread(target=w.run, name=f"worker-{i+1}", daemon=False)
        t.start()
        threads.append(t)

    # Block forever (well, until SIGTERM). Each thread exits when _stop_event is set.
    for t in threads:
        while t.is_alive():
            t.join(timeout=1.0)

    log.info("all worker threads stopped")


if __name__ == "__main__":
    main()
