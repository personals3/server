#!/usr/bin/env bash
# =============================================================================
# Part 5 smoke test — transcoding pipeline
#
# Flow:
#  1. Login → API key
#  2. Generate a 5-second test video using the worker container's ffmpeg
#  3. Upload it
#  4. Verify a transcode_job row exists with status='pending' or 'processing'
#  5. Poll the object's transcode_status until 'done' (or fail after 60s)
#  6. Verify the HLS output files exist on disk
#  7. Verify the master.m3u8 references quality variants
#  8. Image flow: upload a small generated PNG, verify webp output exists
#  9. Cleanup
# =============================================================================

set -euo pipefail

API="${API_URL:-http://localhost:8080/api}"
BUCKET="media-$(date +%s)"

red()   { echo -e "\033[31m$*\033[0m"; }
green() { echo -e "\033[32m$*\033[0m"; }

cleanup() {
  rm -f /tmp/test-video.mp4 /tmp/test-image.png
}
trap cleanup EXIT

# ---------- 1. Auth ---------------------------------------------------------
echo "== 1. Auth setup ==========================================="
JWT=$(curl -s -X POST "$API/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@local","password":"admin"}' \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['token'])")
KEY_RESP=$(curl -s -X POST "$API/auth/keys" \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{"name":"part5-test"}')
APIKEY=$(echo "$KEY_RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])")
KEY_ID=$(echo "$KEY_RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
AUTH="Authorization: Bearer $APIKEY"
green "OK"

# ---------- 2. Generate test video using worker container's ffmpeg ----------
echo "== 2. Generate test video (5s 720p test pattern) =========="
docker compose exec -T worker ffmpeg -y -f lavfi -i testsrc=duration=5:size=1280x720:rate=30 \
  -f lavfi -i sine=frequency=440:duration=5 \
  -c:v libx264 -pix_fmt yuv420p -preset ultrafast -t 5 \
  -c:a aac -b:a 96k \
  /tmp/test.mp4 >/dev/null 2>&1
docker compose cp s3-worker-1:/tmp/test.mp4 /tmp/test-video.mp4 2>/dev/null || \
  docker compose cp worker:/tmp/test.mp4 /tmp/test-video.mp4
ls -la /tmp/test-video.mp4

# ---------- 3. Upload ------------------------------------------------------
echo "== 3. Create bucket + upload video ========================="
curl -sf -X PUT "$API/$BUCKET" -H "$AUTH" >/dev/null
curl -sf -X PUT "$API/$BUCKET/sample.mp4" \
  -H "$AUTH" -H "Content-Type: video/mp4" \
  --data-binary "@/tmp/test-video.mp4" -o /dev/null
green "OK uploaded"

# ---------- 4. Job enqueued ------------------------------------------------
echo "== 4. Verify transcode_job enqueued ========================"
sleep 1
JOB_STATE=$(docker compose exec -T postgres psql -U s3admin -d personals3 -t -A \
  -c "SELECT status FROM transcode_jobs ORDER BY created_at DESC LIMIT 1")
echo "  job state: '$JOB_STATE'"
if [[ "$JOB_STATE" =~ ^(pending|processing|done)$ ]]; then
  green "OK"
else
  red "expected pending/processing/done, got '$JOB_STATE'"
  docker compose logs --tail=30 worker
  exit 1
fi

# ---------- 5. Wait for completion -----------------------------------------
echo "== 5. Wait for transcode (max 60s) ========================="
for i in $(seq 1 60); do
  STATUS=$(curl -s "$API/$BUCKET/sample.mp4" -H "$AUTH" \
    --head -o /dev/null -w "%{http_code}")
  TS=$(docker compose exec -T postgres psql -U s3admin -d personals3 -t -A \
    -c "SELECT transcode_status FROM objects WHERE key='sample.mp4' ORDER BY created_at DESC LIMIT 1")
  echo "  [${i}s] transcode_status = $TS"
  if [ "$TS" = "done" ]; then break; fi
  if [ "$TS" = "failed" ]; then
    red "transcode failed:"
    docker compose exec -T postgres psql -U s3admin -d personals3 \
      -c "SELECT error FROM transcode_jobs ORDER BY created_at DESC LIMIT 1"
    docker compose logs --tail=40 worker
    exit 1
  fi
  sleep 1
done

if [ "$TS" != "done" ]; then
  red "timeout"; docker compose logs --tail=40 worker; exit 1
fi
green "OK transcoded"

# ---------- 6. Check HLS output exists on disk -----------------------------
echo "== 6. Verify HLS files exist ==============================="
OBJECT_ID=$(docker compose exec -T postgres psql -U s3admin -d personals3 -t -A \
  -c "SELECT id FROM objects WHERE key='sample.mp4' ORDER BY created_at DESC LIMIT 1")
SEG_DIR="./storage/segments/$OBJECT_ID"
echo "  segments dir: $SEG_DIR"
ls -la "$SEG_DIR" | head -10

if [ -f "$SEG_DIR/master.m3u8" ]; then
  green "OK master.m3u8 present"
else
  red "master.m3u8 missing"; exit 1
fi

VARIANT_COUNT=$(find "$SEG_DIR" -name "playlist.m3u8" | wc -l)
echo "  variant playlists: $VARIANT_COUNT"
[ "$VARIANT_COUNT" -ge 1 ] && green "OK $VARIANT_COUNT quality variants" || { red "no variants"; exit 1; }

SEGMENT_COUNT=$(find "$SEG_DIR" -name "segment_*.ts" | wc -l)
echo "  total .ts segments: $SEGMENT_COUNT"
[ "$SEGMENT_COUNT" -ge 1 ] && green "OK $SEGMENT_COUNT segments" || { red "no segments"; exit 1; }

THUMB_COUNT=$(find "$SEG_DIR" -name "thumb_*.jpg" | wc -l)
echo "  thumbnails: $THUMB_COUNT"

# ---------- 7. master.m3u8 content check ----------------------------------
echo "== 7. Verify master.m3u8 lists variants ===================="
cat "$SEG_DIR/master.m3u8" | sed 's/^/  | /'
grep -q "EXT-X-STREAM-INF" "$SEG_DIR/master.m3u8" && green "OK contains stream variants" || { red "missing"; exit 1; }

# ---------- 8. Image transcode --------------------------------------------
echo "== 8. Image transcode test ================================="
docker compose exec -T worker python3 -c "
from PIL import Image
img = Image.new('RGB', (1024, 768), color=(80, 130, 200))
img.save('/tmp/test.png', format='PNG')
"
docker compose cp s3-worker-1:/tmp/test.png /tmp/test-image.png 2>/dev/null || \
  docker compose cp worker:/tmp/test.png /tmp/test-image.png

curl -sf -X PUT "$API/$BUCKET/sample.png" \
  -H "$AUTH" -H "Content-Type: image/png" \
  --data-binary "@/tmp/test-image.png" -o /dev/null

# Wait up to 30s
for i in $(seq 1 30); do
  ITS=$(docker compose exec -T postgres psql -U s3admin -d personals3 -t -A \
    -c "SELECT transcode_status FROM objects WHERE key='sample.png' ORDER BY created_at DESC LIMIT 1")
  if [ "$ITS" = "done" ]; then break; fi
  if [ "$ITS" = "failed" ]; then
    red "image transcode failed"; docker compose logs --tail=30 worker; exit 1
  fi
  sleep 1
done

IMG_OBJECT_ID=$(docker compose exec -T postgres psql -U s3admin -d personals3 -t -A \
  -c "SELECT id FROM objects WHERE key='sample.png' ORDER BY created_at DESC LIMIT 1")
IMG_DIR="./storage/segments/$IMG_OBJECT_ID"
if [ -f "$IMG_DIR/original.webp" ]; then
  WEBP_SIZE=$(stat -c %s "$IMG_DIR/original.webp")
  green "OK image transcoded to WebP ($WEBP_SIZE bytes)"
  ls "$IMG_DIR"
else
  red "WebP output missing"; ls "$IMG_DIR" 2>/dev/null; exit 1
fi

# ---------- 9. Cleanup ----------------------------------------------------
echo "== 9. Cleanup =============================================="
curl -sf -X DELETE "$API/$BUCKET/sample.mp4" -H "$AUTH" -o /dev/null
curl -sf -X DELETE "$API/$BUCKET/sample.png" -H "$AUTH" -o /dev/null
curl -sf -X DELETE "$API/$BUCKET" -H "$AUTH" -o /dev/null
curl -sf -X DELETE "$API/auth/keys/$KEY_ID" -H "Authorization: Bearer $JWT" -o /dev/null
green "OK"

echo
green "All Part 5 tests passed."
