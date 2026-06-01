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

    # HLS audio for streaming-mode playback.
    #
    # -hls_time 4: 4-second segments. Shorter than the usual 10s because:
    #   - First-byte-to-first-sample latency = ~ one segment.
    #     Long audio (audiobook, concert) starts ~6s sooner with 4s segments.
    #   - Segments are tiny (~65 KB at 128 kbps) — small enough that re-fetching
    #     after a network blip is cheap.
    #   - HLS recommendation for VOD is 6s; 4s is on the aggressive side but
    #     still well within player tolerance.
    # -hls_list_size 0 + -hls_playlist_type vod: complete playlist, all segments
    #   listed, browser can seek anywhere immediately.
    # -map 0:a:0: pick the FIRST audio stream explicitly. Cover-art-only files
    #   reach this code path via hasRealVideo() routing on the API side, so
    #   we shouldn't see them, but being explicit is cheap insurance against
    #   accidentally picking an attached_pic video stream.
    run([
        "ffmpeg", "-y", "-i", input_path,
        "-map", "0:a:0",
        "-vn",                           # belt + suspenders: no video out
        "-c:a", "aac", "-b:a", "128k",
        "-f", "hls",
        "-hls_time", "4",
        "-hls_list_size", "0",
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
