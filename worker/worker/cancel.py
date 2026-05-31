"""Cancel-on-delete signalling.

The API publishes a JSON message on the Valkey `transcode:cancel` channel
when it deletes/replaces an object that might be mid-transcode. A subscriber
thread inside this process matches the message against in-flight jobs and
SIGTERMs the local ffmpeg subprocess so the worker thread can fail-fast.

Two paths to noticing the cancel:
  1. This pub/sub — instant (~ms)
  2. db.job_still_exists() — checked every progress callback (~1s)
Both are best-effort. Either alone is correct; together is fast + robust.
"""

from __future__ import annotations

import json
import logging
import os
import subprocess
import threading
import uuid
from typing import Optional

log = logging.getLogger("worker.cancel")

CHANNEL = "transcode:cancel"


class ProcessRegistry:
    """Tracks the currently-running ffmpeg subprocess for each worker thread.

    Each entry: job_id (uuid str) → {object_id, proc (subprocess.Popen)}.
    `proc` may be None if the worker has claimed a job but not yet spawned
    ffmpeg (e.g. doing ffprobe).
    """

    def __init__(self) -> None:
        self._by_job: dict[str, dict] = {}
        self._lock = threading.RLock()

    def register_job(self, job_id: str, object_id: str) -> None:
        with self._lock:
            self._by_job[job_id] = {"object_id": object_id, "proc": None}

    def set_process(self, job_id: str, proc: subprocess.Popen) -> None:
        with self._lock:
            if job_id in self._by_job:
                self._by_job[job_id]["proc"] = proc

    def clear_process(self, job_id: str) -> None:
        with self._lock:
            if job_id in self._by_job:
                self._by_job[job_id]["proc"] = None

    def unregister_job(self, job_id: str) -> None:
        with self._lock:
            self._by_job.pop(job_id, None)

    def kill_by_job(self, job_id: str, reason: str = "") -> bool:
        """SIGTERM the ffmpeg attached to this job, if any. Returns True if a
        process was killed."""
        with self._lock:
            entry = self._by_job.get(job_id)
        return _kill_entry(job_id, entry, reason)

    def kill_by_object(self, object_id: str, reason: str = "") -> int:
        """Kill every ffmpeg attached to any job for this object. Returns count."""
        killed = 0
        with self._lock:
            targets = [(jid, e) for jid, e in self._by_job.items()
                       if e.get("object_id") == object_id]
        for jid, e in targets:
            if _kill_entry(jid, e, reason):
                killed += 1
        return killed


def _kill_entry(job_id: str, entry: Optional[dict], reason: str) -> bool:
    if not entry:
        return False
    proc: Optional[subprocess.Popen] = entry.get("proc")
    if proc is None or proc.poll() is not None:
        return False
    try:
        log.info("cancel: SIGTERM job=%s pid=%s reason=%s", job_id, proc.pid, reason)
        proc.terminate()
        # Give it a moment; force-kill if it ignores us.
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            log.warning("cancel: ffmpeg pid=%s didn't exit on SIGTERM, SIGKILL", proc.pid)
            proc.kill()
        return True
    except Exception as e:
        log.error("cancel: kill failed for job=%s: %s", job_id, e)
        return False


# Module-level singleton; every worker thread shares it.
registry = ProcessRegistry()


def start_subscriber(stop_event: threading.Event) -> Optional[threading.Thread]:
    """Spawn a daemon thread that listens to the Valkey channel and acts on
    matching cancel messages. Returns the thread (or None if disabled).

    Reads VALKEY_URL from env. If missing, logs a warning and returns None —
    cancel-on-delete then falls back to the db.job_still_exists path only.
    """
    url = os.getenv("VALKEY_URL", "")
    if not url:
        log.info("cancel: VALKEY_URL not set; pub/sub disabled (DB poll fallback only)")
        return None

    try:
        import redis  # local import so worker still starts if redis lib missing
    except ImportError:
        log.warning("cancel: redis package not installed; pub/sub disabled")
        return None

    def run() -> None:
        # Reconnect loop — Valkey blips shouldn't kill the subscriber.
        # socket_timeout=None lets pubsub.listen() block forever; the
        # health_check_interval keeps the connection alive across NAT
        # idle-timeouts without dropping us back into the reconnect path.
        while not stop_event.is_set():
            try:
                client = redis.from_url(
                    url,
                    decode_responses=True,
                    socket_timeout=None,
                    socket_keepalive=True,
                    health_check_interval=30,
                )
                pubsub = client.pubsub(ignore_subscribe_messages=True)
                pubsub.subscribe(CHANNEL)
                log.info("cancel: subscribed to %s", CHANNEL)
                # get_message with a long timeout lets us notice stop_event
                # promptly on shutdown without spamming the reconnect path.
                while not stop_event.is_set():
                    msg = pubsub.get_message(timeout=5.0)
                    if msg is None:
                        continue
                    if msg.get("type") != "message":
                        continue
                    try:
                        data = json.loads(msg.get("data") or "{}")
                    except Exception:
                        continue
                    reason = str(data.get("reason") or "")
                    if jid := data.get("jobId"):
                        if _looks_like_uuid(jid):
                            registry.kill_by_job(str(jid), reason)
                    if oid := data.get("objectId"):
                        if _looks_like_uuid(oid):
                            n = registry.kill_by_object(str(oid), reason)
                            if n > 0:
                                log.info("cancel: killed %d job(s) for object %s", n, oid)
            except Exception as e:
                if not stop_event.is_set():
                    log.warning("cancel subscriber error (%s) — retrying in 5s", e)
                    stop_event.wait(5)

    t = threading.Thread(target=run, name="cancel-subscriber", daemon=True)
    t.start()
    return t


def _looks_like_uuid(s: object) -> bool:
    try:
        uuid.UUID(str(s))
        return True
    except Exception:
        return False
