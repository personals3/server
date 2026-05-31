#!/usr/bin/env bash
# =============================================================================
# Part 6 smoke test — nginx static HLS serving + proxy
#
# Verifies:
#   1. Nginx /nginx-health returns 200
#   2. Nginx proxies API requests (login through nginx works)
#   3. Upload a video through nginx (proxied to API → transcoded by worker)
#   4. Fetch master.m3u8 through nginx /stream/{id}/master.m3u8 directly
#      → no auth (public HLS), CORS headers present, Cache-Control set
#   5. Fetch a .ts segment → cached forever, correct MIME
# =============================================================================

set -euo pipefail

NGINX="${NGINX_URL:-http://localhost:8080}"
API="${NGINX}/api"
BUCKET="hls-$(date +%s)"

red()   { echo -e "\033[31m$*\033[0m"; }
green() { echo -e "\033[32m$*\033[0m"; }

cleanup() {
  rm -f /tmp/test-video.mp4
}
trap cleanup EXIT

# ---------- 1. Nginx health -------------------------------------------------
echo "== 1. Nginx health ============================================"
RESP=$(curl -s "$NGINX/nginx-health")
[ "$RESP" = "ok" ] && green "OK" || { red "got: $RESP"; exit 1; }

# ---------- 2. API proxy via nginx -----------------------------------------
echo "== 2. Login via nginx proxy ==================================="
JWT=$(curl -s -X POST "$API/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@local","password":"admin"}' \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['token'])")
[ -n "$JWT" ] && green "OK got JWT through nginx" || { red "login failed"; exit 1; }

KEY_RESP=$(curl -s -X POST "$API/auth/keys" \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{"name":"part6-test"}')
APIKEY=$(echo "$KEY_RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])")
KEY_ID=$(echo "$KEY_RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
AUTH="Authorization: Bearer $APIKEY"

# ---------- 3. Upload through nginx ----------------------------------------
echo "== 3. Create bucket + upload video through nginx ============="
docker compose exec -T worker ffmpeg -y -f lavfi -i testsrc=duration=5:size=1280x720:rate=30 \
  -f lavfi -i sine=frequency=440:duration=5 \
  -c:v libx264 -pix_fmt yuv420p -preset ultrafast -t 5 \
  -c:a aac -b:a 96k \
  /tmp/test.mp4 >/dev/null 2>&1
docker compose cp s3-worker-1:/tmp/test.mp4 /tmp/test-video.mp4 2>/dev/null || \
  docker compose cp worker:/tmp/test.mp4 /tmp/test-video.mp4

curl -sf -X PUT "$API/$BUCKET" -H "$AUTH" >/dev/null
curl -sf -X PUT "$API/$BUCKET/sample.mp4" \
  -H "$AUTH" -H "Content-Type: video/mp4" \
  --data-binary "@/tmp/test-video.mp4" -o /dev/null
green "OK uploaded $(stat -c %s /tmp/test-video.mp4) bytes through nginx"

# ---------- 4. Wait for transcode -------------------------------------------
echo "== 4. Wait for transcode ======================================"
for i in $(seq 1 60); do
  TS=$(docker compose exec -T postgres psql -U s3admin -d personals3 -t -A \
    -c "SELECT transcode_status FROM objects WHERE key='sample.mp4' ORDER BY created_at DESC LIMIT 1")
  if [ "$TS" = "done" ]; then break; fi
  if [ "$TS" = "failed" ]; then red "transcode failed"; exit 1; fi
  sleep 1
done
[ "$TS" = "done" ] && green "OK transcoded in ~${i}s" || { red "timeout"; exit 1; }

OBJECT_ID=$(docker compose exec -T postgres psql -U s3admin -d personals3 -t -A \
  -c "SELECT id FROM objects WHERE key='sample.mp4' ORDER BY created_at DESC LIMIT 1")
echo "  object_id = $OBJECT_ID"

# ---------- 5. Fetch master.m3u8 through nginx (no auth) -------------------
echo "== 5. GET /stream/{id}/master.m3u8 ============================"
HEADERS=$(curl -s -i "$NGINX/stream/$OBJECT_ID/master.m3u8" | tr -d '\r')
echo "$HEADERS" | head -15
STATUS=$(echo "$HEADERS" | head -1 | awk '{print $2}')
[ "$STATUS" = "200" ] && green "OK 200" || { red "got $STATUS"; exit 1; }

# Check headers
echo "$HEADERS" | grep -qi "content-type: application/vnd.apple.mpegurl" \
  && green "OK MIME type application/vnd.apple.mpegurl" \
  || red "WRONG MIME type"

echo "$HEADERS" | grep -qi "access-control-allow-origin: \*" \
  && green "OK CORS header present" \
  || { red "missing CORS"; exit 1; }

echo "$HEADERS" | grep -qi "cache-control: public" \
  && green "OK Cache-Control set" \
  || red "no cache header"

# Body check
BODY=$(curl -s "$NGINX/stream/$OBJECT_ID/master.m3u8")
echo "$BODY" | grep -q "EXT-X-STREAM-INF" \
  && green "OK master.m3u8 has stream variants" \
  || { red "no variants"; exit 1; }

# ---------- 6. Fetch a .ts segment -----------------------------------------
echo "== 6. GET a .ts segment ======================================="
# Read variant playlist to find segment names
VARIANT_LINE=$(echo "$BODY" | grep -v '^#' | head -1)
echo "  variant playlist: $VARIANT_LINE"
VARIANT_PATH=$(echo "$VARIANT_LINE" | head -1)
VARIANT_BODY=$(curl -s "$NGINX/stream/$OBJECT_ID/$VARIANT_PATH")
SEGMENT=$(echo "$VARIANT_BODY" | grep '\.ts$' | head -1)
SEGMENT_URL="/stream/$OBJECT_ID/$(dirname "$VARIANT_PATH")/$SEGMENT"
echo "  fetching $SEGMENT_URL"

SEG_HEADERS=$(curl -s -I "$NGINX$SEGMENT_URL" | tr -d '\r')
echo "$SEG_HEADERS" | head -6
echo "$SEG_HEADERS" | grep -qi "content-type: video/mp2t" \
  && green "OK video/mp2t MIME" \
  || red "wrong MIME"
echo "$SEG_HEADERS" | grep -qi "cache-control: public, max-age=31536000, immutable" \
  && green "OK immutable cache (1 year)" \
  || red "wrong cache"

SEG_SIZE=$(echo "$SEG_HEADERS" | grep -i 'content-length' | awk '{print $2}')
echo "  segment size: $SEG_SIZE bytes"

# ---------- 7. CORS preflight ----------------------------------------------
echo "== 7. CORS OPTIONS preflight =================================="
PRE=$(curl -s -i -X OPTIONS "$NGINX/stream/$OBJECT_ID/master.m3u8" \
  -H "Origin: https://example.com" \
  -H "Access-Control-Request-Method: GET" \
  | tr -d '\r')
echo "$PRE" | head -8
STATUS=$(echo "$PRE" | head -1 | awk '{print $2}')
[ "$STATUS" = "204" ] && green "OK 204 No Content" || { red "got $STATUS"; exit 1; }

# ---------- 8. Cleanup -----------------------------------------------------
echo "== 8. Cleanup ================================================="
curl -sf -X DELETE "$API/$BUCKET/sample.mp4" -H "$AUTH" -o /dev/null
curl -sf -X DELETE "$API/$BUCKET" -H "$AUTH" -o /dev/null
curl -sf -X DELETE "$API/auth/keys/$KEY_ID" -H "Authorization: Bearer $JWT" -o /dev/null
green "OK"

echo
green "All Part 6 tests passed."
echo
echo "You can now play the HLS stream in a browser:"
echo "  https://hls-js.netlify.app/demo/?src=$NGINX/stream/{object-id}/master.m3u8"
echo "Or with ffplay:"
echo "  ffplay $NGINX/stream/{object-id}/master.m3u8"
