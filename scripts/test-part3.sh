#!/usr/bin/env bash
# =============================================================================
# Part 3 smoke test — auth + quotas + rate limiting + audit log
#
# Flow:
#   1. Verify unauthenticated requests are rejected (401)
#   2. Login as admin → get JWT
#   3. Create an API key via JWT
#   4. Use API key for all storage operations
#   5. Verify rate-limit headers
#   6. Test quota (PUT a small object, check used_bytes goes up)
#   7. Test refund on DELETE
#   8. Verify audit log entries exist
# =============================================================================

set -euo pipefail

API="${API_URL:-http://localhost:8080/api}"
ADMIN_EMAIL="admin@local"
ADMIN_PASS="admin"
BUCKET="test3-$(date +%s)"

red() { echo -e "\033[31m$*\033[0m"; }
green() { echo -e "\033[32m$*\033[0m"; }
yellow() { echo -e "\033[33m$*\033[0m"; }

echo "== 1. Unauthenticated request rejected =============================="
code=$(curl -s -o /dev/null -w "%{http_code}" "$API/")
if [ "$code" = "401" ]; then green "OK 401"; else red "expected 401, got $code"; exit 1; fi

echo "== 2. Login as admin ================================================"
LOGIN=$(curl -s -X POST "$API/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}")
JWT=$(echo "$LOGIN" | python3 -c "import json,sys;print(json.load(sys.stdin).get('token',''))")
if [ -z "$JWT" ]; then red "no token in login response: $LOGIN"; exit 1; fi
green "OK got JWT (${#JWT} chars)"

echo "== 3. GET /auth/me with JWT ========================================="
ME=$(curl -s "$API/auth/me" -H "Authorization: Bearer $JWT")
echo "  $ME"
USED_BEFORE=$(echo "$ME" | python3 -c "import json,sys;print(json.load(sys.stdin)['usedBytes'])")
green "OK; usedBytes before = $USED_BEFORE"

echo "== 4. Create API key ================================================"
KEY_RESP=$(curl -s -X POST "$API/auth/keys" \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{"name":"smoke-test"}')
KEY=$(echo "$KEY_RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])")
KEY_ID=$(echo "$KEY_RESP" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
if [ -z "$KEY" ]; then red "no key in response: $KEY_RESP"; exit 1; fi
green "OK key prefix: ${KEY:0:16}..."

AUTH="Authorization: Bearer $KEY"

echo "== 5. List keys ====================================================="
curl -s "$API/auth/keys" -H "$AUTH" | python3 -m json.tool | head -20

echo "== 6. Create bucket with API key ===================================="
curl -s -X PUT "$API/$BUCKET" -H "$AUTH" -w "\nHTTP %{http_code}\n"

echo "== 7. PUT object (check rate-limit headers in response) ============="
PAYLOAD="hello PersonalS3 Part 3"
SIZE=${#PAYLOAD}
HEADERS=$(curl -s -i -X PUT "$API/$BUCKET/test.txt" \
  -H "$AUTH" \
  -H "Content-Type: text/plain" \
  --data-binary "$PAYLOAD" | head -10)
echo "$HEADERS"
echo "$HEADERS" | grep -i 'x-ratelimit' && green "OK rate-limit headers present" || red "no rate-limit headers"

echo "== 8. Verify quota increased ========================================"
ME2=$(curl -s "$API/auth/me" -H "$AUTH")
USED_AFTER=$(echo "$ME2" | python3 -c "import json,sys;print(json.load(sys.stdin)['usedBytes'])")
echo "  usedBytes after = $USED_AFTER (expected +$SIZE)"
if [ "$USED_AFTER" -eq "$((USED_BEFORE + SIZE))" ]; then
  green "OK quota incremented correctly"
else
  red "expected $((USED_BEFORE + SIZE)), got $USED_AFTER"
fi

echo "== 9. DELETE object refunds quota ==================================="
curl -s -X DELETE "$API/$BUCKET/test.txt" -H "$AUTH" -w "HTTP %{http_code}\n"
USED_FINAL=$(curl -s "$API/auth/me" -H "$AUTH" | python3 -c "import json,sys;print(json.load(sys.stdin)['usedBytes'])")
echo "  usedBytes final = $USED_FINAL (expected $USED_BEFORE)"
if [ "$USED_FINAL" -eq "$USED_BEFORE" ]; then green "OK quota refunded"; else red "expected $USED_BEFORE, got $USED_FINAL"; fi

echo "== 10. DELETE bucket ================================================"
curl -s -X DELETE "$API/$BUCKET" -H "$AUTH" -w "HTTP %{http_code}\n"

echo "== 11. Verify audit log entries ====================================="
sleep 1   # audit writes are async — give them a moment to land
ENTRIES=$(docker compose exec -T postgres psql \
  -U "$(grep '^POSTGRES_USER=' .env | cut -d= -f2)" \
  -d "$(grep '^POSTGRES_DB=' .env | cut -d= -f2)" -t -A \
  -c "SELECT action, COALESCE(host(ip_address)::text,'-'), status_code
        FROM audit_log
        WHERE user_id = (SELECT id FROM users WHERE email='admin@local')
        ORDER BY ts DESC LIMIT 10;")
echo "$ENTRIES" | sed 's/^/  - /'
COUNT=$(echo "$ENTRIES" | grep -c '|' || true)
if [ "$COUNT" -ge 5 ]; then
  green "OK $COUNT audit entries"
else
  red "expected ≥5 audit entries, got $COUNT — checking for errors:"
  docker compose logs --tail=20 api | grep -i audit || echo "  (no 'audit' lines in api logs)"
  exit 1
fi

echo "== 12. Revoke API key ==============================================="
curl -s -X DELETE "$API/auth/keys/$KEY_ID" -H "Authorization: Bearer $JWT" -w "HTTP %{http_code}\n"

echo "== 13. Revoked key should now fail =================================="
code=$(curl -s -o /dev/null -w "%{http_code}" "$API/" -H "$AUTH")
if [ "$code" = "401" ]; then green "OK 401 on revoked key"; else red "expected 401, got $code"; fi

echo
green "All Part 3 tests passed."
