"""Video transcoding — split into per-quality + thumbnails + finalize.

Encoding path: tries VA-API hardware encoding first (Intel iGPU / AMD APU on
Linux when /dev/dri is mounted into the container), falls back to libx264
on the CPU if GPU encode fails for any reason.

Set USE_GPU=0 in the worker's env to force CPU.
"""

from __future__ import annotations

import json
import logging
import os
import subprocess
from typing import Callable, Optional

log = logging.getLogger(__name__)

# Used by the legacy monolithic path only. The new per-quality pipeline
# receives height/vbr/abr from the API enqueue side, which now builds the
# ladder dynamically based on the source's actual resolution (see
# api/internal/handlers/transcode.go: PickLadder).
LADDER = [
    (1080, 5000, 192),
    (720, 2800, 128),
    (480, 1200, 96),
    (360, 600, 64),
]

# ---- Hardware encoder detection (module-level, cached at import) ----------

USE_GPU = os.getenv("USE_GPU", "1") != "0"
FFMPEG_THREADS = int(os.getenv("FFMPEG_THREADS", "4"))
GPU_RENDER_NODE = "/dev/dri/renderD128"   # standard for the first GPU


def _probe_vaapi() -> bool:
    """Quick check whether VA-API is usable in this container."""
    if not USE_GPU:
        log.info("GPU encoding disabled by USE_GPU=0")
        return False
    if not os.path.exists(GPU_RENDER_NODE):
        log.info("VA-API: %s not present — CPU encoding only", GPU_RENDER_NODE)
        return False
    try:
        # vainfo prints what hardware is available
        res = subprocess.run(
            ["vainfo", "--display", "drm", "--device", GPU_RENDER_NODE],
            capture_output=True, text=True, timeout=5,
        )
        # Look for any H.264 encode profile in the output
        if "H264" in res.stdout and "EncSlice" in res.stdout:
            log.info("VA-API: hardware H.264 encode AVAILABLE")
            return True
        log.info("VA-API: device present but no H.264 encode profile — CPU fallback")
        log.debug("vainfo: %s", res.stdout[:1000])
        return False
    except Exception as e:
        log.info("VA-API probe failed (%s) — CPU encoding only", e)
        return False


HAS_VAAPI = _probe_vaapi()


# ----- helpers --------------------------------------------------------------

def ffprobe(path: str) -> dict:
    # -v error keeps stdout JSON-clean while routing the ACTUAL failure
    # reason (missing file, codec mismatch, truncated mp4, etc) to stderr.
    # We hand-check returncode so we can attach stderr to the exception —
    # the old `check=True` raised a CalledProcessError whose str() only
    # showed the command + exit code, hiding the useful bit.
    out = subprocess.run(
        ["ffprobe", "-v", "error",
         "-print_format", "json",
         "-show_format", "-show_streams",
         path],
        capture_output=True, check=False,
    )
    if out.returncode != 0:
        stderr = out.stderr.decode("utf-8", errors="replace").strip()
        # Include path so the DB row in transcode_jobs.error tells the operator
        # exactly which file ffprobe failed on, not just the command shape.
        raise RuntimeError(
            f"ffprobe exit {out.returncode} on {path}: "
            f"{stderr or '<no stderr>'}"
        )
    return json.loads(out.stdout)


def run(cmd: list[str], label: str,
        progress_cb: Optional[Callable[[int], None]] = None,
        duration_sec: float = 0,
        job_id: Optional[str] = None) -> None:
    """Run ffmpeg. If progress_cb + duration provided, pipe `-progress` so we
    can call progress_cb(pct) every second.

    When job_id is given, the subprocess is registered with the cancel
    registry so a pub/sub cancel message can SIGTERM it instantly. Also
    writes the PID into transcode_jobs.pid for visibility (best-effort, via
    optional Database hook — skipped if no env var is set)."""
    from .cancel import registry as _cancel_registry
    log.info("ffmpeg %s: %s", label, " ".join(cmd[:6]) + " ...")

    if progress_cb is None or duration_sec <= 0:
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        if job_id:
            _cancel_registry.set_process(job_id, proc)
            _write_pid_best_effort(job_id, proc.pid)
        try:
            _, stderr = proc.communicate()
            if proc.returncode != 0:
                tail = "\n".join((stderr or "").strip().splitlines()[-15:])
                raise RuntimeError(f"ffmpeg {label} failed (rc={proc.returncode}):\n{tail}")
        finally:
            if job_id:
                _cancel_registry.clear_process(job_id)
        return

    # Streaming mode — read stdout for `-progress pipe:1` k=v lines.
    cmd = cmd + ["-progress", "pipe:1", "-nostats", "-loglevel", "error"]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if job_id:
        _cancel_registry.set_process(job_id, proc)
        _write_pid_best_effort(job_id, proc.pid)

    last_emit = 0.0
    stderr_buf: list[str] = []

    try:
        assert proc.stdout is not None
        for line in proc.stdout:
            line = line.rstrip()
            # ffmpeg emits microseconds of processed output as `out_time_us=N`
            if line.startswith("out_time_us="):
                try:
                    us = int(line.split("=", 1)[1])
                except ValueError:
                    continue
                processed_sec = us / 1_000_000
                pct = int(min(99, (processed_sec / duration_sec) * 100))
                # Throttle DB writes to once per second
                import time as _t
                now = _t.time()
                if now - last_emit >= 1.0:
                    try: progress_cb(pct)
                    except Exception: pass
                    last_emit = now
    finally:
        # Drain stderr for error reporting
        if proc.stderr is not None:
            stderr_buf.append(proc.stderr.read() or "")
        proc.wait()
        if job_id:
            _cancel_registry.clear_process(job_id)

    if proc.returncode != 0:
        err = "".join(stderr_buf).strip().splitlines()[-15:]
        raise RuntimeError(f"ffmpeg {label} failed (rc={proc.returncode}):\n" + "\n".join(err))


# --- best-effort PID writeback ---------------------------------------------
# Stores ffmpeg's PID into transcode_jobs.pid so admin tooling / cleaner can
# see what's actually running. Soft-fails on any DB error — non-essential.

def _write_pid_best_effort(job_id: str, pid: int) -> None:
    dsn = os.environ.get("DATABASE_URL")
    if not dsn:
        return
    try:
        import psycopg
        with psycopg.connect(dsn, autocommit=True) as conn:
            with conn.cursor() as cur:
                cur.execute(
                    "UPDATE transcode_jobs SET pid = %s WHERE id = %s",
                    (pid, job_id),
                )
    except Exception as e:
        log.debug("write pid failed (non-fatal): %s", e)


def _probe_summary(input_path: str):
    """Returns (input_height, duration_seconds, has_audio)."""
    probe = ffprobe(input_path)
    video_stream = next((s for s in probe["streams"] if s["codec_type"] == "video"), None)
    if not video_stream:
        raise ValueError("no video stream in input")
    input_h = int(video_stream.get("height") or 0)
    duration = float(probe.get("format", {}).get("duration", 0))
    has_audio = any(s["codec_type"] == "audio" for s in probe["streams"])
    return input_h, duration, has_audio


# ----- NEW: single-quality job ---------------------------------------------

class JobSkipped(Exception):
    """Raised to indicate this quality is not applicable (e.g. would upscale)."""


def transcode_video_quality(
    input_path: str,
    output_dir: str,
    height: int,
    vbr: int,
    abr: int,
    progress_cb: Optional[Callable[[int], None]] = None,
    job_id: Optional[str] = None,
) -> dict:
    """Render ONE HLS quality variant. Designed to run as its own job in parallel
    with other quality jobs on other workers.

    Raises JobSkipped if the source is smaller than the target — we never
    upscale; the finalize step will just not include this height in the
    master playlist.

    progress_cb(int 0-99) is called every ~1 second while ffmpeg is running.
    """
    input_h, duration, has_audio = _probe_summary(input_path)
    if input_h > 0 and height > input_h:
        raise JobSkipped(f"source is {input_h}p; not upscaling to {height}p")

    os.makedirs(output_dir, exist_ok=True)
    q_dir = os.path.join(output_dir, f"{height}p")
    os.makedirs(q_dir, exist_ok=True)

    # Try GPU first; fall back to CPU on any error.
    if HAS_VAAPI:
        try:
            _ffmpeg_vaapi(input_path, q_dir, height, vbr, abr, has_audio,
                          duration, progress_cb, job_id=job_id)
            log.info("video %dp: GPU encode succeeded", height)
            return _quality_result(height, vbr, abr, has_audio)
        except Exception as e:
            log.warning("video %dp: GPU encode failed (%s); falling back to CPU",
                        height, e)

    _ffmpeg_cpu(input_path, q_dir, height, vbr, abr, has_audio,
                duration, progress_cb, job_id=job_id)
    return _quality_result(height, vbr, abr, has_audio)


def _quality_result(height: int, vbr: int, abr: int, has_audio: bool) -> dict:
    return {
        "height": height,
        "video_bitrate_kbps": vbr,
        "audio_bitrate_kbps": abr if has_audio else 0,
        "playlist": f"{height}p/playlist.m3u8",
    }


def _ffmpeg_cpu(input_path, q_dir, height, vbr, abr, has_audio, duration, progress_cb, job_id=None):
    """Software libx264 encode. Reliable but ~1x realtime."""
    cmd = [
        "ffmpeg", "-y",
        "-threads", str(FFMPEG_THREADS),
        "-i", input_path,
        "-map", "0:v:0",
    ]
    if has_audio:
        cmd += ["-map", "0:a:0?"]
    cmd += [
        "-vf", f"scale=-2:{height}",
        "-c:v", "libx264",
        "-preset", "fast",
        "-b:v", f"{vbr}k",
        "-maxrate", f"{int(vbr * 1.07)}k",
        "-bufsize", f"{vbr * 2}k",
        "-profile:v", "main",
        "-g", "60",
        "-sc_threshold", "0",
    ]
    if has_audio:
        cmd += ["-c:a", "aac", "-b:a", f"{abr}k", "-ac", "2"]
    else:
        cmd += ["-an"]
    cmd += _hls_output(q_dir)
    run(cmd, f"video {height}p (CPU)", progress_cb=progress_cb, duration_sec=duration, job_id=job_id)


def _ffmpeg_vaapi(input_path, q_dir, height, vbr, abr, has_audio, duration, progress_cb, job_id=None):
    """VA-API hardware encode. 5-20x realtime on Intel iGPU / AMD APU.

    Uses Constant Quantization Parameter (CQP) rate control — the most
    universally supported VA-API mode. Some older Intel drivers (notably
    i965/Gen7) only support CQP and reject -b:v with "supported modes: CQP".

    Higher qp = smaller file, lower quality. 22-28 is the visually-good range.
    We pick qp based on target height so smaller renditions get more compression.
    """
    qp = _qp_for_height(height)
    cmd = [
        "ffmpeg", "-y",
        # Hardware context
        "-init_hw_device", f"vaapi=va:{GPU_RENDER_NODE}",
        "-hwaccel", "vaapi",
        "-hwaccel_device", "va",
        "-hwaccel_output_format", "vaapi",
        "-i", input_path,
        "-map", "0:v:0",
    ]
    if has_audio:
        cmd += ["-map", "0:a:0?"]
    cmd += [
        # Decode result lives in GPU memory; if not, hwupload moves it there
        "-vf", f"format=nv12|vaapi,hwupload,scale_vaapi=w=-2:h={height}",
        "-c:v", "h264_vaapi",
        "-rc_mode", "CQP",         # constant quantization — works on all VAAPI drivers
        "-qp", str(qp),
        "-profile:v", "main",
        "-g", "60",
    ]
    if has_audio:
        cmd += ["-c:a", "aac", "-b:a", f"{abr}k", "-ac", "2"]
    else:
        cmd += ["-an"]
    cmd += _hls_output(q_dir)
    run(cmd, f"video {height}p (GPU/VAAPI qp={qp})",
        progress_cb=progress_cb, duration_sec=duration, job_id=job_id)


# Canonical rungs used to fill in (vbr, abr) for the master playlist when the
# original enqueue params aren't available at finalize time. Must stay roughly
# in sync with api/internal/handlers/transcode.go: CanonicalLadder.
_BITRATE_RUNGS = [
    (4320, 50000, 320),
    (2160, 18000, 256),
    (1440, 10000, 192),
    (1080,  5000, 192),
    (720,   2800, 128),
    (480,   1200,  96),
    (360,    600,  64),
    (240,    400,  64),
]


def _bitrates_for_height(height: int) -> tuple[int, int]:
    """Pick (video_kbps, audio_kbps) for an arbitrary height.

    Exact rung match wins; otherwise we pick the nearest rung above and
    scale video bitrate by pixel-area ratio so a custom 1200p sits between
    1080p and 1440p reasonably."""
    for h, v, a in _BITRATE_RUNGS:
        if height == h:
            return v, a
    for h, v, a in _BITRATE_RUNGS:
        if height > h:
            ratio = (height * height) / (h * h)
            return int(v * ratio), a
    return 400, 64


def _qp_for_height(height: int) -> int:
    """Map target height → VA-API QP. Lower QP = higher quality + bigger file.

    Extended for 1440p/2160p/4320p rungs introduced by the dynamic ladder.
    Bigger frames already get more bits per QP step, so we keep QPs flat at
    the high end and only lower them slightly for the very biggest frames
    to preserve detail."""
    if height >= 4320: return 20   # 8K — preserve detail
    if height >= 2160: return 21   # 4K
    if height >= 1440: return 21   # QHD
    if height >= 1080: return 22
    if height >= 720:  return 23
    if height >= 480:  return 25
    return 27   # 360p and below — aggressive compression


def _hls_output(q_dir):
    return [
        "-f", "hls",
        "-hls_time", "6",
        "-hls_playlist_type", "vod",
        "-hls_segment_filename", os.path.join(q_dir, "segment_%03d.ts"),
        os.path.join(q_dir, "playlist.m3u8"),
    ]

    return {
        "height": height,
        "video_bitrate_kbps": vbr,
        "audio_bitrate_kbps": abr if has_audio else 0,
        "playlist": f"{height}p/playlist.m3u8",
    }


# ----- NEW: thumbnails job --------------------------------------------------

def transcode_video_thumbnails(input_path: str, output_dir: str, job_id: Optional[str] = None) -> dict:
    """Extract thumbnails at 0%, 25%, 50%, 75% of duration. Runs in parallel
    with the quality jobs — small and fast (3-5s each)."""
    _input_h, duration, _ = _probe_summary(input_path)
    os.makedirs(output_dir, exist_ok=True)

    thumbs = []
    if duration > 0:
        for i, frac in enumerate([0.0, 0.25, 0.5, 0.75]):
            t = max(0.1, duration * frac)
            thumb_name = f"thumb_{i}.jpg"
            run([
                "ffmpeg", "-y",
                "-ss", f"{t:.2f}", "-i", input_path,
                "-map", "0:v:0",            # only video stream
                "-frames:v", "1",
                "-update", "1",             # single-image output (modern ffmpeg)
                "-q:v", "3",
                "-vf", "scale='min(640,iw)':-2",
                os.path.join(output_dir, thumb_name),
            ], f"thumb_{i}", job_id=job_id)
            thumbs.append(thumb_name)
    return {"thumbnails": thumbs, "duration_seconds": duration}


# ----- NEW: finalize job ----------------------------------------------------

def transcode_video_finalize(input_path: str, output_dir: str) -> dict:
    """Scan output_dir for whichever qualities + thumbs the sibling jobs
    produced, write master.m3u8 referencing them, return the aggregated
    transcoded metadata. Runs only after all siblings are done."""
    input_h, duration, _ = _probe_summary(input_path)

    qualities = []
    if os.path.isdir(output_dir):
        # Sort by numeric height, NOT string — otherwise "1080p" sorts
        # before "240p" alphabetically and ends up in the wrong slot.
        # Highest quality first so master.m3u8 picks 1080p as the top variant.
        entries = [
            e for e in os.listdir(output_dir)
            if e.endswith("p") and e[:-1].isdigit()
        ]
        entries.sort(key=lambda e: int(e[:-1]), reverse=True)
        for entry in entries:
            playlist = os.path.join(output_dir, entry, "playlist.m3u8")
            if not os.path.isfile(playlist):
                continue
            height = int(entry[:-1])
            vbr, abr = _bitrates_for_height(height)
            qualities.append({
                "height": height,
                "video_bitrate_kbps": vbr,
                "audio_bitrate_kbps": abr,
                "playlist": f"{entry}/playlist.m3u8",
            })

    # Master playlist
    master_path = os.path.join(output_dir, "master.m3u8")
    lines = ["#EXTM3U", "#EXT-X-VERSION:3"]
    for q in qualities:
        approx_w = (q["height"] * 16) // 9
        bw_bps = (q["video_bitrate_kbps"] + q["audio_bitrate_kbps"]) * 1000
        lines.append(
            f"#EXT-X-STREAM-INF:BANDWIDTH={bw_bps},RESOLUTION={approx_w}x{q['height']}"
        )
        lines.append(q["playlist"])
    with open(master_path, "w") as f:
        f.write("\n".join(lines) + "\n")

    # Collect thumbs already on disk
    thumbs = []
    if os.path.isdir(output_dir):
        for f in sorted(os.listdir(output_dir)):
            if f.startswith("thumb_") and f.endswith(".jpg"):
                thumbs.append(f)

    return {
        "type": "video",
        "master": "master.m3u8",
        "qualities": qualities,
        "thumbnails": thumbs,
        "duration_seconds": duration,
        "source_height": input_h,
    }


# ----- LEGACY: monolithic path (kept for backwards compat) -----------------

def transcode_video(
    input_path: str,
    output_dir: str,
    is_cancelled: Optional[Callable[[], bool]] = None,
    job_id: Optional[str] = None,
) -> dict:
    """The original sequential 4-quality pipeline. Used when the API enqueues
    an old-style 'video' job (no group_id). New uploads use the parallel
    pipeline via transcode_video_quality + transcode_video_finalize."""
    from .transcoder import JobCancelled
    def check_cancelled():
        if is_cancelled and is_cancelled():
            raise JobCancelled("object was deleted mid-transcode")

    input_h, duration, has_audio = _probe_summary(input_path)
    os.makedirs(output_dir, exist_ok=True)

    rungs = [r for r in LADDER if r[0] <= input_h] or [LADDER[-1]]
    log.info("monolithic transcode to %d quality levels: %s",
             len(rungs), ",".join(f"{h}p" for h, *_ in rungs))

    qualities = []
    for height, vbr, abr in rungs:
        check_cancelled()
        q = transcode_video_quality(input_path, output_dir, height, vbr, abr)
        qualities.append(q)

    # Thumbnails
    thumb_meta = transcode_video_thumbnails(input_path, output_dir)

    # Master playlist
    master_path = os.path.join(output_dir, "master.m3u8")
    lines = ["#EXTM3U", "#EXT-X-VERSION:3"]
    for q in qualities:
        approx_w = (q["height"] * 16) // 9
        bw_bps = (q["video_bitrate_kbps"] + q["audio_bitrate_kbps"]) * 1000
        lines.append(
            f"#EXT-X-STREAM-INF:BANDWIDTH={bw_bps},RESOLUTION={approx_w}x{q['height']}"
        )
        lines.append(q["playlist"])
    with open(master_path, "w") as f:
        f.write("\n".join(lines) + "\n")

    return {
        "type": "video",
        "master": "master.m3u8",
        "qualities": qualities,
        "thumbnails": thumb_meta["thumbnails"],
        "duration_seconds": duration,
        "source_width": 0,
        "source_height": input_h,
    }
