#!/usr/bin/env bash
# =============================================================================
# Quota math integration test
#
# Exercises the paths most likely to leak used_bytes:
#   1. Single PUT upload + delete  (smallest happy path)
#   2. Multipart upload + delete
#   3. Multipart upload aborted mid-way (the race fix from 2026-05-30)
#   4. Object soft-delete (to trash) + purge
#   5. Transcode pre-flight + publish settle (if VIDEO_FIXTURE set)
#
# After each step, asserts drift == 0 via the same SQL the reconcile
# script uses. If anything diverges, prints the offending user's row and
# bails non-zero.
#
# Usage:
#   ADMIN_EMAIL=admin@local ADMIN_PASSWORD=admin ./scripts/test-quota.sh
#   VIDEO_FIXTURE=/path/to/some.mp4 ./scripts/test-quota.sh   # also runs step 5
# =============================================================================

set -euo pipefail

HOST="${HOST:-http://localhost:8080}"
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@local}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-admin}"
COMPOSE_PG="docker compose exec -T postgres psql -U s3admin -d personals3 -tA"

red()   { printf '\033[31m%s\033[0m\n' "$*" >&2; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
step()  { printf '\033[1;36m=== %s\033[0m\n' "$*"; }

# ---- login + bucket setup -----------------------------------------------------
step "logging in as $ADMIN_EMAIL"
TOKEN=$(curl -fsS -X POST "$HOST/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | jq -r .token)
[ -n "$TOKEN" ] || { red "login failed"; exit 1; }

BUCKET="quota-test-$$"
step "creating bucket $BUCKET"
curl -fsS -X PUT "$HOST/api/$BUCKET" -H "Authorization: Bearer $TOKEN" >/dev/null

cleanup() {
  step "tearing down bucket $BUCKET"
  curl -fsS -X DELETE "$HOST/api/$BUCKET?force=true" \
    -H "Authorization: Bearer $TOKEN" >/dev/null || true
  rm -f /tmp/quota-test-*.bin
}
trap cleanup EXIT

# ---- assertion helper ---------------------------------------------------------
assert_drift_zero() {
  local label="$1"
  local drift
  drift=$($COMPOSE_PG -c "
    SELECT used_bytes
         - ((SELECT COALESCE(SUM(size_bytes + COALESCE(transcoded_bytes,0)
                                  + COALESCE(transcode_reserved_bytes,0)),0)
               FROM objects WHERE bucket_id IN
                 (SELECT id FROM buckets WHERE owner_id=u.id))
          + (SELECT COALESCE(SUM(ov.size_bytes),0)
               FROM object_versions ov JOIN objects o ON o.id=ov.object_id
              WHERE o.bucket_id IN
                 (SELECT id FROM buckets WHERE owner_id=u.id))) AS drift
      FROM users u WHERE email='$ADMIN_EMAIL'
  " | tr -d '[:space:]')
  if [ "$drift" = "0" ]; then
    green "  ✓ $label: drift = 0 bytes"
  else
    red   "  ✗ $label: drift = $drift bytes"
    $COMPOSE_PG -c "
      SELECT email, used_bytes,
             (SELECT COALESCE(SUM(size_bytes + COALESCE(transcoded_bytes,0)
                                   + COALESCE(transcode_reserved_bytes,0)),0)
                FROM objects WHERE bucket_id IN
                  (SELECT id FROM buckets WHERE owner_id=u.id)) AS sum_objects
        FROM users u WHERE email='$ADMIN_EMAIL';"
    exit 1
  fi
}

# ---- step 1: single PUT --------------------------------------------------------
step "step 1: single PUT (4 MiB) + DELETE?purge"
dd if=/dev/urandom of=/tmp/quota-test-small.bin bs=1M count=4 status=none
curl -fsS -X PUT "$HOST/api/$BUCKET/small.bin" \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @/tmp/quota-test-small.bin >/dev/null
assert_drift_zero "after PUT"

curl -fsS -X DELETE "$HOST/api/$BUCKET/small.bin?purge=true" \
  -H "Authorization: Bearer $TOKEN" >/dev/null
assert_drift_zero "after PUT delete"

# ---- step 2: multipart ---------------------------------------------------------
step "step 2: multipart upload (20 MiB) + DELETE?purge"
dd if=/dev/urandom of=/tmp/quota-test-mp.bin bs=1M count=20 status=none

UPLOAD_ID=$(curl -fsS -X POST "$HOST/api/$BUCKET/mp.bin?uploads" \
  -H "Authorization: Bearer $TOKEN" | jq -r .uploadId)

PARTS_JSON='['
for n in 1 2 3 4; do
  off=$(( (n-1) * 5 * 1024 * 1024 ))
  etag=$(dd if=/tmp/quota-test-mp.bin bs=$((5*1024*1024)) skip=$((n-1)) count=1 2>/dev/null |
    curl -fsS -X PUT --data-binary @- \
      -H "Authorization: Bearer $TOKEN" \
      -H "Content-Length: $((5*1024*1024))" \
      -D - "$HOST/api/$BUCKET/mp.bin?partNumber=$n&uploadId=$UPLOAD_ID" |
    grep -i '^ETag:' | sed -E 's/.*"([^"]+)".*/\1/' | tr -d '\r\n')
  PARTS_JSON+="{\"partNumber\":$n,\"etag\":\"$etag\"}"
  [ $n -lt 4 ] && PARTS_JSON+=","
done
PARTS_JSON+=']'

curl -fsS -X POST "$HOST/api/$BUCKET/mp.bin?uploadId=$UPLOAD_ID" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"parts\":$PARTS_JSON}" >/dev/null
assert_drift_zero "after multipart complete"

curl -fsS -X DELETE "$HOST/api/$BUCKET/mp.bin?purge=true" \
  -H "Authorization: Bearer $TOKEN" >/dev/null
assert_drift_zero "after multipart delete"

# ---- step 3: aborted multipart (the race fix) ---------------------------------
step "step 3: initiate multipart + upload 2 parts + ABORT (the leak fix)"
UPLOAD_ID=$(curl -fsS -X POST "$HOST/api/$BUCKET/aborted.bin?uploads" \
  -H "Authorization: Bearer $TOKEN" | jq -r .uploadId)
for n in 1 2; do
  dd if=/tmp/quota-test-mp.bin bs=$((5*1024*1024)) skip=$((n-1)) count=1 2>/dev/null |
    curl -fsS -X PUT --data-binary @- \
      -H "Authorization: Bearer $TOKEN" \
      -H "Content-Length: $((5*1024*1024))" \
      "$HOST/api/$BUCKET/aborted.bin?partNumber=$n&uploadId=$UPLOAD_ID" >/dev/null
done
curl -fsS -X DELETE "$HOST/api/$BUCKET/aborted.bin?uploadId=$UPLOAD_ID" \
  -H "Authorization: Bearer $TOKEN" >/dev/null
assert_drift_zero "after abort"

# ---- step 4: soft-delete + purge ----------------------------------------------
step "step 4: PUT, soft-delete to trash, purge from trash"
curl -fsS -X PUT "$HOST/api/$BUCKET/trash-me.bin" \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @/tmp/quota-test-small.bin >/dev/null 2>&1 || \
dd if=/dev/urandom of=/tmp/quota-test-small.bin bs=1M count=4 status=none && \
  curl -fsS -X PUT "$HOST/api/$BUCKET/trash-me.bin" \
    -H "Authorization: Bearer $TOKEN" \
    --data-binary @/tmp/quota-test-small.bin >/dev/null
curl -fsS -X DELETE "$HOST/api/$BUCKET/trash-me.bin" \
  -H "Authorization: Bearer $TOKEN" >/dev/null
assert_drift_zero "after soft-delete (still in trash, charged)"
curl -fsS -X DELETE "$HOST/api/trash/$BUCKET/trash-me.bin" \
  -H "Authorization: Bearer $TOKEN" >/dev/null
assert_drift_zero "after purge from trash"

# ---- step 5: transcode lifecycle (optional) -----------------------------------
if [ -n "${VIDEO_FIXTURE:-}" ] && [ -f "$VIDEO_FIXTURE" ]; then
  step "step 5: upload $VIDEO_FIXTURE, wait for transcode, delete"
  SIZE=$(stat -c%s "$VIDEO_FIXTURE")
  curl -fsS -X PUT "$HOST/api/$BUCKET/video.mp4" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: video/mp4" \
    -H "Content-Length: $SIZE" \
    --data-binary "@$VIDEO_FIXTURE" >/dev/null
  green "  waiting up to 120s for transcode to settle..."
  for i in $(seq 1 60); do
    sleep 2
    status=$(curl -fsS "$HOST/api/$BUCKET/video.mp4?info" \
      -H "Authorization: Bearer $TOKEN" | jq -r .transcodeStatus)
    case "$status" in
      done|failed|failed_quota|skipped_quota)
        green "  transcode reached terminal state: $status"
        break
        ;;
    esac
  done
  assert_drift_zero "after transcode settle ($status)"

  curl -fsS -X DELETE "$HOST/api/$BUCKET/video.mp4?purge=true" \
    -H "Authorization: Bearer $TOKEN" >/dev/null
  assert_drift_zero "after video purge"
fi

green ""
green "all assertions passed — quota math is sound"
