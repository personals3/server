"""Image → WebP + AVIF + responsive thumbnails (via Pillow + ffmpeg fallback)."""

from __future__ import annotations

import logging
import os
import subprocess

from PIL import Image, ImageOps

log = logging.getLogger(__name__)

# (suffix, max_width) — thumbnail sizes
THUMBS = [("100", 100), ("300", 300), ("800", 800)]


def transcode_image(input_path: str, output_dir: str) -> dict:
    os.makedirs(output_dir, exist_ok=True)

    img = Image.open(input_path)
    # Apply EXIF rotation so portrait phone photos look right.
    img = ImageOps.exif_transpose(img)
    # Convert to RGB if needed (WebP/AVIF require RGB or RGBA, not P or CMYK).
    if img.mode not in ("RGB", "RGBA"):
        img = img.convert("RGBA" if "A" in img.mode else "RGB")

    width, height = img.size

    # Full-resolution WebP (quality 85)
    webp_path = os.path.join(output_dir, "original.webp")
    img.save(webp_path, format="WEBP", quality=85, method=4)

    # AVIF via ffmpeg (Pillow's AVIF support requires pillow-heif which is heavy)
    avif_path = os.path.join(output_dir, "original.avif")
    avif_ok = False
    try:
        subprocess.run(
            [
                "ffmpeg", "-y", "-i", input_path,
                "-c:v", "libaom-av1", "-still-picture", "1",
                "-cpu-used", "6", "-crf", "30",
                avif_path,
            ],
            capture_output=True, check=True,
        )
        avif_ok = True
    except subprocess.CalledProcessError as e:
        log.warning("AVIF encode failed (codec missing?), skipping: %s",
                    e.stderr.decode("utf-8", "ignore")[-200:])
        avif_path = None

    # Responsive thumbnails (WebP)
    thumbs = []
    for suffix, max_w in THUMBS:
        if max_w >= width:
            continue
        thumb_name = f"thumb_{suffix}.webp"
        t = img.copy()
        t.thumbnail((max_w, max_w * 10))  # cap width; allow tall portraits
        t.save(os.path.join(output_dir, thumb_name),
               format="WEBP", quality=80, method=4)
        thumbs.append({"suffix": suffix, "width": t.size[0], "file": thumb_name})

    return {
        "type": "image",
        "source_width": width,
        "source_height": height,
        "webp": "original.webp",
        "avif": "original.avif" if avif_ok else None,
        "thumbnails": thumbs,
    }
