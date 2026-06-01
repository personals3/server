# =============================================================================
# Python transcoding worker — FFmpeg + Pillow
#
# We use the official python:3.12-slim-bookworm base and install ffmpeg from
# Debian. Debian's ffmpeg includes libx264, AAC, libmp3lame, libvorbis, and
# libaom-av1.
#
# IMPORTANT: pinned to bookworm intentionally. Trixie ships ffmpeg 7.1.x
# built with libplacebo, whose SIMD dispatcher segfaults on pre-AVX2 CPUs
# (AMD Mullins/Beema/Puma+ — e.g. A6-7310, our current host). Bookworm's
# ffmpeg 5.1.x runs fine on the same hardware. When the host moves to a
# mainstream AMD/Intel CPU with AVX2, this can go back to plain `slim`.
# =============================================================================

FROM python:3.12-slim-bookworm

# System deps: ffmpeg + libaom (AV1/AVIF) + image libs Pillow needs.
# Plus VA-API for hardware video encoding on Intel iGPU / AMD APU.
#
# Driver packages we try (only those present on the running distro install):
#   - intel-media-va-driver  — Intel Gen 8+ (Broadwell and later)
#   - i965-va-driver         — Intel Gen 4-7 (older)
#   - mesa-va-drivers        — AMD / AMDGPU
#
# We install drivers in a second pass so any missing package doesn't fail
# the whole layer. The loop tries both `-non-free` and plain names so it
# keeps working across Debian releases.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg \
        libjpeg62-turbo \
        zlib1g \
        libpng16-16 \
        libtiff6 \
        libwebp7 \
        ca-certificates \
        libva2 \
        libva-drm2 \
        vainfo \
    && for pkg in intel-media-va-driver intel-media-va-driver-non-free \
                  i965-va-driver mesa-va-drivers; do \
         apt-get install -y --no-install-recommends "$pkg" 2>/dev/null \
           && echo "installed: $pkg" \
           || echo "skip: $pkg not available"; \
       done \
    && rm -rf /var/lib/apt/lists/* \
    && ffmpeg -version | head -1 \
    && (vainfo 2>&1 | head -5 || true)

WORKDIR /app

# Install Python deps first so this layer caches across code changes.
COPY pyproject.toml ./
RUN pip install --no-cache-dir \
        "psycopg[binary]>=3.2" \
        "Pillow>=10.4" \
        "redis>=5.0"

# Copy worker code
COPY worker/ ./worker/

# Run as the host UID/GID (set in compose) so /storage permissions match.
ENTRYPOINT ["python", "-m", "worker.main"]
