"""Dispatch a job to the right transcoder based on file_type.

Supported types:
  audio                      → monolithic audio (HLS + MP3 + OGG)
  image                      → image transcode (WebP/AVIF/thumbs)
  video                      → LEGACY monolithic video (kept for backwards compat)
  video_quality_1080p        → one HLS variant (parallelizable across workers)
  video_quality_720p         → "
  video_quality_480p         → "
  video_quality_360p         → "
  video_thumbnails           → frame grabs (runs in parallel with quality jobs)
  video_finalize             → master.m3u8 + aggregated metadata (last step)
"""

from __future__ import annotations

import logging
import os
import re
from typing import Callable, Optional

from .video import (
    transcode_video,             # legacy monolithic
    transcode_video_quality,
    transcode_video_thumbnails,
    transcode_video_finalize,
    JobSkipped,
)
from .audio import transcode_audio
from .image import transcode_image

log = logging.getLogger(__name__)


class JobCancelled(Exception):
    """Raised when the job's underlying object has been deleted mid-transcode."""


QUALITY_RE = re.compile(r"^video_quality_(\d+)p$")


def transcode(
    input_path: str,
    output_dir: str,
    file_type: str,
    params: Optional[dict] = None,
    is_cancelled: Optional[Callable[[], bool]] = None,
    progress_cb: Optional[Callable[[int], None]] = None,
    job_id: Optional[str] = None,
) -> dict:
    """Run the transcoder for one job. Raises JobSkipped if the job doesn't
    apply (e.g. would upscale). progress_cb(0-99) is called periodically for
    long-running jobs (currently: video quality encodes).

    job_id is passed through to the underlying runners so they can register
    their ffmpeg PID into the cancel registry — letting a pub/sub message
    SIGTERM the right process."""
    if not os.path.exists(input_path):
        raise FileNotFoundError(f"input not found: {input_path}")

    os.makedirs(output_dir, exist_ok=True)
    params = params or {}

    # ---- New parallel pipeline -----------------------------------------
    m = QUALITY_RE.match(file_type)
    if m:
        height = int(params.get("height") or m.group(1))
        vbr    = int(params.get("vbr", 3000))
        abr    = int(params.get("abr", 128))
        return transcode_video_quality(input_path, output_dir, height, vbr, abr,
                                       progress_cb=progress_cb, job_id=job_id)

    if file_type == "video_thumbnails":
        return transcode_video_thumbnails(input_path, output_dir, job_id=job_id)

    if file_type == "video_finalize":
        return transcode_video_finalize(input_path, output_dir)

    # ---- Legacy + non-video --------------------------------------------
    if file_type == "video":
        return transcode_video(input_path, output_dir, is_cancelled=is_cancelled, job_id=job_id)
    if file_type == "audio":
        return transcode_audio(input_path, output_dir, job_id=job_id)
    if file_type == "image":
        return transcode_image(input_path, output_dir)

    raise ValueError(f"unsupported file type: {file_type}")
