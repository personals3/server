#!/usr/bin/env bash
# =============================================================================
# Part 4 smoke test — multipart upload
#
# Flow:
#  1. Login → API key
#  2. Create bucket
#  3. Create 16 MiB random file, split into 3x5MiB + 1x1MiB parts
#  4. Initiate multipart upload  → uploadId
#  5. Upload each part           → record ETags
#  6. ListParts                  → verify all 4 present
#  7. Complete                   → verify ETag has "-4" suffix
#  8. Download assembled object  → md5sum must match original
#  9. Verify quota usage         → exactly 16 MiB
# 10. Abort flow on a fresh upload + verify quota refund
# 11. Cleanup
# =============================================================================

set -euo pipefail

API="${API_URL:-http://localhost:8080/api}"
BUCKET="mp4-$(date +%s)"
KEY="big/file.bin"

red()    { echo -e "\033[31m$*\033[0m"; }
green()  { echo -e "\033[32m$*\033[0m"; }

cleanup() {
  rm -f /tmp/s3-orig /tmp/s3-down /tmp/s3-part-*
}
trap cleanup EXIT

# ---------- 1. Login, get API key ------------------------------------------
echo "== 1. Login + create API key ==============================="
JWT=$(curl -s -X POST "$API/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@local","password":"admin"}' \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['token'])")
KEY_RESP=$(curl -s -X POST "$API/auth/keys" \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{"name":"part4-test"}')
APIKEY=$(echo "$KEY_RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])")
KEY_ID=$(echo "$KEY_RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
AUTH="Authorization: Bearer $APIKEY"
green "OK"

USED_BEFORE=$(curl -s "$API/auth/me" -H "$AUTH" \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['usedBytes'])")
echo "  usedBytes before = $USED_BEFORE"

# ---------- 2. Create bucket ------------------------------------------------
echo "== 2. Create bucket: $BUCKET ==============================="
curl -sf -X PUT "$API/$BUCKET" -H "$AUTH" >/dev/null
green "OK"

# ---------- 3. Make test data ----------------------------------------------
echo "== 3. Generate 16 MiB file + split into 3x5MiB + 1x1MiB ===="
dd if=/dev/urandom of=/tmp/s3-orig bs=1048576 count=16 status=none
ORIG_MD5=$(md5sum /tmp/s3-orig | cut -d' ' -f1)
ORIG_SIZE=$(stat -c %s /tmp/s3-orig)
echo "  size=$ORIG_SIZE md5=$ORIG_MD5"

# Split into 5MiB parts (last one ends up at 1MiB).
split -b 5242880 -d -a 1 /tmp/s3-orig /tmp/s3-part-
ls -l /tmp/s3-part-* | awk '{print "  " $NF " " $5 " bytes"}'

# ---------- 4. Initiate -----------------------------------------------------
echo "== 4. Initiate multipart upload ============================"
INIT=$(curl -sf -X POST "$API/$BUCKET/$KEY?uploads" \
  -H "$AUTH" -H "Content-Type: application/octet-stream")
UPLOAD_ID=$(echo "$INIT" | python3 -c "import json,sys;print(json.load(sys.stdin)['uploadId'])")
echo "  uploadId = $UPLOAD_ID"

# ---------- 5. Upload parts -------------------------------------------------
echo "== 5. Upload 4 parts ======================================="
declare -a ETAGS
for i in 0 1 2 3; do
  PART_NUM=$((i + 1))
  PART_FILE="/tmp/s3-part-$i"
  RESP_HEADERS=$(curl -sf -i -X PUT \
    "$API/$BUCKET/$KEY?partNumber=$PART_NUM&uploadId=$UPLOAD_ID" \
    -H "$AUTH" \
    --data-binary "@$PART_FILE" \
    | tr -d '\r' | head -10)
  ETAG=$(echo "$RESP_HEADERS" | grep -i '^etag:' | awk '{print $2}' | tr -d '"')
  ETAGS[$PART_NUM]=$ETAG
  echo "  part $PART_NUM → ETag $ETAG"
done

# ---------- 6. List parts ---------------------------------------------------
echo "== 6. List parts ==========================================="
LIST=$(curl -sf "$API/$BUCKET/$KEY?uploadId=$UPLOAD_ID" -H "$AUTH")
COUNT=$(echo "$LIST" | python3 -c "import json,sys;print(len(json.load(sys.stdin)['parts']))")
if [ "$COUNT" = "4" ]; then green "OK 4 parts listed"; else red "expected 4, got $COUNT"; exit 1; fi

# ---------- 7. Complete -----------------------------------------------------
echo "== 7. Complete upload ======================================"
COMPLETE_BODY=$(python3 -c "
import json
parts = [
  {'partNumber': 1, 'etag': '${ETAGS[1]}'},
  {'partNumber': 2, 'etag': '${ETAGS[2]}'},
  {'partNumber': 3, 'etag': '${ETAGS[3]}'},
  {'partNumber': 4, 'etag': '${ETAGS[4]}'},
]
print(json.dumps({'parts': parts}))")
COMPLETE=$(curl -sf -X POST "$API/$BUCKET/$KEY?uploadId=$UPLOAD_ID" \
  -H "$AUTH" -H "Content-Type: application/json" \
  -d "$COMPLETE_BODY")
echo "  $COMPLETE"
FINAL_ETAG=$(echo "$COMPLETE" | python3 -c "import json,sys;print(json.load(sys.stdin)['etag'])")
FINAL_SIZE=$(echo "$COMPLETE" | python3 -c "import json,sys;print(json.load(sys.stdin)['size'])")
if [[ "$FINAL_ETAG" == *-4 ]]; then green "OK ETag has -4 suffix: $FINAL_ETAG"; else red "ETag malformed: $FINAL_ETAG"; exit 1; fi
[ "$FINAL_SIZE" = "$ORIG_SIZE" ] && green "OK size matches" || { red "size mismatch: $FINAL_SIZE vs $ORIG_SIZE"; exit 1; }

# ---------- 8. Download + verify --------------------------------------------
echo "== 8. Download assembled object + MD5 ======================"
curl -sf "$API/$BUCKET/$KEY" -H "$AUTH" -o /tmp/s3-down
DOWN_MD5=$(md5sum /tmp/s3-down | cut -d' ' -f1)
[ "$DOWN_MD5" = "$ORIG_MD5" ] && green "OK MD5 matches: $DOWN_MD5" || { red "MD5 mismatch! orig=$ORIG_MD5 down=$DOWN_MD5"; exit 1; }

# ---------- 9. Quota verification ------------------------------------------
echo "== 9. Quota usage ========================================="
USED_AFTER=$(curl -s "$API/auth/me" -H "$AUTH" \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['usedBytes'])")
DELTA=$((USED_AFTER - USED_BEFORE))
echo "  before=$USED_BEFORE after=$USED_AFTER delta=$DELTA (expected $ORIG_SIZE)"
[ "$DELTA" = "$ORIG_SIZE" ] && green "OK quota tracks file size" || { red "delta mismatch"; exit 1; }

# ---------- 10. Abort flow --------------------------------------------------
echo "== 10. Abort flow on a fresh upload ========================"
INIT2=$(curl -sf -X POST "$API/$BUCKET/willbeaborted?uploads" -H "$AUTH" -H "Content-Type: text/plain")
UPLOAD2=$(echo "$INIT2" | python3 -c "import json,sys;print(json.load(sys.stdin)['uploadId'])")
echo "  uploadId = $UPLOAD2"
# Upload one 5MiB part (so total_size becomes non-zero)
curl -sf -X PUT "$API/$BUCKET/willbeaborted?partNumber=1&uploadId=$UPLOAD2" \
  -H "$AUTH" --data-binary "@/tmp/s3-part-0" -o /dev/null
USED_DURING=$(curl -s "$API/auth/me" -H "$AUTH" \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['usedBytes'])")
echo "  usedBytes during in-progress upload = $USED_DURING (expect $((USED_AFTER + 5242880)))"
[ "$USED_DURING" = "$((USED_AFTER + 5242880))" ] && green "OK in-progress reserves quota" || { red "wrong"; exit 1; }

# Abort and verify refund.
curl -sf -X DELETE "$API/$BUCKET/willbeaborted?uploadId=$UPLOAD2" -H "$AUTH" -o /dev/null
USED_AFTER_ABORT=$(curl -s "$API/auth/me" -H "$AUTH" \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['usedBytes'])")
[ "$USED_AFTER_ABORT" = "$USED_AFTER" ] && green "OK quota refunded on abort" || { red "expected $USED_AFTER, got $USED_AFTER_ABORT"; exit 1; }

# ---------- 11. Cleanup -----------------------------------------------------
echo "== 11. Cleanup ============================================="
curl -sf -X DELETE "$API/$BUCKET/$KEY" -H "$AUTH" -o /dev/null
curl -sf -X DELETE "$API/$BUCKET" -H "$AUTH" -o /dev/null
curl -sf -X DELETE "$API/auth/keys/$KEY_ID" -H "Authorization: Bearer $JWT" -o /dev/null
green "OK"

echo
green "All Part 4 tests passed."
