"""Audio → HLS audio + MP3 + OGG transcoding."""

from __future__ import annotations

import logging
import os

from .video import ffprobe, run

log = logging.getLogger(__name__)


def transcode_audio(input_path: str, output_dir: str, job_id: str | None = None) -> dict:
    os.makedirs(output_dir, exist_ok=True)
    probe = ffprobe(input_path)
    duration = float(probe.get("format", {}).get("duration", 0))

    # HLS audio (for browser streaming)
    run([
        "ffmpeg", "-y", "-i", input_path,
        "-vn",                           # no video
        "-c:a", "aac", "-b:a", "128k",
        "-f", "hls",
        "-hls_time", "10",
        "-hls_playlist_type", "vod",
        "-hls_segment_filename", os.path.join(output_dir, "audio_%03d.aac"),
        os.path.join(output_dir, "audio.m3u8"),
    ], "audio HLS", job_id=job_id)

    # MP3 320 kbps (universal compatibility)
    run([
        "ffmpeg", "-y", "-i", input_path,
        "-vn", "-c:a", "libmp3lame", "-b:a", "320k",
        os.path.join(output_dir, "audio_320.mp3"),
    ], "audio MP3 320", job_id=job_id)

    # OGG Vorbis (open format)
    run([
        "ffmpeg", "-y", "-i", input_path,
        "-vn", "-c:a", "libvorbis", "-q:a", "6",
        os.path.join(output_dir, "audio.ogg"),
    ], "audio OGG", job_id=job_id)

    return {
        "type": "audio",
        "hls": "audio.m3u8",
        "mp3": "audio_320.mp3",
        "ogg": "audio.ogg",
        "duration_seconds": duration,
    }
